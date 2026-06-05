package migrations

import (
	"encoding/json"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// Seeds the alert.maintenance notification template from the compiled-in
// default so a fresh install AND an existing database both get the row.
// SeededEventTypes() lists it too, but that path only fires on first install;
// this migration covers the upgrade. notification_dedupe already exists
// (created in 1784000000), so this migration does not recreate it — the
// maintenance alert reuses the same (event_type, ref, day) gate with
// ref = transaction_id.
//
// Additive.

func init() {
	m.Register(addMaintenanceAlertsUp, addMaintenanceAlertsDown)
}

func addMaintenanceAlertsUp(app core.App) error {
	existing, _ := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeMaintenanceEntered},
	)
	if existing != nil {
		return nil
	}
	col, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return fmt.Errorf("find %s: %w", notifications.CollectionName, err)
	}
	subject, body, ok := notifications.Defaults(notifications.EventTypeMaintenanceEntered)
	if !ok {
		return fmt.Errorf("no defaults for %s", notifications.EventTypeMaintenanceEntered)
	}
	recipients := notifications.DefaultRecipients(notifications.EventTypeMaintenanceEntered)
	recipientsJSON, err := json.Marshal(recipients)
	if err != nil {
		return fmt.Errorf("marshal recipients: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("event_type", notifications.EventTypeMaintenanceEntered)
	rec.Set("name", notifications.DefaultName(notifications.EventTypeMaintenanceEntered))
	rec.Set("enabled", true)
	rec.Set("subject", subject)
	rec.Set("body", body)
	rec.Set("recipients", string(recipientsJSON))
	if err := app.Save(rec); err != nil {
		return fmt.Errorf("seed maintenance template: %w", err)
	}
	return nil
}

func addMaintenanceAlertsDown(app core.App) error {
	if rec, err := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeMaintenanceEntered},
	); err == nil && rec != nil {
		if err := app.Delete(rec); err != nil {
			return fmt.Errorf("delete maintenance template row: %w", err)
		}
	}
	return nil
}
