package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds the advisory last_observed_* columns to item_instances — the materialized
// "where was this unit last seen" view for the location/sightings feature (Phase
// L1 of docs/location-sightings-plan.md). Last-observed is ADVISORY, lossy,
// last-write-wins: it never gates or mutates custody. Populated by RFID custody
// reads at the reader's zone (L1), external gateway sightings (L2+), and the
// controller's KV mirror (L3). All nullable; empty until something reports —
// kiosk-local fields, never touched by catalog resync (the catalog watcher only
// writes `items`). Idempotent.
//
//   - last_observed_at      (date)   — timestamp of the latest sighting; the
//                                       monotonicity anchor for the stamp.
//   - last_observed_zone    (text)   — coarse location label (static gateway / reader zone).
//   - last_observed_gateway (text)   — which device observed it (reader_id / gateway_id).
//   - last_observed_lat/lon (number) — GPS, for roaming gateways; 0,0 for zone-only sightings
//                                       (PB number columns are NOT NULL DEFAULT 0).

func init() {
	m.Register(addInstanceLastObservedUp, addInstanceLastObservedDown)
}

func addInstanceLastObservedUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		return fmt.Errorf("find item_instances: %w", err)
	}
	if col.Fields.GetByName("last_observed_at") == nil {
		col.Fields.Add(&core.DateField{Name: "last_observed_at"})
	}
	if col.Fields.GetByName("last_observed_zone") == nil {
		col.Fields.Add(&core.TextField{Name: "last_observed_zone"})
	}
	if col.Fields.GetByName("last_observed_gateway") == nil {
		col.Fields.Add(&core.TextField{Name: "last_observed_gateway"})
	}
	if col.Fields.GetByName("last_observed_lat") == nil {
		col.Fields.Add(&core.NumberField{Name: "last_observed_lat"})
	}
	if col.Fields.GetByName("last_observed_lon") == nil {
		col.Fields.Add(&core.NumberField{Name: "last_observed_lon"})
	}
	// Index supports the L4 reconciliation "stale beyond a threshold" query.
	if !hasIndex(col, "idx_item_instances_last_observed_at") {
		col.AddIndex("idx_item_instances_last_observed_at", false, "last_observed_at", "")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("add item_instances.last_observed_*: %w", err)
	}
	return nil
}

func addInstanceLastObservedDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		return nil
	}
	removeIndex(col, "idx_item_instances_last_observed_at")
	for _, name := range []string{
		"last_observed_at",
		"last_observed_zone",
		"last_observed_gateway",
		"last_observed_lat",
		"last_observed_lon",
	} {
		if col.Fields.GetByName(name) != nil {
			col.Fields.RemoveByName(name)
		}
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("drop item_instances.last_observed_*: %w", err)
	}
	return nil
}
