package migrations

import (
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Splits "one physical thing" away from "one SKU". Pre-this migration, three
// impact drivers with different serials were three items. Post-this migration,
// they're one item ("Impact Driver") with three rows in item_instances. The
// items collection keeps holding the SKU; instances hold the per-unit code,
// serial, and RFID.
//
// The move is data-preserving and one-way:
//
//   1. Create item_instances collection (admin-only).
//   2. Add item_instance FK to open_checkouts and transaction_lines (nullable
//      — non-serialized items leave it empty).
//   3. For every existing item with tracking_mode=serialized, create one
//      instance whose code/serial/rfid_epc/active come from the item, then
//      clear the now-redundant fields on the item. No auto-merge of look-
//      alike codes — admins consolidate manually.
//   4. Backfill open_checkouts.item_instance and transaction_lines.item_instance
//      by joining on item.id for any rows whose item is now serialized.
//
// Idempotent: re-running against a half-applied state skips collections and
// fields that already exist.

func init() {
	m.Register(addItemInstancesUp, addItemInstancesDown)
}

func addItemInstancesUp(app core.App) error {
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		return fmt.Errorf("find items: %w", err)
	}

	instances, err := createItemInstancesCollection(app, items)
	if err != nil {
		return err
	}

	if err := addItemInstanceFK(app, "open_checkouts", instances); err != nil {
		return err
	}
	if err := addItemInstanceFK(app, "transaction_lines", instances); err != nil {
		return err
	}

	return moveSerializedItemData(app, items, instances)
}

func createItemInstancesCollection(app core.App, items *core.Collection) (*core.Collection, error) {
	if existing, err := app.FindCollectionByNameOrId("item_instances"); err == nil {
		return existing, nil
	}

	col := core.NewBaseCollection("item_instances")
	col.Fields.Add(&core.RelationField{
		Name:          "item",
		CollectionId:  items.Id,
		Required:      true,
		MaxSelect:     1,
		CascadeDelete: true,
	})
	col.Fields.Add(&core.TextField{Name: "code", Required: true})
	col.Fields.Add(&core.TextField{Name: "serial"})
	col.Fields.Add(&core.TextField{Name: "rfid_epc"})
	col.Fields.Add(&core.BoolField{Name: "active"})
	col.Fields.Add(&core.TextField{Name: "notes"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	col.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})

	col.AddIndex("idx_item_instances_code", true, "code", "code != ''")
	col.AddIndex("idx_item_instances_rfid_epc", true, "rfid_epc", "rfid_epc != ''")
	col.AddIndex("idx_item_instances_item", false, "item", "")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule
	col.CreateRule = &rule
	col.UpdateRule = &rule
	col.DeleteRule = &rule

	if err := app.Save(col); err != nil {
		return nil, fmt.Errorf("save item_instances: %w", err)
	}
	return col, nil
}

func addItemInstanceFK(app core.App, collectionName string, instances *core.Collection) error {
	col, err := app.FindCollectionByNameOrId(collectionName)
	if err != nil {
		return fmt.Errorf("find %s: %w", collectionName, err)
	}
	if col.Fields.GetByName("item_instance") != nil {
		return nil
	}
	col.Fields.Add(&core.RelationField{
		Name:         "item_instance",
		CollectionId: instances.Id,
		MaxSelect:    1,
		// CascadeDelete intentionally false: the ledger (and the open table)
		// must survive an accidental instance delete. The CascadeDelete=true
		// on item_instances.item handles the legitimate cleanup path.
	})
	col.AddIndex("idx_"+collectionName+"_item_instance", false, "item_instance", "item_instance != ''")
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save %s: %w", collectionName, err)
	}
	return nil
}

func moveSerializedItemData(app core.App, items, instances *core.Collection) error {
	rows, err := app.FindRecordsByFilter("items", "tracking_mode = 'serialized'", "", 0, 0)
	if err != nil {
		return fmt.Errorf("load serialized items: %w", err)
	}

	for _, item := range rows {
		// Skip items that already have at least one instance — keeps re-runs
		// safe if someone applied this migration manually mid-stream.
		existing, err := app.FindRecordsByFilter(
			"item_instances",
			"item = {:item}",
			"", 1, 0,
			dbx.Params{"item": item.Id},
		)
		if err != nil {
			return fmt.Errorf("probe existing instances for %s: %w", item.GetString("code"), err)
		}
		if len(existing) > 0 {
			continue
		}

		inst := core.NewRecord(instances)
		inst.Set("item", item.Id)
		inst.Set("code", item.GetString("code"))
		inst.Set("serial", item.GetString("serial"))
		inst.Set("rfid_epc", item.GetString("rfid_epc"))
		inst.Set("active", item.GetBool("active"))
		if err := app.Save(inst); err != nil {
			return fmt.Errorf("save instance for %s: %w", item.GetString("code"), err)
		}

		// Clear instance-y fields on the item — instances are authoritative now.
		// The unique-when-not-empty indexes on items.serial / items.rfid_epc
		// remain, but only consumables and quantity-tracked tools populate them.
		if item.GetString("serial") != "" || item.GetString("rfid_epc") != "" {
			item.Set("serial", "")
			item.Set("rfid_epc", "")
			if err := app.Save(item); err != nil {
				return fmt.Errorf("clear instance fields on %s: %w", item.GetString("code"), err)
			}
		}

		// Backfill instance FK on existing open_checkouts and transaction_lines
		// for this item. The migration runs before any new writes happen, so
		// every prior row that referenced this serialized item now points at
		// the single new instance we just created.
		if err := backfillFK(app, "open_checkouts", item.Id, inst.Id); err != nil {
			return err
		}
		if err := backfillFK(app, "transaction_lines", item.Id, inst.Id); err != nil {
			return err
		}
	}

	return nil
}

func backfillFK(app core.App, collectionName, itemID, instanceID string) error {
	rows, err := app.FindRecordsByFilter(
		collectionName,
		"item = {:item} && (item_instance = '' || item_instance = null)",
		"", 0, 0,
		dbx.Params{"item": itemID},
	)
	if err != nil {
		return fmt.Errorf("find %s rows to backfill: %w", collectionName, err)
	}
	for _, r := range rows {
		r.Set("item_instance", instanceID)
		if err := app.Save(r); err != nil {
			return fmt.Errorf("save %s row %s: %w", collectionName, r.Id, err)
		}
	}
	return nil
}

func addItemInstancesDown(app core.App) error {
	// Best-effort reverse: drop the FKs on the linking tables (data on disk is
	// preserved because we don't move it back to items.serial / .rfid_epc),
	// then drop the collection. Tests / dev only.
	for _, name := range []string{"open_checkouts", "transaction_lines"} {
		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			continue
		}
		if f := col.Fields.GetByName("item_instance"); f != nil {
			col.Fields.RemoveByName("item_instance")
			if err := app.Save(col); err != nil {
				return fmt.Errorf("save %s on down: %w", name, err)
			}
		}
	}
	col, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		return nil
	}
	if err := app.Delete(col); err != nil {
		return fmt.Errorf("delete item_instances: %w", err)
	}
	return nil
}
