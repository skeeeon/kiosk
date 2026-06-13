package migrations

import (
	"encoding/json"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// Extends scheduled_reports to host the timeclock digest: appends
// "timeclock" to the report_key select values and seeds its
// notification_templates row from the compiled-in defaults. Sibling of
// 1798000000_maintenance_digest.go — same shape, same idempotency guards,
// reusing extendScheduledReportsKey / removeScheduledReportsKey. The
// report_key ("timeclock") names the scheduler runner; the seeded template's
// event_type is digest.timeclock (notifications.EventTypeTimeclockDigest).
// SeededEventTypes() lists it too, but that path only fires on first install;
// this migration covers DBs that pre-date the timeclock feature.

func init() {
	m.Register(addTimeclockDigestUp, addTimeclockDigestDown)
}

func addTimeclockDigestUp(app core.App) error {
	if err := extendScheduledReportsKey(app, "timeclock"); err != nil {
		return err
	}
	return seedTimeclockDigestTemplate(app)
}

func addTimeclockDigestDown(app core.App) error {
	if rec, err := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeTimeclockDigest},
	); err == nil && rec != nil {
		if err := app.Delete(rec); err != nil {
			return fmt.Errorf("delete timeclock-digest template row: %w", err)
		}
	}
	return removeScheduledReportsKey(app, "timeclock")
}

func seedTimeclockDigestTemplate(app core.App) error {
	existing, _ := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeTimeclockDigest},
	)
	if existing != nil {
		return nil
	}
	col, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return fmt.Errorf("find %s: %w", notifications.CollectionName, err)
	}
	subject, body, ok := notifications.Defaults(notifications.EventTypeTimeclockDigest)
	if !ok {
		return fmt.Errorf("no defaults for %s", notifications.EventTypeTimeclockDigest)
	}
	recipients := notifications.DefaultRecipients(notifications.EventTypeTimeclockDigest)
	recipientsJSON, err := json.Marshal(recipients)
	if err != nil {
		return fmt.Errorf("marshal recipients: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("event_type", notifications.EventTypeTimeclockDigest)
	rec.Set("name", notifications.DefaultName(notifications.EventTypeTimeclockDigest))
	rec.Set("enabled", true)
	rec.Set("subject", subject)
	rec.Set("body", body)
	rec.Set("recipients", string(recipientsJSON))
	if err := app.Save(rec); err != nil {
		return fmt.Errorf("seed timeclock-digest template: %w", err)
	}
	return nil
}
