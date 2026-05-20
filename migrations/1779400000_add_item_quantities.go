package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds stock-tracking fields to the items collection:
//
//   quantity_on_hand   For tools, this is fleet count (total owned, never
//                      mutated by checkout/return). For consumables, this is
//                      current stock and is decremented by the commit hook on
//                      every consume line. Allowed to go negative so the
//                      ledger remains the source of truth even if the worker
//                      grabs more than was recorded as on hand.
//   reorder_threshold  Low-stock alert level. Zero means "no alert".
//
// Both default to zero on existing rows. Idempotent.

func init() {
	m.Register(addItemQuantitiesUp, addItemQuantitiesDown)
}

func addItemQuantitiesUp(app core.App) error {
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		return fmt.Errorf("find items: %w", err)
	}

	changed := false
	if items.Fields.GetByName("quantity_on_hand") == nil {
		items.Fields.Add(&core.NumberField{Name: "quantity_on_hand", OnlyInt: true})
		changed = true
	}
	if items.Fields.GetByName("reorder_threshold") == nil {
		items.Fields.Add(&core.NumberField{Name: "reorder_threshold", OnlyInt: true})
		changed = true
	}
	if !changed {
		return nil
	}
	if err := app.Save(items); err != nil {
		return fmt.Errorf("save items: %w", err)
	}
	return nil
}

func addItemQuantitiesDown(app core.App) error {
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		return nil
	}
	changed := false
	for _, name := range []string{"quantity_on_hand", "reorder_threshold"} {
		if f := items.Fields.GetByName(name); f != nil {
			if _, ok := f.(*core.NumberField); ok {
				items.Fields.RemoveByName(name)
				changed = true
			}
		}
	}
	if changed {
		if err := app.Save(items); err != nil {
			return fmt.Errorf("save items: %w", err)
		}
	}
	return nil
}
