package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds item_instances.ble_id: a BLE beacon id, the BLE analog of rfid_epc
// (docs/location-sightings-plan.md, L4). A BLE gateway is just another external
// sighting publisher; the only difference from RFID is resolution, which the
// scan resolver now does by ble_id too. Nullable + indexed; empty for units
// without a beacon. Instance-only (beacon ids never live on the SKU), same rule
// as rfid_epc. Idempotent.

func init() {
	m.Register(addInstanceBLEIDUp, addInstanceBLEIDDown)
}

func addInstanceBLEIDUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		return fmt.Errorf("find item_instances: %w", err)
	}
	if col.Fields.GetByName("ble_id") == nil {
		col.Fields.Add(&core.TextField{Name: "ble_id"})
	}
	if !hasIndex(col, "idx_item_instances_ble_id") {
		col.AddIndex("idx_item_instances_ble_id", false, "ble_id", "")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("add item_instances.ble_id: %w", err)
	}
	return nil
}

func addInstanceBLEIDDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		return nil
	}
	removeIndex(col, "idx_item_instances_ble_id")
	if col.Fields.GetByName("ble_id") != nil {
		col.Fields.RemoveByName("ble_id")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("drop item_instances.ble_id: %w", err)
	}
	return nil
}
