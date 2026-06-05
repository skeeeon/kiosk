package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Migrates the controller's instance_lifecycle_audit projection from the
// active-bool vocabulary to the status-enum vocabulary, matching the kiosk
// side (migrations 1794 + 1796). See those for the full rationale.
//
//	action → [create, to_maintenance, return_to_service, retire, unretire]
//	prev_active / new_active (bool) → prev_status / new_status (enum)
//
// Existing rows remapped in place (decommission/delete → retire, reactivate →
// unretire; status derived from the old bools). Idempotent.

func init() {
	m.Register(instanceLifecycleAuditStatusUp, instanceLifecycleAuditStatusDown)
}

var lifecycleAuditActions = []string{"create", "to_maintenance", "return_to_service", "retire", "unretire"}
var lifecycleAuditStatusValues = []string{"in_service", "maintenance", "retired"}

func instanceLifecycleAuditStatusUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId(instanceLifecycleAuditCollection)
	if err != nil {
		return fmt.Errorf("find %s: %w", instanceLifecycleAuditCollection, err)
	}
	if col.Fields.GetByName("prev_status") != nil {
		return nil
	}
	col.Fields.Add(&core.SelectField{Name: "prev_status", Values: lifecycleAuditStatusValues, MaxSelect: 1})
	col.Fields.Add(&core.SelectField{Name: "new_status", Values: lifecycleAuditStatusValues, MaxSelect: 1})
	if err := app.Save(col); err != nil {
		return fmt.Errorf("add %s status fields: %w", instanceLifecycleAuditCollection, err)
	}
	for _, q := range []string{
		"UPDATE instance_lifecycle_audit SET prev_status = CASE WHEN prev_active = 1 THEN 'in_service' ELSE 'retired' END",
		"UPDATE instance_lifecycle_audit SET new_status = CASE WHEN new_active = 1 THEN 'in_service' ELSE 'retired' END",
		"UPDATE instance_lifecycle_audit SET action = 'retire' WHERE action IN ('decommission', 'delete')",
		"UPDATE instance_lifecycle_audit SET action = 'unretire' WHERE action = 'reactivate'",
	} {
		if _, err := app.DB().NewQuery(q).Execute(); err != nil {
			return fmt.Errorf("backfill %s: %w", instanceLifecycleAuditCollection, err)
		}
	}
	col, err = app.FindCollectionByNameOrId(instanceLifecycleAuditCollection)
	if err != nil {
		return fmt.Errorf("reload %s: %w", instanceLifecycleAuditCollection, err)
	}
	col.Fields.RemoveByName("prev_active")
	col.Fields.RemoveByName("new_active")
	if f, ok := col.Fields.GetByName("action").(*core.SelectField); ok {
		f.Values = lifecycleAuditActions
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("finalize %s schema: %w", instanceLifecycleAuditCollection, err)
	}
	return nil
}

func instanceLifecycleAuditStatusDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId(instanceLifecycleAuditCollection)
	if err != nil {
		return nil
	}
	if col.Fields.GetByName("prev_active") == nil {
		col.Fields.Add(&core.BoolField{Name: "prev_active"})
		col.Fields.Add(&core.BoolField{Name: "new_active"})
		if f, ok := col.Fields.GetByName("action").(*core.SelectField); ok {
			f.Values = []string{"create", "decommission", "reactivate", "delete"}
		}
		if err := app.Save(col); err != nil {
			return fmt.Errorf("restore %s bools: %w", instanceLifecycleAuditCollection, err)
		}
	}
	for _, q := range []string{
		"UPDATE instance_lifecycle_audit SET prev_active = CASE WHEN prev_status = 'retired' THEN 0 ELSE 1 END",
		"UPDATE instance_lifecycle_audit SET new_active = CASE WHEN new_status = 'retired' THEN 0 ELSE 1 END",
		"UPDATE instance_lifecycle_audit SET action = 'decommission' WHERE action IN ('retire', 'to_maintenance')",
		"UPDATE instance_lifecycle_audit SET action = 'reactivate' WHERE action IN ('return_to_service', 'unretire')",
	} {
		if _, err := app.DB().NewQuery(q).Execute(); err != nil {
			return fmt.Errorf("restore %s action: %w", instanceLifecycleAuditCollection, err)
		}
	}
	col, err = app.FindCollectionByNameOrId(instanceLifecycleAuditCollection)
	if err != nil {
		return nil
	}
	col.Fields.RemoveByName("prev_status")
	col.Fields.RemoveByName("new_status")
	if err := app.Save(col); err != nil {
		return fmt.Errorf("drop %s status fields: %w", instanceLifecycleAuditCollection, err)
	}
	return nil
}
