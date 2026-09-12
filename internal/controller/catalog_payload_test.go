package controller

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

// TestItemPayloadFrom_CarriesEveryPolicyField guards the third hop.
//
// Getting a field onto the fleet takes three edits that have to agree:
// the wire type in internal/catalog, the record→payload build here, and
// the payload→record projection in the kiosk's watcher. The catalog
// package's own tests cover the first and third. This one covers the
// middle, which is otherwise unguarded — and a missed rec.GetBool here
// fails exactly as quietly as a missing struct field: the payload
// marshals, the KV entry lands, the kiosk projects a zero value, and the
// feature is simply off across the whole fleet.
//
// requires_maintenance_on_return is here because it was missed once, and
// nothing caught it until a demo estate needed the maintenance queue to
// populate itself.
func TestItemPayloadFrom_CarriesEveryPolicyField(t *testing.T) {
	app := setupApp(t)

	col, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("code", "TORQUE-D1")
	rec.Set("name", "Digital Torque Wrench")
	rec.Set("type", "tool")
	rec.Set("unit", "each")
	rec.Set("tracking_mode", "serialized")
	rec.Set("category", "power-tools")
	rec.Set("active", true)
	rec.Set("notes", "calibrated annually")
	rec.Set("requires_maintenance_on_return", true)
	// Kiosk-local state, deliberately NOT carried — set here so the
	// assertions below are proving exclusion, not absence.
	rec.Set("quantity_on_hand", 4)
	rec.Set("reorder_threshold", 2)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save item: %v", err)
	}

	got := itemPayloadFrom(rec)

	for _, c := range []struct {
		field string
		got   any
		want  any
	}{
		{"Code", got.Code, "TORQUE-D1"},
		{"Name", got.Name, "Digital Torque Wrench"},
		{"Type", got.Type, "tool"},
		{"Unit", got.Unit, "each"},
		{"TrackingMode", got.TrackingMode, "serialized"},
		{"Category", got.Category, "power-tools"},
		{"Active", got.Active, true},
		{"Notes", got.Notes, "calibrated annually"},
		{"RequiresMaintenanceOnReturn", got.RequiresMaintenanceOnReturn, true},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v — a managed kiosk would see the zero value", c.field, c.got, c.want)
		}
	}
}
