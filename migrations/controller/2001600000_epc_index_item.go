package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds item_code + item_name to instance_epc_index. Item identity belongs
// alongside the instance identity the index already resolves: it lets the
// fleet location report (docs/location-sightings-plan.md, L4) name *what* a
// seen unit is, not just its instance code. Fed from the same instance.lifecycle
// payload that already carries ItemCode/ItemName, so populating it is a two-line
// addition in the aggregator's upsertInstanceEPCIndex.
//
// Nullable, no backfill: per the index's own cold-start note, an existing row's
// item columns fill in on the unit's next lifecycle event — acceptable for an
// advisory view (the instance_code still identifies the unit meanwhile).
// Idempotent.

func init() {
	m.Register(addEPCIndexItemUp, addEPCIndexItemDown)
}

func addEPCIndexItemUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId(instanceEPCIndexCollection)
	if err != nil {
		return fmt.Errorf("find %s: %w", instanceEPCIndexCollection, err)
	}
	if col.Fields.GetByName("item_code") == nil {
		col.Fields.Add(&core.TextField{Name: "item_code"})
	}
	if col.Fields.GetByName("item_name") == nil {
		col.Fields.Add(&core.TextField{Name: "item_name"})
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("add %s item columns: %w", instanceEPCIndexCollection, err)
	}
	return nil
}

func addEPCIndexItemDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId(instanceEPCIndexCollection)
	if err != nil {
		return nil
	}
	for _, name := range []string{"item_code", "item_name"} {
		if col.Fields.GetByName(name) != nil {
			col.Fields.RemoveByName(name)
		}
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("drop %s item columns: %w", instanceEPCIndexCollection, err)
	}
	return nil
}
