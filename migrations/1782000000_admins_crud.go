package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// Phase 2 prerequisites for "send to admins" everywhere downstream:
//
//   1. Relax the admins collection rules from self-only (the init migration's
//      conservative default) to adminRule for all five operations. Any admin
//      can now list, view, create, update, or delete admins from the SPA.
//      Deletion of an admin that has FK references (notably stock_adjustments.
//      admin) will still 409 at PB-level — soft retire via active=false is
//      the operational path. The hard-delete affordance is intentionally
//      omitted from the SPA but exposed at the API for the edge case of a
//      just-created typo'd admin with no FK references yet.
//
//   2. Add `updated_by` (FK → admins, optional) to notification_templates so
//      the SPA can show "last edited by <email>". Pre-existing rows stay
//      null until the next save stamps a value through UpdateNotificationTemplate.
//
// Both changes are additive and idempotent.

func init() {
	m.Register(addAdminsCrudUp, addAdminsCrudDown)
}

func addAdminsCrudUp(app core.App) error {
	if err := relaxAdminsRules(app); err != nil {
		return err
	}
	return addUpdatedByToNotificationTemplates(app)
}

func addAdminsCrudDown(app core.App) error {
	if err := restoreAdminsRules(app); err != nil {
		return err
	}
	return removeUpdatedByFromNotificationTemplates(app)
}

func relaxAdminsRules(app core.App) error {
	admins, err := app.FindCollectionByNameOrId("admins")
	if err != nil {
		return fmt.Errorf("find admins: %w", err)
	}
	rule := adminRule
	admins.ListRule = &rule
	admins.ViewRule = &rule
	admins.CreateRule = &rule
	admins.UpdateRule = &rule
	admins.DeleteRule = &rule
	if err := app.Save(admins); err != nil {
		return fmt.Errorf("save admins: %w", err)
	}
	return nil
}

func restoreAdminsRules(app core.App) error {
	admins, err := app.FindCollectionByNameOrId("admins")
	if err != nil {
		return nil // collection gone, nothing to revert
	}
	selfOnly := adminSelfRule
	admins.ListRule = &selfOnly
	admins.ViewRule = &selfOnly
	admins.UpdateRule = &selfOnly
	admins.CreateRule = nil
	admins.DeleteRule = nil
	if err := app.Save(admins); err != nil {
		return fmt.Errorf("save admins: %w", err)
	}
	return nil
}

func addUpdatedByToNotificationTemplates(app core.App) error {
	col, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return nil // notification_templates not yet migrated — phase 1 will land first
	}
	admins, err := app.FindCollectionByNameOrId("admins")
	if err != nil {
		return fmt.Errorf("find admins: %w", err)
	}
	if col.Fields.GetByName("updated_by") == nil {
		col.Fields.Add(&core.RelationField{
			Name:         "updated_by",
			CollectionId: admins.Id,
			MaxSelect:    1,
			// CascadeDelete false: deleting an admin should null the FK on
			// the template, not delete the template row.
		})
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save %s: %w", notifications.CollectionName, err)
	}
	return nil
}

func removeUpdatedByFromNotificationTemplates(app core.App) error {
	col, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return nil
	}
	if col.Fields.GetByName("updated_by") != nil {
		col.Fields.RemoveByName("updated_by")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save %s: %w", notifications.CollectionName, err)
	}
	return nil
}
