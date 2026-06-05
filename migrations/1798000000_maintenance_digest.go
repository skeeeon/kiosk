package migrations

import (
	"encoding/json"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// Extends the scheduled_reports collection to host the maintenance digest:
// appends "maintenance" to the report_key select values and seeds its
// notification_templates row from the compiled-in defaults.
//
// Sibling of 1791000000_daily_activity_digest.go — same shape, same
// idempotency guards, reusing extendScheduledReportsKey /
// removeScheduledReportsKey defined there. The report_key ("maintenance")
// names the scheduler runner; the seeded template's event_type is
// digest.maintenance (notifications.EventTypeMaintenanceDigest), mirroring
// the daily_activity → digest.daily_activity split.

func init() {
	m.Register(addMaintenanceDigestUp, addMaintenanceDigestDown)
}

func addMaintenanceDigestUp(app core.App) error {
	if err := extendScheduledReportsKey(app, "maintenance"); err != nil {
		return err
	}
	return seedMaintenanceDigestTemplate(app)
}

func addMaintenanceDigestDown(app core.App) error {
	if rec, err := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeMaintenanceDigest},
	); err == nil && rec != nil {
		if err := app.Delete(rec); err != nil {
			return fmt.Errorf("delete maintenance-digest template row: %w", err)
		}
	}
	if err := removeScheduledReportsKey(app, "maintenance"); err != nil {
		return err
	}
	return nil
}

func seedMaintenanceDigestTemplate(app core.App) error {
	existing, _ := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeMaintenanceDigest},
	)
	if existing != nil {
		return nil
	}
	col, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return fmt.Errorf("find %s: %w", notifications.CollectionName, err)
	}
	subject, body, ok := notifications.Defaults(notifications.EventTypeMaintenanceDigest)
	if !ok {
		return fmt.Errorf("no defaults for %s", notifications.EventTypeMaintenanceDigest)
	}
	recipients := notifications.DefaultRecipients(notifications.EventTypeMaintenanceDigest)
	recipientsJSON, err := json.Marshal(recipients)
	if err != nil {
		return fmt.Errorf("marshal recipients: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("event_type", notifications.EventTypeMaintenanceDigest)
	rec.Set("name", notifications.DefaultName(notifications.EventTypeMaintenanceDigest))
	rec.Set("enabled", true)
	rec.Set("subject", subject)
	rec.Set("body", body)
	rec.Set("recipients", string(recipientsJSON))
	if err := app.Save(rec); err != nil {
		return fmt.Errorf("seed maintenance-digest template: %w", err)
	}
	return nil
}
