package sightings_test

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/sightings"

	// Register kiosk migrations via init() so the runner can apply them below.
	_ "github.com/skeeeon/kiosk/migrations"
)

// setupApp boots a fresh PB app with our migrations applied (same pattern as
// internal/commit's setupApp — migratecmd's Automigrate hooks OnServe, so tests
// apply migrations via the runner directly).
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

// seedInstance creates a serialized item + one in_service instance and returns
// the instance id.
func seedInstance(t *testing.T, app core.App) string {
	t.Helper()

	itemsCol, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	item := core.NewRecord(itemsCol)
	item.Set("code", "DRILL-01")
	item.Set("name", "Drill")
	item.Set("type", "tool")
	item.Set("tracking_mode", "serialized")
	item.Set("active", true)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}

	instCol, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		t.Fatalf("find item_instances: %v", err)
	}
	inst := core.NewRecord(instCol)
	inst.Set("item", item.Id)
	inst.Set("code", "DRILL-01-A")
	inst.Set("rfid_epc", "e280-aaa")
	inst.Set("status", "in_service")
	if err := app.Save(inst); err != nil {
		t.Fatalf("save instance: %v", err)
	}
	return inst.Id
}

func observedFields(t *testing.T, app core.App, instanceID string) (at, zone, gateway string) {
	t.Helper()
	rec, err := app.FindRecordById("item_instances", instanceID)
	if err != nil {
		t.Fatalf("reload instance: %v", err)
	}
	return rec.GetString("last_observed_at"), rec.GetString("last_observed_zone"), rec.GetString("last_observed_gateway")
}

func TestStampLastObserved_SetsAndAdvances(t *testing.T) {
	app := setupApp(t)
	id := seedInstance(t, app)

	t0 := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	if err := sightings.StampLastObserved(app, id, "Yard", "reader1", nil, nil, t0); err != nil {
		t.Fatalf("stamp t0: %v", err)
	}
	at, zone, gw := observedFields(t, app, id)
	if at == "" || zone != "Yard" || gw != "reader1" {
		t.Fatalf("after t0 want zone=Yard gateway=reader1 non-empty at; got at=%q zone=%q gw=%q", at, zone, gw)
	}

	// A newer sighting advances zone/gateway/time.
	t1 := t0.Add(1 * time.Hour)
	if err := sightings.StampLastObserved(app, id, "Building B", "reader2", nil, nil, t1); err != nil {
		t.Fatalf("stamp t1: %v", err)
	}
	_, zone, gw = observedFields(t, app, id)
	if zone != "Building B" || gw != "reader2" {
		t.Fatalf("after t1 want zone=Building B gateway=reader2; got zone=%q gw=%q", zone, gw)
	}
}

func TestStampLastObserved_MonotonicNoOpOnOlder(t *testing.T) {
	app := setupApp(t)
	id := seedInstance(t, app)

	newer := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	if err := sightings.StampLastObserved(app, id, "Yard", "reader1", nil, nil, newer); err != nil {
		t.Fatalf("stamp newer: %v", err)
	}

	// An older, out-of-order sighting must be ignored (monotonic).
	older := newer.Add(-2 * time.Hour)
	if err := sightings.StampLastObserved(app, id, "Stale Zone", "old-reader", nil, nil, older); err != nil {
		t.Fatalf("stamp older: %v", err)
	}
	_, zone, gw := observedFields(t, app, id)
	if zone != "Yard" || gw != "reader1" {
		t.Fatalf("older sighting should be a no-op; got zone=%q gw=%q", zone, gw)
	}
}

func TestStampLastObserved_StoresGPS(t *testing.T) {
	app := setupApp(t)
	id := seedInstance(t, app)

	lat, lon := 47.6062, -122.3321
	when := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	if err := sightings.StampLastObserved(app, id, "", "truck-7", &lat, &lon, when); err != nil {
		t.Fatalf("stamp gps: %v", err)
	}
	rec, err := app.FindRecordById("item_instances", id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := rec.GetFloat("last_observed_lat"); got != lat {
		t.Fatalf("lat: want %v got %v", lat, got)
	}
	if got := rec.GetFloat("last_observed_lon"); got != lon {
		t.Fatalf("lon: want %v got %v", lon, got)
	}
}

func TestStampLastObserved_EmptyInstanceNoError(t *testing.T) {
	app := setupApp(t)
	if err := sightings.StampLastObserved(app, "", "Yard", "r", nil, nil, time.Now()); err != nil {
		t.Fatalf("empty instance id should be a no-op, got %v", err)
	}
}
