package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds items.requires_maintenance_on_return: when true, returning a unit of
// this (serialized) SKU automatically routes the instance into maintenance
// status instead of straight back to in_service. Per-SKU opt-in; defaults
// false so existing items are unaffected. Idempotent.

func init() {
	m.Register(itemsRequiresMaintenanceUp, itemsRequiresMaintenanceDown)
}

func itemsRequiresMaintenanceUp(app core.App) error {
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		return fmt.Errorf("find items: %w", err)
	}
	if items.Fields.GetByName("requires_maintenance_on_return") != nil {
		return nil
	}
	items.Fields.Add(&core.BoolField{Name: "requires_maintenance_on_return"})
	if err := app.Save(items); err != nil {
		return fmt.Errorf("add items.requires_maintenance_on_return: %w", err)
	}
	return nil
}

func itemsRequiresMaintenanceDown(app core.App) error {
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		return nil
	}
	if items.Fields.GetByName("requires_maintenance_on_return") != nil {
		items.Fields.RemoveByName("requires_maintenance_on_return")
		if err := app.Save(items); err != nil {
			return fmt.Errorf("drop items.requires_maintenance_on_return: %w", err)
		}
	}
	return nil
}
