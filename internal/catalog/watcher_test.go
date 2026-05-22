package catalog

import (
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	_ "github.com/skeeeon/kiosk/migrations"
)

// setupApp boots a fresh kiosk-shape PB (no controller migrations) — this
// mirrors what a managed kiosk's DB looks like when the watcher runs.
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

func newTestWatcher(app core.App) *Watcher {
	// js is unused by the projection methods we exercise here — the
	// watcher's Start path is what would touch JetStream, and that's
	// integration-tested out of process. kioskCode is arbitrary; it only
	// matters when Start runs the prefix-filtered Watch.
	return NewWatcher(app, nil, ItemsBucket, UsersBucket, GroupsBucket, "KTEST")
}

func TestWatcher_UpsertItem_InsertsAndUpdates(t *testing.T) {
	app := setupApp(t)
	w := newTestWatcher(app)

	insert := ItemPayload{
		Code: "WRENCH-10", Name: "10mm Wrench",
		Type: "tool", TrackingMode: "quantity",
		Category: "hand-tools", Active: true,
	}
	if err := w.upsertItem(insert); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	rec, err := app.FindFirstRecordByFilter("items",
		"code = {:c}", dbx.Params{"c": "WRENCH-10"})
	if err != nil {
		t.Fatalf("find after insert: %v", err)
	}
	if rec.GetString("name") != "10mm Wrench" {
		t.Errorf("name not set: got %q", rec.GetString("name"))
	}
	if rec.GetInt("quantity_on_hand") != 0 {
		t.Errorf("expected qty=0 on insert, got %d", rec.GetInt("quantity_on_hand"))
	}

	// Set a local quantity to make sure the next upsert preserves it.
	rec.Set("quantity_on_hand", 17)
	if err := app.Save(rec); err != nil {
		t.Fatalf("set local qty: %v", err)
	}

	update := ItemPayload{
		Code: "WRENCH-10", Name: "10mm Combination Wrench",
		Type: "tool", TrackingMode: "quantity",
		Category: "hand-tools", Active: true, Notes: "renamed",
	}
	if err := w.upsertItem(update); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	rows, err := app.FindRecordsByFilter("items",
		"code = {:c}", "", 10, 0, dbx.Params{"c": "WRENCH-10"})
	if err != nil {
		t.Fatalf("find after update: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].GetString("name") != "10mm Combination Wrench" {
		t.Errorf("name not updated: got %q", rows[0].GetString("name"))
	}
	if rows[0].GetInt("quantity_on_hand") != 17 {
		t.Errorf("kiosk-local qty clobbered: got %d, want 17", rows[0].GetInt("quantity_on_hand"))
	}
	if rows[0].GetString("notes") != "renamed" {
		t.Errorf("notes not updated: got %q", rows[0].GetString("notes"))
	}
}

func TestWatcher_SoftDeleteItem_SetsActiveFalse(t *testing.T) {
	app := setupApp(t)
	w := newTestWatcher(app)

	if err := w.upsertItem(ItemPayload{
		Code: "HAMMER-1", Name: "Hammer",
		Type: "tool", TrackingMode: "quantity", Active: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := w.softDelete("items", "HAMMER-1"); err != nil {
		t.Fatalf("softDelete: %v", err)
	}
	rec, err := app.FindFirstRecordByFilter("items",
		"code = {:c}", dbx.Params{"c": "HAMMER-1"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if rec.GetBool("active") {
		t.Errorf("expected active=false after soft delete")
	}

	// Idempotent: deleting an unknown code is a no-op (not an error).
	if err := w.softDelete("items", "DOES-NOT-EXIST"); err != nil {
		t.Errorf("softDelete of unknown code returned error: %v", err)
	}
}

func TestWatcher_UpsertUser_InsertsAndUpdates(t *testing.T) {
	app := setupApp(t)
	w := newTestWatcher(app)

	insert := UserPayload{
		Code: "BADGE-1", Name: "Alice",
		Email: "alice@example.com", Role: "worker", Active: true,
	}
	if err := w.upsertUser(insert); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	rec, err := app.FindFirstRecordByFilter("users",
		"code = {:c}", dbx.Params{"c": "BADGE-1"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if rec.GetString("name") != "Alice" {
		t.Errorf("name: got %q", rec.GetString("name"))
	}

	update := UserPayload{
		Code: "BADGE-1", Name: "Alice Smith",
		Email: "alice@example.com", Role: "foreman", Active: true,
	}
	if err := w.upsertUser(update); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	rows, err := app.FindRecordsByFilter("users",
		"code = {:c}", "", 10, 0, dbx.Params{"c": "BADGE-1"})
	if err != nil {
		t.Fatalf("find after update: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d users for one code", len(rows))
	}
	if rows[0].GetString("name") != "Alice Smith" {
		t.Errorf("name not updated: got %q", rows[0].GetString("name"))
	}
	if rows[0].GetString("role") != "foreman" {
		t.Errorf("role not updated: got %q", rows[0].GetString("role"))
	}
}

func TestStripPrefix(t *testing.T) {
	tests := []struct {
		key, prefix, want string
	}{
		{"KIOSK01.DR-IMPACT-042", "KIOSK01.", "DR-IMPACT-042"},
		{"KIOSK01.SCREW-3", "KIOSK02.", ""}, // wrong prefix
		{"KIOSK01.", "KIOSK01.", ""},        // prefix-only key
		{"", "KIOSK01.", ""},                // empty key
		{"KIOSK01.A.B", "KIOSK01.", "A.B"},  // remainder may include dots
	}
	for _, tt := range tests {
		got := stripPrefix(tt.key, tt.prefix)
		if got != tt.want {
			t.Errorf("stripPrefix(%q, %q) = %q, want %q", tt.key, tt.prefix, got, tt.want)
		}
	}
}

func TestWatcher_UpsertGroup_InsertsAndUpdates(t *testing.T) {
	app := setupApp(t)
	w := newTestWatcher(app)

	insert := GroupPayload{
		Code: "ACME", Name: "Acme Subcontracting",
		ContactEmail: "foreman@acme.example",
		Active:       true,
	}
	if err := w.upsertGroup(insert); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	rec, err := app.FindFirstRecordByFilter("groups",
		"code = {:c}", dbx.Params{"c": "ACME"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got := rec.GetString("name"); got != "Acme Subcontracting" {
		t.Errorf("name: got %q", got)
	}
	if got := rec.GetString("contact_email"); got != "foreman@acme.example" {
		t.Errorf("contact_email: got %q", got)
	}

	update := GroupPayload{
		Code: "ACME", Name: "Acme Subcontracting LLC",
		ContactEmail: "pm@acme.example",
		Active:       true,
	}
	if err := w.upsertGroup(update); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	rec, _ = app.FindFirstRecordByFilter("groups",
		"code = {:c}", dbx.Params{"c": "ACME"})
	if got := rec.GetString("name"); got != "Acme Subcontracting LLC" {
		t.Errorf("name not updated: got %q", got)
	}
}

func TestWatcher_UpsertUser_ResolvesGroupCodeToFK(t *testing.T) {
	app := setupApp(t)
	w := newTestWatcher(app)

	// Group lands first (the typical order).
	if err := w.upsertGroup(GroupPayload{
		Code: "ACME", Name: "Acme", Active: true,
	}); err != nil {
		t.Fatalf("upsertGroup: %v", err)
	}
	groupRec, _ := app.FindFirstRecordByFilter("groups",
		"code = {:c}", dbx.Params{"c": "ACME"})

	if err := w.upsertUser(UserPayload{
		Code: "BADGE-3", Name: "Carol",
		Email: "carol@example.com",
		Role:  "worker", GroupCode: "ACME", Active: true,
	}); err != nil {
		t.Fatalf("upsertUser: %v", err)
	}
	userRec, _ := app.FindFirstRecordByFilter("users",
		"code = {:c}", dbx.Params{"c": "BADGE-3"})
	if got := userRec.GetString("group"); got != groupRec.Id {
		t.Errorf("user.group FK: want %q, got %q", groupRec.Id, got)
	}
}

func TestWatcher_UpsertUser_HandlesGroupBeforeGroupArrives(t *testing.T) {
	app := setupApp(t)
	w := newTestWatcher(app)

	// User arrives before its group (the out-of-order case). The user's
	// group FK is left blank for now; a subsequent user-update will resolve
	// it once the group lands.
	if err := w.upsertUser(UserPayload{
		Code: "BADGE-4", Name: "Dave",
		Email: "dave@example.com",
		Role:  "worker", GroupCode: "BETA", Active: true,
	}); err != nil {
		t.Fatalf("upsertUser early: %v", err)
	}
	userRec, _ := app.FindFirstRecordByFilter("users",
		"code = {:c}", dbx.Params{"c": "BADGE-4"})
	if got := userRec.GetString("group"); got != "" {
		t.Errorf("user.group FK with missing group: want empty, got %q", got)
	}

	// Group lands. Next user-update resolves the FK.
	if err := w.upsertGroup(GroupPayload{Code: "BETA", Name: "Beta", Active: true}); err != nil {
		t.Fatalf("upsertGroup: %v", err)
	}
	if err := w.upsertUser(UserPayload{
		Code: "BADGE-4", Name: "Dave",
		Email: "dave@example.com",
		Role:  "worker", GroupCode: "BETA", Active: true,
	}); err != nil {
		t.Fatalf("upsertUser after group: %v", err)
	}
	userRec, _ = app.FindFirstRecordByFilter("users",
		"code = {:c}", dbx.Params{"c": "BADGE-4"})
	groupRec, _ := app.FindFirstRecordByFilter("groups",
		"code = {:c}", dbx.Params{"c": "BETA"})
	if got := userRec.GetString("group"); got != groupRec.Id {
		t.Errorf("user.group FK after retry: want %q, got %q", groupRec.Id, got)
	}
}

func TestWatcher_SoftDeleteUser_SetsActiveFalse(t *testing.T) {
	app := setupApp(t)
	w := newTestWatcher(app)

	if err := w.upsertUser(UserPayload{
		Code: "BADGE-2", Name: "Bob",
		Email: "bob@example.com", Role: "worker", Active: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := w.softDelete("users", "BADGE-2"); err != nil {
		t.Fatalf("softDelete: %v", err)
	}
	rec, err := app.FindFirstRecordByFilter("users",
		"code = {:c}", dbx.Params{"c": "BADGE-2"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if rec.GetBool("active") {
		t.Errorf("expected active=false after soft delete")
	}
}
