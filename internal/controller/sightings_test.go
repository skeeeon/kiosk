package controller

import (
	"context"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
)

// seedEPCIndex inserts an instance_epc_index row so ProjectSighting can resolve.
func seedEPCIndex(t *testing.T, app core.App, epc, instanceID, instanceCode, kioskCode string) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("instance_epc_index")
	if err != nil {
		t.Fatalf("find instance_epc_index: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("rfid_epc", epc)
	rec.Set("instance_id", instanceID)
	rec.Set("instance_code", instanceCode)
	rec.Set("kiosk_code", kioskCode)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save epc index: %v", err)
	}
}

func locationRow(t *testing.T, app core.App, kioskCode, instanceCode string) *core.Record {
	t.Helper()
	rec, err := app.FindFirstRecordByFilter("instance_location",
		"kiosk_code = {:k} && instance_code = {:c}",
		dbx.Params{"k": kioskCode, "c": instanceCode})
	if err != nil {
		return nil
	}
	return rec
}

func newIngest(app core.App) *SightingIngest {
	// nil js → no KV; ProjectSighting still upserts instance_location.
	return NewSightingIngest(context.Background(), app, nil)
}

func TestSightingIngest_ResolvesAndUpserts(t *testing.T) {
	app := setupApp(t)
	seedEPCIndex(t, app, "e280-aaa", "inst-1", "DRILL-01-A", "KIOSK-A")
	ing := newIngest(app)

	when := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	if err := ing.ProjectSighting(events.SightingPayload{
		TagID: "E280-AAA", GatewayID: "gw-yard", Zone: "Yard", ObservedAt: when,
	}); err != nil {
		t.Fatalf("ProjectSighting: %v", err)
	}
	rec := locationRow(t, app, "KIOSK-A", "DRILL-01-A")
	if rec == nil {
		t.Fatal("expected an instance_location row")
	}
	if rec.GetString("last_observed_zone") != "Yard" || rec.GetString("instance_id") != "inst-1" {
		t.Fatalf("row not populated: zone=%q instance_id=%q",
			rec.GetString("last_observed_zone"), rec.GetString("instance_id"))
	}
}

func TestSightingIngest_UnknownTagDropped(t *testing.T) {
	app := setupApp(t)
	ing := newIngest(app)

	if err := ing.ProjectSighting(events.SightingPayload{
		TagID: "no-index-entry", Zone: "Yard", ObservedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("ProjectSighting: %v", err)
	}
	if locationRow(t, app, "KIOSK-A", "DRILL-01-A") != nil {
		t.Fatal("unknown tag should not create a location row")
	}
}

func TestSightingIngest_DedupUnchangedZone(t *testing.T) {
	app := setupApp(t)
	seedEPCIndex(t, app, "e280-bbb", "inst-2", "DRILL-02-A", "KIOSK-A")
	ing := newIngest(app)

	t0 := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	_ = ing.ProjectSighting(events.SightingPayload{TagID: "e280-bbb", Zone: "Yard", ObservedAt: t0})
	first := locationRow(t, app, "KIOSK-A", "DRILL-02-A")
	firstUpdated := first.GetDateTime("updated").String()

	// Same zone, later time → dedup gate drops it before any write (no update).
	_ = ing.ProjectSighting(events.SightingPayload{TagID: "e280-bbb", Zone: "Yard", ObservedAt: t0.Add(time.Hour)})
	second := locationRow(t, app, "KIOSK-A", "DRILL-02-A")
	if second.GetDateTime("updated").String() != firstUpdated {
		t.Fatal("unchanged zone should be deduped (no second write)")
	}

	// A real move (new zone) writes through.
	_ = ing.ProjectSighting(events.SightingPayload{TagID: "e280-bbb", Zone: "Building B", ObservedAt: t0.Add(2 * time.Hour)})
	moved := locationRow(t, app, "KIOSK-A", "DRILL-02-A")
	if moved.GetString("last_observed_zone") != "Building B" {
		t.Fatalf("move should update zone; got %q", moved.GetString("last_observed_zone"))
	}
}

func TestSightingIngest_MonotonicOlderIgnored(t *testing.T) {
	app := setupApp(t)
	seedEPCIndex(t, app, "e280-ccc", "inst-3", "DRILL-03-A", "KIOSK-A")
	ing := newIngest(app)

	newer := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	_ = ing.ProjectSighting(events.SightingPayload{TagID: "e280-ccc", Zone: "Yard", ObservedAt: newer})
	// Older sighting in a different zone — distinct dedup key, so it isn't
	// deduped; the monotonic upsert must still reject it.
	_ = ing.ProjectSighting(events.SightingPayload{TagID: "e280-ccc", Zone: "Stale", ObservedAt: newer.Add(-2 * time.Hour)})
	rec := locationRow(t, app, "KIOSK-A", "DRILL-03-A")
	if rec.GetString("last_observed_zone") != "Yard" {
		t.Fatalf("older sighting should be ignored; got zone=%q", rec.GetString("last_observed_zone"))
	}
}

func TestProjectInstanceLifecycle_PopulatesEPCIndex(t *testing.T) {
	app := setupApp(t)
	agg := NewAggregator(app, nil, "")

	out := agg.ProjectInstanceLifecycle(EventPayload{
		KioskCode:     "KIOSK-A",
		InstanceID:    "inst-9",
		InstanceCode:  "DRILL-09-A",
		Action:        "create",
		NewStatus:     "in_service",
		SourceAuditID: "audit-1",
		RFIDEPC:       "E280-XYZ",
		CompletedAt:   time.Now().UTC(),
	})
	if out != projectAck {
		t.Fatalf("ProjectInstanceLifecycle: got %v want projectAck", out)
	}

	rec, err := app.FindFirstRecordByFilter("instance_epc_index",
		"rfid_epc = {:e}", dbx.Params{"e": "e280-xyz"}) // stored lower-cased
	if err != nil {
		t.Fatalf("epc index row not found: %v", err)
	}
	if rec.GetString("instance_id") != "inst-9" || rec.GetString("kiosk_code") != "KIOSK-A" {
		t.Fatalf("epc index not populated: instance_id=%q kiosk_code=%q",
			rec.GetString("instance_id"), rec.GetString("kiosk_code"))
	}
}
