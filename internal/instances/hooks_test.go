package instances

import (
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/kioskctx"

	// Register migrations via init() so the runner can apply them.
	_ "github.com/skeeeon/kiosk/migrations"
)

func setupApp(t *testing.T) *pocketbase.PocketBase {
	t.Helper()
	t.Setenv("KIOSK_QUIET_BOOTSTRAP", "1")

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  t.TempDir(),
		HideStartBanner: true,
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	runner := core.NewMigrationsRunner(app, core.AppMigrations)
	if _, err := runner.Up(); err != nil {
		t.Fatalf("migrations up: %v", err)
	}
	t.Cleanup(func() { _ = app.ResetBootstrapState() })
	return app
}

type capturedEvent struct {
	Subject string
	Payload any
}

type capturedPub struct {
	events []capturedEvent
}

func (c *capturedPub) Publish(subject string, payload any) {
	c.events = append(c.events, capturedEvent{subject, payload})
}

// seedItemWithInstance creates an items row + an item_instances row in the
// test DB so the hooks have something to audit. Returns the item id and
// the instance record.
func seedItemWithInstance(t *testing.T, app core.App) (itemID string, instance *core.Record) {
	t.Helper()
	kioskctx.Set(kioskctx.Identity{KioskCode: "TEST", LocationCode: "T"})

	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	item := core.NewRecord(items)
	item.Set("code", "DRILL-A")
	item.Set("name", "Drill A")
	item.Set("type", "tool")
	item.Set("tracking_mode", "serialized")
	item.Set("active", true)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}

	instances, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		t.Fatalf("find item_instances: %v", err)
	}
	inst := core.NewRecord(instances)
	inst.Set("item", item.Id)
	inst.Set("code", "DRILL-A-1")
	inst.Set("serial", "SN-1")
	inst.Set("active", true)
	inst.Set("notes", "new from PO-1234")
	if err := app.Save(inst); err != nil {
		t.Fatalf("save instance: %v", err)
	}
	return item.Id, inst
}

func ensureAdmin(t *testing.T, app core.App, email string) string {
	t.Helper()
	admins, err := app.FindCollectionByNameOrId("admins")
	if err != nil {
		t.Fatalf("find admins: %v", err)
	}
	rec := core.NewRecord(admins)
	rec.Set("email", email)
	rec.Set("name", "Test Admin")
	rec.Set("active", true)
	rec.SetPassword("test-password-1234")
	if err := app.Save(rec); err != nil {
		t.Fatalf("save admin: %v", err)
	}
	return rec.Id
}

func TestWriteAudit_Create_RowAndEvent(t *testing.T) {
	app := setupApp(t)
	_, inst := seedItemWithInstance(t, app)
	adminID := ensureAdmin(t, app, "audit-create@test.local")

	pub := &capturedPub{}
	h := NewWith(pub)

	h.writeAudit(app, auditInput{
		Record:     inst,
		Action:     "create",
		PrevActive: false,
		NewActive:  true,
		Reason:     "new from PO-1234",
		AdminID:    adminID,
	})

	rows, err := app.FindRecordsByFilter("instance_audit",
		"item_instance = {:i}", "", 0, 0, dbx.Params{"i": inst.Id})
	if err != nil {
		t.Fatalf("list instance_audit: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: want 1, got %d", len(rows))
	}
	got := rows[0]
	if got.GetString("action") != "create" {
		t.Errorf("action: got %q, want create", got.GetString("action"))
	}
	if got.GetBool("new_active") != true {
		t.Errorf("new_active: got false, want true")
	}
	if got.GetString("admin") != adminID {
		t.Errorf("admin: got %q, want %q", got.GetString("admin"), adminID)
	}
	if got.GetString("source") != "local" {
		t.Errorf("source: got %q, want local", got.GetString("source"))
	}

	if len(pub.events) != 1 {
		t.Fatalf("events: want 1, got %d", len(pub.events))
	}
	if want := events.InstanceLifecycleSubject("TEST"); pub.events[0].Subject != want {
		t.Errorf("subject: got %q, want %q", pub.events[0].Subject, want)
	}
	payload, _ := pub.events[0].Payload.(map[string]any)
	if payload["action"] != "create" {
		t.Errorf("payload.action: %v", payload["action"])
	}
	if payload["admin_id"] != adminID {
		t.Errorf("payload.admin_id: %v", payload["admin_id"])
	}
}

func TestWriteAudit_Decommission_CapturesActiveFlip(t *testing.T) {
	app := setupApp(t)
	_, inst := seedItemWithInstance(t, app)
	adminID := ensureAdmin(t, app, "audit-decommission@test.local")

	pub := &capturedPub{}
	h := NewWith(pub)

	h.writeAudit(app, auditInput{
		Record:     inst,
		Action:     "decommission",
		PrevActive: true,
		NewActive:  false,
		Reason:     "broken handle",
		AdminID:    adminID,
	})

	rows, err := app.FindRecordsByFilter("instance_audit",
		"item_instance = {:i} && action = 'decommission'",
		"", 0, 0, dbx.Params{"i": inst.Id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("decommission row: len=%d err=%v", len(rows), err)
	}
	r := rows[0]
	if r.GetBool("prev_active") != true || r.GetBool("new_active") != false {
		t.Errorf("active states: prev=%v new=%v (want true→false)",
			r.GetBool("prev_active"), r.GetBool("new_active"))
	}
}

func TestWriteAuditFromSnapshot_Delete_SurvivesGoneRecord(t *testing.T) {
	app := setupApp(t)
	itemID, inst := seedItemWithInstance(t, app)
	adminID := ensureAdmin(t, app, "audit-delete@test.local")

	pub := &capturedPub{}
	h := NewWith(pub)

	// Simulate what the OnRecordDelete hook captured before the cascade.
	snap := deleteSnapshot{
		id:         inst.Id,
		code:       "DRILL-A-1",
		itemID:     itemID,
		prevActive: true,
		reason:     "scrap",
		adminID:    adminID,
	}
	h.writeAuditFromSnapshot(app, snap)

	rows, err := app.FindRecordsByFilter("instance_audit",
		"item_instance = {:i} && action = 'delete'",
		"", 0, 0, dbx.Params{"i": inst.Id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("delete row: len=%d err=%v", len(rows), err)
	}
	r := rows[0]
	if r.GetString("item") != itemID {
		t.Errorf("item FK: got %q, want %q", r.GetString("item"), itemID)
	}
	if r.GetString("reason") != "scrap" {
		t.Errorf("reason: %q", r.GetString("reason"))
	}

	if len(pub.events) != 1 {
		t.Fatalf("events: want 1, got %d", len(pub.events))
	}
	payload, _ := pub.events[0].Payload.(map[string]any)
	if payload["action"] != "delete" {
		t.Errorf("payload.action: %v", payload["action"])
	}
	if payload["item_code"] != "DRILL-A" {
		t.Errorf("payload.item_code: %v (want DRILL-A — denormalised via lookup)", payload["item_code"])
	}
}

// TestAuditCollection_RulesAreAdminOnly is a quick smoke test that the
// instance_audit collection mirrors stock_adjustments' admin-only rules.
// Catches a future migration that accidentally drops the rules.
func TestAuditCollection_RulesAreAdminOnly(t *testing.T) {
	app := setupApp(t)
	col, err := app.FindCollectionByNameOrId("instance_audit")
	if err != nil {
		t.Fatalf("find instance_audit: %v", err)
	}
	if col.ListRule == nil || !strings.Contains(*col.ListRule, "admins") {
		t.Errorf("ListRule must scope to admins, got %v", col.ListRule)
	}
	if col.CreateRule != nil {
		t.Errorf("CreateRule must be nil (hooks-only writer), got %v", col.CreateRule)
	}
}
