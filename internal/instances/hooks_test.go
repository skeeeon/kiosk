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
	inst.Set("status", "in_service")
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

// TestNormalizeEPC_LowercasedOnWrite is the H5 regression: rfid_epc must be
// stored lower-case (and trimmed) on every write so it matches the LLRP
// reader's lower-case hex output. Covers both create and update through the
// model-level hooks Register binds.
func TestNormalizeEPC_LowercasedOnWrite(t *testing.T) {
	app := setupApp(t)
	New().Register(app)

	items, _ := app.FindCollectionByNameOrId("items")
	item := core.NewRecord(items)
	item.Set("code", "DRILL-X")
	item.Set("name", "Drill X")
	item.Set("type", "tool")
	item.Set("tracking_mode", "serialized")
	item.Set("active", true)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}

	instances, _ := app.FindCollectionByNameOrId("item_instances")
	inst := core.NewRecord(instances)
	inst.Set("item", item.Id)
	inst.Set("code", "DRILL-X-1")
	inst.Set("status", "in_service")
	inst.Set("rfid_epc", "  E2806890ABCDEF12  ") // upper-case + surrounding space
	if err := app.Save(inst); err != nil {
		t.Fatalf("save instance: %v", err)
	}

	got, err := app.FindRecordById("item_instances", inst.Id)
	if err != nil {
		t.Fatalf("reload instance: %v", err)
	}
	if want := "e2806890abcdef12"; got.GetString("rfid_epc") != want {
		t.Errorf("rfid_epc on create: got %q, want %q", got.GetString("rfid_epc"), want)
	}

	// Update path normalizes too.
	got.Set("rfid_epc", "AABBCCDD")
	if err := app.Save(got); err != nil {
		t.Fatalf("update instance: %v", err)
	}
	reloaded, _ := app.FindRecordById("item_instances", inst.Id)
	if want := "aabbccdd"; reloaded.GetString("rfid_epc") != want {
		t.Errorf("rfid_epc on update: got %q, want %q", reloaded.GetString("rfid_epc"), want)
	}
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
		PrevStatus: "",
		NewStatus:  "in_service",
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
	if got.GetString("new_status") != "in_service" {
		t.Errorf("new_status: got %q, want in_service", got.GetString("new_status"))
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

func TestWriteAudit_Retire_CapturesStatusFlip(t *testing.T) {
	app := setupApp(t)
	_, inst := seedItemWithInstance(t, app)
	adminID := ensureAdmin(t, app, "audit-retire@test.local")

	pub := &capturedPub{}
	h := NewWith(pub)

	h.writeAudit(app, auditInput{
		Record:     inst,
		Action:     "retire",
		PrevStatus: "in_service",
		NewStatus:  "retired",
		Reason:     "broken handle",
		AdminID:    adminID,
	})

	rows, err := app.FindRecordsByFilter("instance_audit",
		"item_instance = {:i} && action = 'retire'",
		"", 0, 0, dbx.Params{"i": inst.Id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("retire row: len=%d err=%v", len(rows), err)
	}
	r := rows[0]
	if r.GetString("prev_status") != "in_service" || r.GetString("new_status") != "retired" {
		t.Errorf("status states: prev=%q new=%q (want in_service→retired)",
			r.GetString("prev_status"), r.GetString("new_status"))
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
