package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Controller-only audit log of every stock adjustment that happens
// anywhere in the fleet. The kiosks each maintain their own
// `stock_adjustments` table (untouched by this migration); the controller
// now projects every `{prefix}.{kiosk_code}.inventory.adjust` event it
// receives into this collection so an operator can answer "every change
// to qty, every kiosk, who did it, when" without hopping kiosks.
//
// Schema mirrors the event payload fields the aggregator already decodes
// (see EventPayload in internal/controller/consumer.go). source_adjustment_id
// is the kiosk-side stock_adjustments.id — unique when non-empty so a
// JetStream redelivery is a no-op rather than a duplicate audit row.
//
// Controller-only, registered via RegisterControllerMigrations so the
// kiosk binary never sees this collection on its DB.

func init() {
	// Deferred to RegisterControllerMigrations (see 2000000000_controller_collections.go).
}

// RegisterInventoryAuditMigration is invoked from the controllerOnce body
// in 2000000000_controller_collections.go alongside the other controller
// migrations.
func RegisterInventoryAuditMigration() {
	m.Register(addInventoryAuditUp, addInventoryAuditDown)
}

const inventoryAuditCollection = "inventory_audit"

func addInventoryAuditUp(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(inventoryAuditCollection); err == nil {
		return nil
	}

	col := core.NewBaseCollection(inventoryAuditCollection)

	col.Fields.Add(&core.TextField{Name: "kiosk_code", Required: true})
	col.Fields.Add(&core.TextField{Name: "item_code", Required: true})
	col.Fields.Add(&core.TextField{Name: "item_name"})
	col.Fields.Add(&core.TextField{Name: "source_adjustment_id"})
	col.Fields.Add(&core.TextField{Name: "admin_id"})
	col.Fields.Add(&core.TextField{Name: "mode"})
	col.Fields.Add(&core.NumberField{Name: "delta", OnlyInt: true})
	col.Fields.Add(&core.NumberField{Name: "prev_quantity", OnlyInt: true})
	col.Fields.Add(&core.NumberField{Name: "new_quantity", OnlyInt: true})
	col.Fields.Add(&core.TextField{Name: "reason"})
	col.Fields.Add(&core.SelectField{
		Name:      "source",
		Values:    []string{"local", "controller"},
		MaxSelect: 1,
	})
	col.Fields.Add(&core.TextField{Name: "command_id"})
	col.Fields.Add(&core.DateField{Name: "occurred_at"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})

	// Unique-when-non-empty index on source_adjustment_id is the
	// idempotency anchor for JetStream redelivery. PB doesn't expose a
	// "partial unique" attribute on the field itself — we model it as a
	// raw index with a WHERE clause, the same pattern used for
	// source_kiosk_code+source_transaction_id on transactions and for
	// command_id on stock_adjustments.
	col.AddIndex("idx_inventory_audit_source",
		true,
		"source_adjustment_id",
		"source_adjustment_id != ''")
	col.AddIndex("idx_inventory_audit_kiosk_created",
		false,
		"kiosk_code, created",
		"")
	col.AddIndex("idx_inventory_audit_item_created",
		false,
		"item_code, created",
		"")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule
	// Create + Update + Delete intentionally nil — only the aggregator
	// writes (via app.Save bypassing collection rules), and these rows
	// are append-only audit. If a retention pass becomes necessary later,
	// add a cron that deletes via app.Delete the same way the notification
	// send log already does.

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save %s: %w", inventoryAuditCollection, err)
	}
	return nil
}

func addInventoryAuditDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId(inventoryAuditCollection)
	if err != nil {
		return nil
	}
	if err := app.Delete(col); err != nil {
		return fmt.Errorf("delete %s: %w", inventoryAuditCollection, err)
	}
	return nil
}
