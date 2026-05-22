package migrations

import (
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// Adds notification_templates — an admin-editable store of subject + body
// strings rendered via text/template at send time. v1 seeds one row for
// receipt.transaction; new built-in templates ship by appending to
// notifications.SeededEventTypes() and re-running migrations (the seeder
// is idempotent — existing rows aren't overwritten).
//
// Schema choices:
//   - event_type: unique key. Templates are keyed by event, not by id, so
//     code paths can address a template without a lookup table.
//   - subject + body: text/template source strings.
//   - enabled: per-event gate. Disabling stops sends without losing the
//     authored content.
//   - Create + delete rules are nil (forbidden via API). Rows are seeded
//     by this migration and only editable, not removable — matching the
//     transactions collection's append-only-via-hooks shape.

func init() {
	m.Register(addNotificationTemplatesUp, addNotificationTemplatesDown)
}

func addNotificationTemplatesUp(app core.App) error {
	col, err := createNotificationTemplatesCollection(app)
	if err != nil {
		return err
	}
	return seedNotificationTemplates(app, col)
}

func addNotificationTemplatesDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return nil // already absent
	}
	if err := app.Delete(col); err != nil {
		return fmt.Errorf("delete %s: %w", notifications.CollectionName, err)
	}
	return nil
}

func createNotificationTemplatesCollection(app core.App) (*core.Collection, error) {
	if existing, err := app.FindCollectionByNameOrId(notifications.CollectionName); err == nil {
		return existing, nil
	}

	col := core.NewBaseCollection(notifications.CollectionName)
	col.Fields.Add(&core.TextField{Name: "event_type", Required: true})
	col.Fields.Add(&core.TextField{Name: "name", Required: true})
	col.Fields.Add(&core.BoolField{Name: "enabled"})
	col.Fields.Add(&core.TextField{Name: "subject"})
	col.Fields.Add(&core.TextField{Name: "body"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	col.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})

	col.AddIndex("idx_notification_templates_event_type", true, "event_type", "")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule
	col.UpdateRule = &rule
	// CreateRule + DeleteRule intentionally nil — rows are seeded by this
	// migration and the API surface is read + update only.

	if err := app.Save(col); err != nil {
		return nil, fmt.Errorf("save %s: %w", notifications.CollectionName, err)
	}
	return col, nil
}

// seedNotificationTemplates inserts one row per built-in event type if it
// doesn't already exist. Re-runs after a partial application are safe.
func seedNotificationTemplates(app core.App, col *core.Collection) error {
	for _, et := range notifications.SeededEventTypes() {
		existing, _ := app.FindFirstRecordByFilter(
			notifications.CollectionName,
			"event_type = {:t}",
			dbx.Params{"t": et},
		)
		if existing != nil {
			continue
		}
		subject, body, ok := notifications.Defaults(et)
		if !ok {
			return fmt.Errorf("no defaults registered for event type %q", et)
		}
		rec := core.NewRecord(col)
		rec.Set("event_type", et)
		rec.Set("name", notifications.DefaultName(et))
		rec.Set("enabled", true)
		rec.Set("subject", subject)
		rec.Set("body", body)
		if err := app.Save(rec); err != nil {
			return fmt.Errorf("seed template %q: %w", et, err)
		}
	}
	return nil
}
