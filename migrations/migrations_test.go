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
		"stock_adjustments",
	} {
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Errorf("collection %q missing after migrate: %v", name, err)
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
