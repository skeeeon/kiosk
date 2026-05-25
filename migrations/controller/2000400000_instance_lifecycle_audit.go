package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Controller-only audit log of every item_instance lifecycle event that
// happens anywhere in the fleet (create / decommission / reactivate /
// delete). The kiosks each maintain their own `instance_audit` table
// (untouched by this migration); the controller now projects every
// `{prefix}.{kiosk_code}.instance.lifecycle` event it receives into this
// collection so an operator can answer "every retire / reactivate / delete,
// every kiosk, who did it, when" without hopping kiosks.
//
// Schema mirrors `inventory_audit` in structure (denormalized strings, no
// FKs into kiosk-local rows the controller doesn't have). source_audit_id
// is the kiosk-side instance_audit.id — unique-when-non-empty so JetStream
// redelivery is a no-op rather than a duplicate row.

func init() {
	m.Register(addInstanceLifecycleAuditUp, addInstanceLifecycleAuditDown)
}

const instanceLifecycleAuditCollection = "instance_lifecycle_audit"

func addInstanceLifecycleAuditUp(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(instanceLifecycleAuditCollection); err == nil {
		return nil
	}

	col := core.NewBaseCollection(instanceLifecycleAuditCollection)

	col.Fields.Add(&core.TextField{Name: "kiosk_code", Required: true})
	col.Fields.Add(&core.TextField{Name: "item_code"})
	col.Fields.Add(&core.TextField{Name: "item_name"})
	col.Fields.Add(&core.TextField{Name: "instance_id", Required: true})
	col.Fields.Add(&core.TextField{Name: "instance_code"})
	col.Fields.Add(&core.SelectField{
		Name:      "action",
		Values:    []string{"create", "decommission", "reactivate", "delete"},
		Required:  true,
		MaxSelect: 1,
	})
	col.Fields.Add(&core.BoolField{Name: "prev_active"})
	col.Fields.Add(&core.BoolField{Name: "new_active"})
	col.Fields.Add(&core.TextField{Name: "reason"})
	col.Fields.Add(&core.TextField{Name: "admin_id"})
	col.Fields.Add(&core.SelectField{
		Name:      "source",
		Values:    []string{"local", "controller"},
		MaxSelect: 1,
	})
	col.Fields.Add(&core.TextField{Name: "command_id"})
	col.Fields.Add(&core.TextField{Name: "source_audit_id"})
	col.Fields.Add(&core.DateField{Name: "occurred_at"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})

	// Unique-when-non-empty index on source_audit_id is the idempotency
	// anchor for JetStream redelivery. Same partial-unique pattern as
	// inventory_audit.source_adjustment_id.
	col.AddIndex("idx_instance_lifecycle_audit_source",
		true,
		"source_audit_id",
		"source_audit_id != ''")
	col.AddIndex("idx_instance_lifecycle_audit_kiosk_created",
		false,
		"kiosk_code, created",
		"")
	col.AddIndex("idx_instance_lifecycle_audit_instance",
		false,
		"instance_id",
		"")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule
	// Create + Update + Delete intentionally nil — only the aggregator
	// writes (via app.Save bypassing collection rules), and these rows
	// are append-only audit.

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save %s: %w", instanceLifecycleAuditCollection, err)
	}
	return nil
}

func addInstanceLifecycleAuditDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId(instanceLifecycleAuditCollection)
	if err != nil {
		return nil
	}
	if err := app.Delete(col); err != nil {
		return fmt.Errorf("delete %s: %w", instanceLifecycleAuditCollection, err)
	}
	return nil
}
