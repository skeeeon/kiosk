package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// stock_adjustments is an append-only audit log of changes to
// items.quantity_on_hand made through the admin UI. Each row records the
// signed delta, the resulting on-hand value (snapshot, so the row is
// self-documenting), a free-form reason, and the admin who did it.
//
// Writes happen only through the /api/kiosk/items/{id}/adjust endpoint so
// the item update and the audit insert are guaranteed atomic. Direct PB
// edits to items.quantity_on_hand are still allowed (admins are trusted);
// those just don't generate audit rows.

func init() {
	m.Register(addStockAdjustmentsUp, addStockAdjustmentsDown)
}

func addStockAdjustmentsUp(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("stock_adjustments"); err == nil {
		return nil
	}

	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		return fmt.Errorf("find items: %w", err)
	}
	admins, err := app.FindCollectionByNameOrId("admins")
	if err != nil {
		return fmt.Errorf("find admins: %w", err)
	}

	col := core.NewBaseCollection("stock_adjustments")
	col.Fields.Add(&core.RelationField{
		Name:         "item",
		CollectionId: items.Id,
		Required:     true,
		MaxSelect:    1,
	})
	col.Fields.Add(&core.NumberField{Name: "delta", Required: true, OnlyInt: true})
	col.Fields.Add(&core.NumberField{Name: "new_quantity", Required: true, OnlyInt: true})
	col.Fields.Add(&core.TextField{Name: "reason", Required: true})
	col.Fields.Add(&core.RelationField{
		Name:         "admin",
		CollectionId: admins.Id,
		Required:     true,
		MaxSelect:    1,
	})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})

	col.AddIndex("idx_stock_adjustments_item", false, "item", "")
	col.AddIndex("idx_stock_adjustments_created", false, "created", "")

	adminRuleStr := adminRule
	col.ListRule = &adminRuleStr
	col.ViewRule = &adminRuleStr
	// create/update/delete are nil — the endpoint is the only writer.

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save stock_adjustments: %w", err)
	}
	return nil
}

func addStockAdjustmentsDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("stock_adjustments")
	if err != nil {
		return nil
	}
	if err := app.Delete(col); err != nil {
		return fmt.Errorf("delete stock_adjustments: %w", err)
	}
	return nil
}
