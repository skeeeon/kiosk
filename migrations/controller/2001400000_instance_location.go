package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Controller-only site-wide advisory location view: the latest sighting per
// serialized unit across the whole fleet (docs/location-sightings-plan.md, L3).
// The controller's SightingIngest subscribes plainly to the lossy `sighting`
// family, resolves each raw tag via instance_epc_index, and upserts one row
// here per (kiosk_code, instance_code) — monotonically on observed_at. This is
// the durable join target for the L4 reconciliation report (custody vs
// location); it is latest-only (one row per instance), never a sighting log.
//
// Advisory: nothing here gates custody. Mirrors the kiosk's
// item_instances.last_observed_* columns; the controller also broadcasts each
// unit's last-observed back down via the last_observed_state KV bucket so the
// owning node sees sightings made by other nodes' gateways.

func init() {
	m.Register(addInstanceLocationUp, addInstanceLocationDown)
}

const instanceLocationCollection = "instance_location"

func addInstanceLocationUp(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(instanceLocationCollection); err == nil {
		return nil
	}

	col := core.NewBaseCollection(instanceLocationCollection)

	col.Fields.Add(&core.TextField{Name: "kiosk_code", Required: true})
	col.Fields.Add(&core.TextField{Name: "instance_id"})
	col.Fields.Add(&core.TextField{Name: "instance_code", Required: true})
	col.Fields.Add(&core.TextField{Name: "item_code"})
	col.Fields.Add(&core.DateField{Name: "last_observed_at"})
	col.Fields.Add(&core.TextField{Name: "last_observed_zone"})
	col.Fields.Add(&core.TextField{Name: "last_observed_gateway"})
	col.Fields.Add(&core.NumberField{Name: "last_observed_lat"})
	col.Fields.Add(&core.NumberField{Name: "last_observed_lon"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	col.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})

	// One row per unit; the ingest upserts by this pair.
	col.AddIndex("idx_instance_location_unit", true, "kiosk_code, instance_code", "")
	// Supports the L4 staleness scan and the join by instance id.
	col.AddIndex("idx_instance_location_observed_at", false, "last_observed_at", "")
	col.AddIndex("idx_instance_location_instance_id", false, "instance_id", "")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule
	// Create/Update/Delete nil — only the SightingIngest writes (app.Save
	// bypasses collection rules).

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save %s: %w", instanceLocationCollection, err)
	}
	return nil
}

func addInstanceLocationDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId(instanceLocationCollection)
	if err != nil {
		return nil
	}
	if err := app.Delete(col); err != nil {
		return fmt.Errorf("delete %s: %w", instanceLocationCollection, err)
	}
	return nil
}
