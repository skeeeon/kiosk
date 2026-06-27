package sightings

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	_ "github.com/skeeeon/kiosk/migrations"
)

// This is an INTERNAL test (package sightings) so it can reach the unexported
// applyLastObservedKV — the pure core of the mirror watcher's apply path.

func bootApp(t *testing.T) *pocketbase.PocketBase {
	t.Helper()
	t.Setenv("KIOSK_QUIET_BOOTSTRAP", "1")
	app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: t.TempDir(), HideStartBanner: true})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := core.NewMigrationsRunner(app, core.AppMigrations).Up(); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	t.Cleanup(func() { _ = app.ResetBootstrapState() })
	return app
}

func seedOne(t *testing.T, app core.App) string {
	t.Helper()
	items, _ := app.FindCollectionByNameOrId("items")
	item := core.NewRecord(items)
	item.Set("code", "X")
	item.Set("name", "X")
	item.Set("type", "tool")
	item.Set("tracking_mode", "serialized")
	item.Set("active", true)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}
	insts, _ := app.FindCollectionByNameOrId("item_instances")
	inst := core.NewRecord(insts)
	inst.Set("item", item.Id)
	inst.Set("code", "X-A")
	inst.Set("status", "in_service")
	if err := app.Save(inst); err != nil {
		t.Fatalf("save instance: %v", err)
	}
	return inst.Id
}

func TestApplyLastObservedKV_StampsLocalInstance(t *testing.T) {
	app := bootApp(t)
	id := seedOne(t, app)

	st := LastObservedState{Zone: "Building C", Gateway: "site-gw", ObservedAt: time.Date(2026, 6, 27, 7, 0, 0, 0, time.UTC)}
	val, _ := json.Marshal(st)
	if err := applyLastObservedKV(app, "KIOSK-A", "KIOSK-A."+id, val); err != nil {
		t.Fatalf("applyLastObservedKV: %v", err)
	}
	rec, _ := app.FindRecordById("item_instances", id)
	if rec.GetString("last_observed_zone") != "Building C" || rec.GetString("last_observed_gateway") != "site-gw" {
		t.Fatalf("not stamped: zone=%q gw=%q",
			rec.GetString("last_observed_zone"), rec.GetString("last_observed_gateway"))
	}
}

func TestApplyLastObservedKV_BadKeyRejected(t *testing.T) {
	app := bootApp(t)
	val, _ := json.Marshal(LastObservedState{Zone: "Z", ObservedAt: time.Now().UTC()})
	// Key without this node's prefix → rejected (never stamps another node's id).
	if err := applyLastObservedKV(app, "KIOSK-A", "OTHER.inst-x", val); err == nil {
		t.Fatal("expected error for a key outside this node's prefix")
	}
}
