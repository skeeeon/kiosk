package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Migrates instance_audit from the active-bool vocabulary to the status-enum
// vocabulary, matching the item_instances.status change (1794):
//
//	action: [create, decommission, reactivate, delete]
//	     →  [create, to_maintenance, return_to_service, retire, unretire]
//	prev_active / new_active (bool)  →  prev_status / new_status (enum)
//
// Existing audit rows are remapped in place so history stays coherent:
// decommission/delete → retire, reactivate → unretire; prev/new_status are
// derived from the old bools (true → in_service, false → retired).
//
// Idempotent: early-return if prev_status already exists.

func init() {
	m.Register(instanceAuditStatusUp, instanceAuditStatusDown)
}

var instanceAuditActions = []string{"create", "to_maintenance", "return_to_service", "retire", "unretire"}
var instanceAuditStatusValues = []string{"in_service", "maintenance", "retired"}

func instanceAuditStatusUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("instance_audit")
	if err != nil {
		return fmt.Errorf("find instance_audit: %w", err)
	}
	if col.Fields.GetByName("prev_status") != nil {
		return nil
	}

	col.Fields.Add(&core.SelectField{Name: "prev_status", Values: instanceAuditStatusValues, MaxSelect: 1})
	col.Fields.Add(&core.SelectField{Name: "new_status", Values: instanceAuditStatusValues, MaxSelect: 1})
	if err := app.Save(col); err != nil {
		return fmt.Errorf("add instance_audit status fields: %w", err)
	}

	for _, q := range []string{
		"UPDATE instance_audit SET prev_status = CASE WHEN prev_active = 1 THEN 'in_service' ELSE 'retired' END",
		"UPDATE instance_audit SET new_status = CASE WHEN new_active = 1 THEN 'in_service' ELSE 'retired' END",
		"UPDATE instance_audit SET action = 'retire' WHERE action IN ('decommission', 'delete')",
		"UPDATE instance_audit SET action = 'unretire' WHERE action = 'reactivate'",
	} {
		if _, err := app.DB().NewQuery(q).Execute(); err != nil {
			return fmt.Errorf("backfill instance_audit: %w", err)
		}
	}

	col, err = app.FindCollectionByNameOrId("instance_audit")
	if err != nil {
		return fmt.Errorf("reload instance_audit: %w", err)
	}
	col.Fields.RemoveByName("prev_active")
	col.Fields.RemoveByName("new_active")
	if f, ok := col.Fields.GetByName("action").(*core.SelectField); ok {
		f.Values = instanceAuditActions
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("finalize instance_audit schema: %w", err)
	}
	return nil
}

func instanceAuditStatusDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("instance_audit")
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
			return fmt.Errorf("restore instance_audit bools: %w", err)
		}
	}
	for _, q := range []string{
		"UPDATE instance_audit SET prev_active = CASE WHEN prev_status = 'retired' THEN 0 ELSE 1 END",
		"UPDATE instance_audit SET new_active = CASE WHEN new_status = 'retired' THEN 0 ELSE 1 END",
		"UPDATE instance_audit SET action = 'decommission' WHERE action IN ('retire', 'to_maintenance')",
		"UPDATE instance_audit SET action = 'reactivate' WHERE action IN ('return_to_service', 'unretire')",
	} {
		if _, err := app.DB().NewQuery(q).Execute(); err != nil {
			return fmt.Errorf("restore instance_audit action: %w", err)
		}
	}
	col, err = app.FindCollectionByNameOrId("instance_audit")
	if err != nil {
		return nil
	}
	col.Fields.RemoveByName("prev_status")
	col.Fields.RemoveByName("new_status")
	if err := app.Save(col); err != nil {
		return fmt.Errorf("drop instance_audit status fields: %w", err)
	}
	return nil
}
