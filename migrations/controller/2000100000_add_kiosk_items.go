package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Per-kiosk item membership for the controller.
//
// Adds:
//   - `kiosk_items` join collection: one row per (kiosk, item) pair. A row
//     exists iff that kiosk stocks that item. Cascade-deletes on either side
//     so removing a kiosk or an item also removes the membership rows.
//   - Opens `kiosks.CreateRule` so admins can pre-register a kiosk before it
//     phones home — required so we can assign items to a brand-new kiosk
//     before it has anything to check out. The aggregator's touchKiosk path
//     still works for kiosks that self-register first; it just becomes a
//     no-op when the row already exists.

func init() {
	m.Register(addKioskItemsUp, addKioskItemsDown)
}

func addKioskItemsUp(app core.App) error {
	kiosks, err := app.FindCollectionByNameOrId("kiosks")
	if err != nil {
		return fmt.Errorf("find kiosks: %w", err)
	}
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		return fmt.Errorf("find items: %w", err)
	}

	if err := openKiosksCreateRule(app, kiosks); err != nil {
		return err
	}
	return createKioskItemsCollection(app, kiosks, items)
}

func addKioskItemsDown(app core.App) error {
	if col, err := app.FindCollectionByNameOrId("kiosk_items"); err == nil {
		if err := app.Delete(col); err != nil {
			return fmt.Errorf("delete kiosk_items: %w", err)
		}
	}
	// Restore the original CreateRule = nil on kiosks. Best-effort.
	if k, err := app.FindCollectionByNameOrId("kiosks"); err == nil {
		k.CreateRule = nil
		if err := app.Save(k); err != nil {
			return fmt.Errorf("save kiosks (down): %w", err)
		}
	}
	return nil
}

func openKiosksCreateRule(app core.App, kiosks *core.Collection) error {
	// Idempotent: if already opened, leave it alone.
	if kiosks.CreateRule != nil && *kiosks.CreateRule == adminRule {
		return nil
	}
	rule := adminRule
	kiosks.CreateRule = &rule
	if err := app.Save(kiosks); err != nil {
		return fmt.Errorf("save kiosks (open CreateRule): %w", err)
	}
	return nil
}

func createKioskItemsCollection(app core.App, kiosks, items *core.Collection) error {
	if _, err := app.FindCollectionByNameOrId("kiosk_items"); err == nil {
		return nil
	}

	col := core.NewBaseCollection("kiosk_items")
	col.Fields.Add(&core.RelationField{
		Name:          "kiosk",
		CollectionId:  kiosks.Id,
		Required:      true,
		MaxSelect:     1,
		CascadeDelete: true,
	})
	col.Fields.Add(&core.RelationField{
		Name:          "item",
		CollectionId:  items.Id,
		Required:      true,
		MaxSelect:     1,
		CascadeDelete: true,
	})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	col.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})

	// Unique pair → "this item stocks at this kiosk" is true-or-false; we
	// never want two rows describing the same membership.
	col.AddIndex("idx_kiosk_items_pair", true, "kiosk, item", "")
	col.AddIndex("idx_kiosk_items_kiosk", false, "kiosk", "")
	col.AddIndex("idx_kiosk_items_item", false, "item", "")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule
	col.CreateRule = &rule
	col.UpdateRule = &rule
	col.DeleteRule = &rule

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save kiosk_items: %w", err)
	}
	return nil
}
