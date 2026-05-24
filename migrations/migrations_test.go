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

// TestControllerMigrationsApplyCleanly covers the controller-only path:
// RegisterControllerMigrations() is called explicitly (it's NOT an init
// side effect by design — see CLAUDE.md), then migrations run, then the
// controller-only collections must exist.
func TestControllerMigrationsApplyCleanly(t *testing.T) {
	t.Setenv("KIOSK_ROLE", "controller")
	RegisterControllerMigrations()

	app := newApp(t)
	runner := core.NewMigrationsRunner(app, core.AppMigrations)
	if _, err := runner.Up(); err != nil {
		t.Fatalf("controller migrations failed: %v", err)
	}

	for _, name := range []string{
		"kiosks", "kiosk_items",
		// kiosk's collections should also be present — the controller
		// uses the same base schema.
		"users", "items", "transactions",
	} {
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Errorf("collection %q missing after migrate: %v", name, err)
		}
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
