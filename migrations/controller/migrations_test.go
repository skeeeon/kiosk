package controllermigrations

import (
	"testing"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	// Side-effect import: the kiosk migrations register via init() and
	// the controller migrations build on top of them, so both sets must
	// be in the global AppMigrations list before the runner fires.
	_ "github.com/skeeeon/kiosk/migrations"
)

// TestControllerMigrationsApplyCleanly is the smoke test for the
// controller-only schema. Living in package controllermigrations means
// the controller-only migrations register via their own init() blocks
// when the test binary loads, with no explicit registration call needed.
func TestControllerMigrationsApplyCleanly(t *testing.T) {
	t.Setenv("KIOSK_QUIET_BOOTSTRAP", "1")
	t.Setenv("KIOSK_ROLE", "controller")

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  t.TempDir(),
		HideStartBanner: true,
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = app.ResetBootstrapState() })

	runner := core.NewMigrationsRunner(app, core.AppMigrations)
	if _, err := runner.Up(); err != nil {
		t.Fatalf("controller migrations failed: %v", err)
	}

	for _, name := range []string{
		// Controller-only collections.
		"kiosks", "kiosk_items", "inventory_audit",
		"instance_lifecycle_audit",
		// Kiosk's collections must also be present — the controller
		// uses the same base schema.
		"users", "items", "transactions",
	} {
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Errorf("collection %q missing after migrate: %v", name, err)
		}
	}

	// Controller-specific columns on open_checkouts (added by
	// 2000500000_open_checkouts_kiosk_code.go).
	open, err := app.FindCollectionByNameOrId("open_checkouts")
	if err != nil {
		t.Fatalf("find open_checkouts: %v", err)
	}
	for _, name := range []string{"kiosk_code", "source_item_instance_id"} {
		if open.Fields.GetByName(name) == nil {
			t.Errorf("open_checkouts.%s missing on controller", name)
		}
	}

	// last_transaction_at on kiosks (2000200000).
	kiosks, err := app.FindCollectionByNameOrId("kiosks")
	if err != nil {
		t.Fatalf("find kiosks: %v", err)
	}
	if kiosks.Fields.GetByName("last_transaction_at") == nil {
		t.Error("kiosks.last_transaction_at missing on controller")
	}
}
