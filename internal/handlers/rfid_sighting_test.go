package handlers_test

import (
	"context"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/rfid"
)

// reloadObserved returns an instance's stamped last-observed fields.
func reloadObserved(t *testing.T, app core.App, instanceID string) (at, zone, gateway string) {
	t.Helper()
	rec, err := app.FindRecordById("item_instances", instanceID)
	if err != nil {
		t.Fatalf("reload instance %s: %v", instanceID, err)
	}
	return rec.GetString("last_observed_at"), rec.GetString("last_observed_zone"), rec.GetString("last_observed_gateway")
}

// TestPerformRFIDScan_StampsLastObserved: a counter read with a configured zone
// stamps advisory last-observed for EVERY resolved EPC it saw — including the
// retired instance that never becomes a cart line ("seen here" ≠ "added").
func TestPerformRFIDScan_StampsLastObserved(t *testing.T) {
	app, s := seedRFIDScan(t)
	s.Handle.ID = "counter"
	s.Handle.Zone = "Yard"
	s.Reader.epcs = []rfid.EPC{rfid.EPC(s.ActiveEPC), rfid.EPC(s.InactiveEPC)}

	if _, err := s.H.PerformRFIDScan(context.Background(), s.CartID, s.Handle); err != nil {
		t.Fatalf("PerformRFIDScan: %v", err)
	}

	for _, id := range []string{s.ActiveInstanceID} {
		at, zone, gw := reloadObserved(t, app, id)
		if at == "" || zone != "Yard" || gw != "counter" {
			t.Errorf("active instance: want zone=Yard gateway=counter non-empty at; got at=%q zone=%q gw=%q", at, zone, gw)
		}
	}

	// The retired instance was observed but not carted — it must still be stamped.
	retired, err := app.FindFirstRecordByFilter("item_instances", "rfid_epc = {:e}",
		map[string]any{"e": s.InactiveEPC})
	if err != nil {
		t.Fatalf("find retired instance: %v", err)
	}
	if retired.GetString("last_observed_zone") != "Yard" {
		t.Errorf("retired instance should also be stamped (seen, not carted); got zone=%q",
			retired.GetString("last_observed_zone"))
	}
}

// TestPerformRFIDScan_NoZoneNoStamp: a reader with no zone configured stamps
// nothing — location is opt-in by topology (N=1 invisible).
func TestPerformRFIDScan_NoZoneNoStamp(t *testing.T) {
	app, s := seedRFIDScan(t)
	// Handle.Zone deliberately left empty.
	s.Reader.epcs = []rfid.EPC{rfid.EPC(s.ActiveEPC)}

	if _, err := s.H.PerformRFIDScan(context.Background(), s.CartID, s.Handle); err != nil {
		t.Fatalf("PerformRFIDScan: %v", err)
	}
	at, zone, _ := reloadObserved(t, app, s.ActiveInstanceID)
	if at != "" || zone != "" {
		t.Errorf("no zone configured should stamp nothing; got at=%q zone=%q", at, zone)
	}
}
