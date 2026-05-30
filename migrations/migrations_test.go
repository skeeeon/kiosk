package migrations

import (
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// newApp boots a fresh PB instance in a temp dir with quiet bootstrap.
// Each test gets its own DB — isolation matters more than speed for
// migration smoke tests.
func newApp(t *testing.T) *pocketbase.PocketBase {
	t.Helper()
	t.Setenv("KIOSK_QUIET_BOOTSTRAP", "1")
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  t.TempDir(),
		HideStartBanner: true,
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = app.ResetBootstrapState() })
	return app
}

// TestKioskMigrationsApplyCleanly is the primary smoke test: every
// init()-registered migration applies without error against an empty DB
// and the kiosk's expected collections + bootstrap admin land.
//
// Failure here means a migration regression got introduced and the rest
// of the test suite (which depends on these migrations as setup) would
// fail with a less clear signal. This test isolates the schema-only
// issue.
func TestKioskMigrationsApplyCleanly(t *testing.T) {
	app := newApp(t)

	runner := core.NewMigrationsRunner(app, core.AppMigrations)
	if _, err := runner.Up(); err != nil {
		t.Fatalf("kiosk migrations failed: %v", err)
	}

	// Verify the kiosk's expected collections exist.
	for _, name := range []string{
		"users", "admins", "items", "item_instances",
		"transactions", "transaction_lines", "open_checkouts",
		"stock_adjustments", "instance_audit",
	} {
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Errorf("collection %q missing after migrate: %v", name, err)
		}
	}

	// admin_close must be in transaction_lines.action enum, and the
	// closure fields must exist. Pins the 1789000000 migration so a
	// future regression on the action enum (or accidental field rename)
	// surfaces here rather than as a runtime panic in commit.AdminClose.
	lines, err := app.FindCollectionByNameOrId("transaction_lines")
	if err != nil {
		t.Fatalf("find transaction_lines: %v", err)
	}
	if f := lines.Fields.GetByName("action"); f != nil {
		if sel, ok := f.(*core.SelectField); ok {
			found := false
			for _, v := range sel.Values {
				if v == "admin_close" {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("transaction_lines.action missing admin_close; have %v", sel.Values)
			}
		}
	}
	for _, name := range []string{"closed_by_admin", "closure_reason"} {
		if lines.Fields.GetByName(name) == nil {
			t.Errorf("transaction_lines.%s missing", name)
		}
	}
	txs, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		t.Fatalf("find transactions: %v", err)
	}
	for _, name := range []string{"closed_by_admin", "command_id"} {
		if txs.Fields.GetByName(name) == nil {
			t.Errorf("transactions.%s missing", name)
		}
	}

	// Bootstrap admin record should be seeded — the init migration writes
	// admin@kiosk.local with a printed-once random password.
	bootstrap, err := app.FindFirstRecordByFilter("admins",
		"email = {:e}", dbx.Params{"e": "admin@kiosk.local"})
	if err != nil {
		t.Errorf("bootstrap admin not found: %v", err)
	}
	if bootstrap != nil && !bootstrap.GetBool("active") {
		t.Error("bootstrap admin should be active=true")
	}
}

// TestMigrationsAreIdempotent confirms that running the migration runner
// a second time on an already-migrated DB is a no-op — PB tracks
// _migrations and skips. Pinned because a future migration author might
// accidentally rely on first-run semantics.
func TestMigrationsAreIdempotent(t *testing.T) {
	app := newApp(t)
	runner := core.NewMigrationsRunner(app, core.AppMigrations)
	if _, err := runner.Up(); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// A second run must succeed without re-executing any migration body
	// (re-executing the init migration's bootstrap-admin seed would
	// fail on email-uniqueness).
	if _, err := runner.Up(); err != nil {
		t.Fatalf("second run (should be no-op): %v", err)
	}
}

// TestBackfillSerializedQty verifies the 1793000000 backfill sets a serialized
// item's quantity_on_hand to its active instance count, leaves quantity-tracked
// items alone, handles zero-instance serialized items, and is idempotent. We
// call backfillSerializedQtyUp directly (re-running it after the initial
// migration) so we can seed deliberately-drifted data first — the same
// pre-migration drift the backfill exists to reconcile. Instances are seeded
// without the recompute hook registered, so the stored quantity stays wrong
// until the backfill runs.
func TestBackfillSerializedQty(t *testing.T) {
	app := newApp(t)
	runner := core.NewMigrationsRunner(app, core.AppMigrations)
	if _, err := runner.Up(); err != nil {
		t.Fatalf("migrations up: %v", err)
	}

	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	insts, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		t.Fatalf("find item_instances: %v", err)
	}

	mkItem := func(code, mode string, qty int) string {
		rec := core.NewRecord(items)
		rec.Set("code", code)
		rec.Set("name", code)
		rec.Set("type", "tool")
		rec.Set("tracking_mode", mode)
		rec.Set("active", true)
		rec.Set("quantity_on_hand", qty)
		if err := app.Save(rec); err != nil {
			t.Fatalf("save item %s: %v", code, err)
		}
		return rec.Id
	}
	mkInst := func(itemID, code string, active bool) {
		rec := core.NewRecord(insts)
		rec.Set("item", itemID)
		rec.Set("code", code)
		rec.Set("active", active)
		if err := app.Save(rec); err != nil {
			t.Fatalf("save instance %s: %v", code, err)
		}
	}

	// Serialized with drifted qty: 2 active + 1 inactive → should become 2.
	serID := mkItem("SER-DRIFT", "serialized", 99)
	mkInst(serID, "SER-DRIFT-1", true)
	mkInst(serID, "SER-DRIFT-2", true)
	mkInst(serID, "SER-DRIFT-3", false)

	// Serialized with no instances → should become 0.
	serEmptyID := mkItem("SER-EMPTY", "serialized", 7)

	// Quantity-tracked item → must be left untouched.
	qtyID := mkItem("QTY-KEEP", "quantity", 50)

	if err := backfillSerializedQtyUp(app); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	reload := func(id string) int {
		rec, err := app.FindRecordById("items", id)
		if err != nil {
			t.Fatalf("reload %s: %v", id, err)
		}
		return rec.GetInt("quantity_on_hand")
	}
	if got := reload(serID); got != 2 {
		t.Errorf("serialized w/ 2 active: want qty 2, got %d", got)
	}
	if got := reload(serEmptyID); got != 0 {
		t.Errorf("serialized w/ 0 instances: want qty 0, got %d", got)
	}
	if got := reload(qtyID); got != 50 {
		t.Errorf("quantity item: want qty 50 (untouched), got %d", got)
	}

	// Idempotent: a second run yields the same values.
	if err := backfillSerializedQtyUp(app); err != nil {
		t.Fatalf("backfill re-run: %v", err)
	}
	if got := reload(serID); got != 2 {
		t.Errorf("after re-run: want qty 2, got %d", got)
	}
}
