package migrations

import (
	"encoding/json"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// Adds the per-worker timeclock summary: appends "timeclock_self" to the
// scheduled_reports.report_key select values and seeds its
// notification_templates row from the compiled-in defaults. Sibling of
// 1799100000_timeclock_digest.go — same shape, same idempotency guards,
// reusing extendScheduledReportsKey / removeScheduledReportsKey. The report_key
// ("timeclock_self") names the scheduler's per-worker fan-out runner; the
// seeded template's event_type is digest.timeclock_self
// (notifications.EventTypeTimeclockSelfDigest).
//
// Runs on both binaries (the controller blank-imports the migrations package),
// so the controller gets the template + report_key option automatically — the
// fan-out runner is pure-DB and needs no controller-side RegisterRunner
// override, unlike the maintenance / open-checkouts digests.

func init() {
	m.Register(addTimeclockSelfDigestUp, addTimeclockSelfDigestDown)
}

func addTimeclockSelfDigestUp(app core.App) error {
	if err := extendScheduledReportsKey(app, "timeclock_self"); err != nil {
		return err
	}
	return seedTimeclockSelfDigestTemplate(app)
}

func addTimeclockSelfDigestDown(app core.App) error {
	if rec, err := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeTimeclockSelfDigest},
	); err == nil && rec != nil {
		if err := app.Delete(rec); err != nil {
			return fmt.Errorf("delete timeclock-self-digest template row: %w", err)
		}
	}
	return removeScheduledReportsKey(app, "timeclock_self")
}

func seedTimeclockSelfDigestTemplate(app core.App) error {
	existing, _ := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeTimeclockSelfDigest},
	)
	if existing != nil {
		return nil
	}
	col, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return fmt.Errorf("find %s: %w", notifications.CollectionName, err)
	}
	subject, body, ok := notifications.Defaults(notifications.EventTypeTimeclockSelfDigest)
	if !ok {
		return fmt.Errorf("no defaults for %s", notifications.EventTypeTimeclockSelfDigest)
	}
	recipients := notifications.DefaultRecipients(notifications.EventTypeTimeclockSelfDigest)
	recipientsJSON, err := json.Marshal(recipients)
	if err != nil {
		return fmt.Errorf("marshal recipients: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("event_type", notifications.EventTypeTimeclockSelfDigest)
	rec.Set("name", notifications.DefaultName(notifications.EventTypeTimeclockSelfDigest))
	rec.Set("enabled", true)
	rec.Set("subject", subject)
	rec.Set("body", body)
	rec.Set("recipients", string(recipientsJSON))
	if err := app.Save(rec); err != nil {
		return fmt.Errorf("seed timeclock-self-digest template: %w", err)
	}
	return nil
}
