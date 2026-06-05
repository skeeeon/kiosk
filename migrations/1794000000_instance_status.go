package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Replaces item_instances.active (bool) with status (enum:
// in_service | maintenance | retired). "in_service" is the old active=true;
// "retired" is the old active=false (decommissioned, and the destination for
// what used to be a hard delete); "maintenance" is new — a unit physically
// present but not checkout-eligible (e.g. returned for service). "Checked out
// / out" remains DERIVED from open_checkouts and is never stored here.
//
// One-way, data-preserving:
//   1. Add the status SelectField.
//   2. Raw-SQL backfill: active=1 → in_service, active=0 → retired. Raw UPDATE
//      (not per-record Save) so the recompute hook doesn't fire mid-migration.
//   3. Drop the now-redundant active field.
//
// Shared migration: the controller's item_instances table is always empty, so
// the backfill is a no-op there (same as 1793000000).
//
// Idempotent: early-return if status already exists.

func init() {
	m.Register(instanceStatusUp, instanceStatusDown)
}

func instanceStatusUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		return fmt.Errorf("find item_instances: %w", err)
	}
	if col.Fields.GetByName("status") != nil {
		return nil
	}

	col.Fields.Add(&core.SelectField{
		Name:      "status",
		Values:    []string{"in_service", "maintenance", "retired"},
		Required:  true,
		MaxSelect: 1,
	})
	if err := app.Save(col); err != nil {
		return fmt.Errorf("add item_instances.status: %w", err)
	}

	// Backfill from the legacy active bool before dropping it.
	if _, err := app.DB().NewQuery(
		"UPDATE item_instances SET status = CASE WHEN active = 1 THEN 'in_service' ELSE 'retired' END",
	).Execute(); err != nil {
		return fmt.Errorf("backfill item_instances.status: %w", err)
	}

	col, err = app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		return fmt.Errorf("reload item_instances: %w", err)
	}
	if col.Fields.GetByName("active") != nil {
		col.Fields.RemoveByName("active")
		if err := app.Save(col); err != nil {
			return fmt.Errorf("drop item_instances.active: %w", err)
		}
	}
	return nil
}

func instanceStatusDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		return nil
	}
	if col.Fields.GetByName("active") == nil {
		col.Fields.Add(&core.BoolField{Name: "active"})
		if err := app.Save(col); err != nil {
			return fmt.Errorf("re-add item_instances.active: %w", err)
		}
	}
	// maintenance collapses to active=true (still a live unit); retired → false.
	if _, err := app.DB().NewQuery(
		"UPDATE item_instances SET active = CASE WHEN status = 'retired' THEN 0 ELSE 1 END",
	).Execute(); err != nil {
		return fmt.Errorf("restore item_instances.active: %w", err)
	}
	col, err = app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		return nil
	}
	if col.Fields.GetByName("status") != nil {
		col.Fields.RemoveByName("status")
		if err := app.Save(col); err != nil {
			return fmt.Errorf("drop item_instances.status: %w", err)
		}
	}
	return nil
}
