package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds item_instances.enclosure_id: the access-controlled cabinet a serialized
// unit lives in — the partition key for enclosure_diff when a node hosts more
// than one cabinet (Phase 4 of docs/asset-tracker-plan.md). Nullable + indexed;
// empty means counter/crib stock or a single-cabinet kiosk (which doesn't
// partition, so existing rows need no backfill). Explicit admin-assigned
// membership — no auto-flow. Idempotent.

func init() {
	m.Register(addInstanceEnclosureIDUp, addInstanceEnclosureIDDown)
}

func addInstanceEnclosureIDUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		return fmt.Errorf("find item_instances: %w", err)
	}
	if col.Fields.GetByName("enclosure_id") == nil {
		col.Fields.Add(&core.TextField{Name: "enclosure_id"})
	}
	if !hasIndex(col, "idx_item_instances_enclosure_id") {
		col.AddIndex("idx_item_instances_enclosure_id", false, "enclosure_id", "")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("add item_instances.enclosure_id: %w", err)
	}
	return nil
}

func addInstanceEnclosureIDDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		return nil
	}
	removeIndex(col, "idx_item_instances_enclosure_id")
	if col.Fields.GetByName("enclosure_id") != nil {
		col.Fields.RemoveByName("enclosure_id")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("drop item_instances.enclosure_id: %w", err)
	}
	return nil
}
