package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Controller-only EPC → owning-unit index (docs/location-sightings-plan.md, L3).
// A raw sighting from an external/standalone gateway carries only a tag id; the
// controller must learn which node owns it. This index is populated by threading
// rfid_epc onto the instance.lifecycle event the controller already projects:
// every create / status transition upserts (rfid_epc → instance_id,
// instance_code, kiosk_code) here. The SightingIngest resolves against it.
//
// Bounded (one row per tagged unit), persistent across restart. Known limit: a
// bare cosmetic rfid_epc change emits no lifecycle event (cosmetic edits aren't
// audited), so a re-tagged unit's index entry refreshes only on its next
// lifecycle event — acceptable for an advisory feature; re-tagging is rare.

func init() {
	m.Register(addInstanceEPCIndexUp, addInstanceEPCIndexDown)
}

const instanceEPCIndexCollection = "instance_epc_index"

func addInstanceEPCIndexUp(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(instanceEPCIndexCollection); err == nil {
		return nil
	}

	col := core.NewBaseCollection(instanceEPCIndexCollection)

	col.Fields.Add(&core.TextField{Name: "rfid_epc", Required: true})
	col.Fields.Add(&core.TextField{Name: "instance_id"})
	col.Fields.Add(&core.TextField{Name: "instance_code"})
	col.Fields.Add(&core.TextField{Name: "kiosk_code", Required: true})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	col.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})

	// Unique-when-non-empty on the tag id — the ingest's resolution key, and the
	// upsert anchor (a re-projected lifecycle event finds and updates the row).
	col.AddIndex("idx_instance_epc_index_epc", true, "rfid_epc", "rfid_epc != ''")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save %s: %w", instanceEPCIndexCollection, err)
	}
	return nil
}

func addInstanceEPCIndexDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId(instanceEPCIndexCollection)
	if err != nil {
		return nil
	}
	if err := app.Delete(col); err != nil {
		return fmt.Errorf("delete %s: %w", instanceEPCIndexCollection, err)
	}
	return nil
}
