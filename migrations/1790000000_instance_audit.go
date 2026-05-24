package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// instance_audit is the append-only audit log for item_instances lifecycle
// changes, mirroring the role stock_adjustments plays for quantity. One row
// per: create, decommission (active true→false), reactivate (active
// false→true), delete. Cosmetic edits (typo fixes in code/serial/rfid_epc/
// notes) are intentionally not audited — they don't change "what physical
// thing this row represents."
//
// Writes flow from the PB record hooks in internal/instances (registered on
// both binaries). Direct PB edits without an authenticated request context
// (system migrations, back-fills) write no audit row. Indexes mirror
// stock_adjustments so future ops queries paginate the same way.
//
// Shape parallels stock_adjustments with one structural change:
// item_instance.CascadeDelete is **false** — the audit must outlive a
// deletion of the very row it documents.

func init() {
	m.Register(addInstanceAuditUp, addInstanceAuditDown)
}

func addInstanceAuditUp(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("instance_audit"); err == nil {
		return nil
	}

	instances, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		return fmt.Errorf("find item_instances: %w", err)
	}
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		return fmt.Errorf("find items: %w", err)
	}
	admins, err := app.FindCollectionByNameOrId("admins")
	if err != nil {
		return fmt.Errorf("find admins: %w", err)
	}

	col := core.NewBaseCollection("instance_audit")
	col.Fields.Add(&core.RelationField{
		Name:         "item_instance",
		CollectionId: instances.Id,
		Required:     true,
		MaxSelect:    1,
		// CascadeDelete false: the audit row outlives the instance it
		// documents. The delete action itself produces an audit row before
		// the row vanishes, but earlier rows (create / decommission) must
		// survive too.
	})
	col.Fields.Add(&core.RelationField{
		Name:         "item",
		CollectionId: items.Id,
		Required:     true,
		MaxSelect:    1,
		// Denormalised: lets the audit log render "which SKU did this
		// belong to" after an item delete cascades through the instances.
	})
	col.Fields.Add(&core.SelectField{
		Name:      "action",
		Values:    []string{"create", "decommission", "reactivate", "delete"},
		Required:  true,
		MaxSelect: 1,
	})
	col.Fields.Add(&core.BoolField{Name: "prev_active"})
	col.Fields.Add(&core.BoolField{Name: "new_active"})
	col.Fields.Add(&core.TextField{Name: "reason"})
	col.Fields.Add(&core.RelationField{
		Name:         "admin",
		CollectionId: admins.Id,
		MaxSelect:    1,
	})
	col.Fields.Add(&core.TextField{Name: "controller_admin_id"})
	col.Fields.Add(&core.SelectField{
		Name:      "source",
		Values:    []string{"local", "controller"},
		Required:  true,
		MaxSelect: 1,
	})
	col.Fields.Add(&core.TextField{Name: "command_id"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})

	col.AddIndex("idx_instance_audit_item_instance", false, "item_instance", "")
	col.AddIndex("idx_instance_audit_item", false, "item", "")
	col.AddIndex("idx_instance_audit_created", false, "created", "")
	col.AddIndex("idx_instance_audit_command_id", true, "command_id", "command_id != ''")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule
	// create/update/delete are nil — the record hooks are the only writer.

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save instance_audit: %w", err)
	}
	return nil
}

func addInstanceAuditDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("instance_audit")
	if err != nil {
		return nil
	}
	if err := app.Delete(col); err != nil {
		return fmt.Errorf("delete instance_audit: %w", err)
	}
	return nil
}
