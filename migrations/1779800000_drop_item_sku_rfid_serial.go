package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Drops `rfid_epc` and `serial` from the items collection. Both fields are
// now exclusively per-unit concerns: serialized tools carry them on
// item_instances, and quantity-tracked tools / consumables can't meaningfully
// carry a single SKU-level EPC or serial (EPCs are unique-per-tag; a pool of
// fungible units has no single "the" serial).
//
// The add_item_instances migration already moved these off serialized rows;
// this migration finishes the job by removing them everywhere. Existing
// values on non-serialized rows are discarded — they were vestigial.
//
// Idempotent: skip fields/indexes that are already gone.

func init() {
	m.Register(dropItemSKURFIDSerialUp, dropItemSKURFIDSerialDown)
}

func dropItemSKURFIDSerialUp(app core.App) error {
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		return fmt.Errorf("find items: %w", err)
	}

	// Indexes reference field names, so remove them before the fields.
	items.RemoveIndex("idx_items_rfid_epc")
	items.RemoveIndex("idx_items_serial")

	if items.Fields.GetByName("rfid_epc") != nil {
		items.Fields.RemoveByName("rfid_epc")
	}
	if items.Fields.GetByName("serial") != nil {
		items.Fields.RemoveByName("serial")
	}

	if err := app.Save(items); err != nil {
		return fmt.Errorf("save items: %w", err)
	}
	return nil
}

func dropItemSKURFIDSerialDown(app core.App) error {
	// Best-effort restore for dev rollbacks. Re-adds the fields and unique
	// indexes; original data is gone.
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		return nil
	}
	if items.Fields.GetByName("rfid_epc") == nil {
		items.Fields.Add(&core.TextField{Name: "rfid_epc"})
	}
	if items.Fields.GetByName("serial") == nil {
		items.Fields.Add(&core.TextField{Name: "serial"})
	}
	items.AddIndex("idx_items_rfid_epc", true, "rfid_epc", "rfid_epc != ''")
	items.AddIndex("idx_items_serial", true, "serial", "serial != ''")
	return app.Save(items)
}
