package migrations

import (
	"encoding/json"
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// Phase 3 of the notifications roadmap:
//
//   1. Adds a recipients JSON column to notification_templates. The shape is
//      {worker_email: bool, all_admins: bool, extras: []string}. Existing
//      rows are stamped with their event-type's compiled-in default so the
//      v1 receipt's behavior (send to worker) survives the upgrade exactly.
//
//   2. Creates notification_send_log — one row per attempted recipient with
//      status (sent/failed/skipped). Indexes cover the SPA's filter-and-sort
//      access pattern (event_type + created) and the retention cron's
//      created-based purge.
//
// Both changes are additive. The notifier reads recipients-or-default at
// send time, so the migration can run before the code change without
// breaking sends.

func init() {
	m.Register(addNotificationRecipientsAndLogUp, addNotificationRecipientsAndLogDown)
}

func addNotificationRecipientsAndLogUp(app core.App) error {
	if err := addRecipientsToNotificationTemplates(app); err != nil {
		return err
	}
	return createNotificationSendLogCollection(app)
}

func addNotificationRecipientsAndLogDown(app core.App) error {
	if col, err := app.FindCollectionByNameOrId("notification_send_log"); err == nil {
		if err := app.Delete(col); err != nil {
			return fmt.Errorf("delete notification_send_log: %w", err)
		}
	}
	if col, err := app.FindCollectionByNameOrId(notifications.CollectionName); err == nil {
		if col.Fields.GetByName("recipients") != nil {
			col.Fields.RemoveByName("recipients")
		}
		if err := app.Save(col); err != nil {
			return fmt.Errorf("save %s: %w", notifications.CollectionName, err)
		}
	}
	return nil
}

func addRecipientsToNotificationTemplates(app core.App) error {
	col, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return nil
	}
	if col.Fields.GetByName("recipients") == nil {
		col.Fields.Add(&core.JSONField{Name: "recipients"})
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save %s: %w", notifications.CollectionName, err)
	}

	// Seed existing rows with the event-type-default recipients shape.
	// Tolerates rows for event types not registered in Defaults (future-proof
	// against partial migrations) by falling back to worker-email only.
	rows, err := app.FindRecordsByFilter(notifications.CollectionName, "", "", 0, 0)
	if err != nil {
		return fmt.Errorf("list templates: %w", err)
	}
	for _, r := range rows {
		if existing := r.GetString("recipients"); existing != "" && existing != "null" {
			continue
		}
		recipients := notifications.DefaultRecipients(r.GetString("event_type"))
		payload, err := json.Marshal(recipients)
		if err != nil {
			return fmt.Errorf("marshal recipients: %w", err)
		}
		r.Set("recipients", string(payload))
		if err := app.Save(r); err != nil {
			return fmt.Errorf("save template %s: %w", r.GetString("event_type"), err)
		}
	}
	return nil
}

func createNotificationSendLogCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("notification_send_log"); err == nil {
		return nil
	}

	templates, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return fmt.Errorf("find %s: %w", notifications.CollectionName, err)
	}

	col := core.NewBaseCollection("notification_send_log")
	col.Fields.Add(&core.TextField{Name: "event_type", Required: true})
	col.Fields.Add(&core.RelationField{
		Name:         "template",
		CollectionId: templates.Id,
		MaxSelect:    1,
		// CascadeDelete false: log rows survive template deletes for audit.
	})
	col.Fields.Add(&core.TextField{Name: "recipient"})
	col.Fields.Add(&core.SelectField{
		Name:      "status",
		Values:    []string{"sent", "failed", "skipped"},
		Required:  true,
		MaxSelect: 1,
	})
	col.Fields.Add(&core.TextField{Name: "error"})
	col.Fields.Add(&core.TextField{Name: "payload_summary"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})

	col.AddIndex("idx_send_log_event_created", false, "event_type, created", "")
	col.AddIndex("idx_send_log_status", false, "status", "")
	col.AddIndex("idx_send_log_created", false, "created", "")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule
	// CreateRule + UpdateRule + DeleteRule intentionally nil — only the
	// notifier writes (via app.Save on a hydrated record) and the retention
	// cron deletes (via app.Delete bypassing collection rules).

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save notification_send_log: %w", err)
	}
	return nil
}
