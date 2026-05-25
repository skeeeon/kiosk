// Adds two controller-specific columns to open_checkouts:
//
//  1. `kiosk_code` — so a single table can hold rows from every kiosk in
//     the fleet. Kiosks themselves know their code implicitly (one DB per
//     kiosk); the controller is one DB for the fleet, so it has to
//     disambiguate. Indexed for per-kiosk reads, not unique.
//
//  2. `source_item_instance_id` — text mirror of the kiosk-local
//     item_instances.id, used for serialized-return matching during
//     projection. The existing item_instance field (added by the shared
//     migration 1779500000_add_item_instances.go) is a RelationField
//     pointing at the controller's own item_instances collection, which
//     stays empty on the controller — instances are kiosk-local state.
//     Trying to set a kiosk's instance id on that RelationField fails the
//     FK constraint, so we keep a parallel text column for cross-binary
//     matching and leave the relation field unused on the controller.
//
// The base open_checkouts collection itself (the FK to transaction_lines,
// the existing item_instance relation, etc.) is created by the shared
// init migration 1779000000_init.go and extended by 1779500000 — both
// run unconditionally via init(), so the controller already inherits the
// structure.
package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(openCheckoutsKioskCodeUp, openCheckoutsKioskCodeDown)
}

func openCheckoutsKioskCodeUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("open_checkouts")
	if err != nil {
		// Shared init hasn't created it yet — migrations apply in
		// timestamp order, so this shouldn't happen; treat as a no-op
		// rather than fail loudly.
		return nil
	}
	if col.Fields.GetByName("kiosk_code") == nil {
		col.Fields.Add(&core.TextField{Name: "kiosk_code"})
	}
	if col.Fields.GetByName("source_item_instance_id") == nil {
		col.Fields.Add(&core.TextField{Name: "source_item_instance_id"})
	}
	if !hasIndex(col, "idx_open_checkouts_kiosk_code") {
		col.AddIndex("idx_open_checkouts_kiosk_code", false, "kiosk_code", "")
	}
	if !hasIndex(col, "idx_open_checkouts_source_instance") {
		col.AddIndex("idx_open_checkouts_source_instance", false,
			"source_item_instance_id",
			"source_item_instance_id != ''")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save open_checkouts (controller cols): %w", err)
	}
	return nil
}

func openCheckoutsKioskCodeDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("open_checkouts")
	if err != nil {
		return nil
	}
	for _, name := range []string{"kiosk_code", "source_item_instance_id"} {
		if col.Fields.GetByName(name) != nil {
			col.Fields.RemoveByName(name)
		}
	}
	removeIndex(col, "idx_open_checkouts_kiosk_code")
	removeIndex(col, "idx_open_checkouts_source_instance")
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save open_checkouts (drop controller cols): %w", err)
	}
	return nil
}
