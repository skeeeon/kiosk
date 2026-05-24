package migrations

import (
	"encoding/json"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// Extends the scheduled_reports collection to host the daily-activity
// digest: appends "daily_activity" to the report_key select values and
// seeds its notification_templates row from the compiled-in defaults.
//
// Sibling of 1785000000_scheduled_reports.go — same shape, same idempotency
// guards. The seeded template body renders DailyActivityContext.

func init() {
	m.Register(addDailyActivityDigestUp, addDailyActivityDigestDown)
}

func addDailyActivityDigestUp(app core.App) error {
	if err := extendScheduledReportsKey(app, "daily_activity"); err != nil {
		return err
	}
	return seedDailyActivityTemplate(app)
}

func addDailyActivityDigestDown(app core.App) error {
	if rec, err := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeDailyActivity},
	); err == nil && rec != nil {
		if err := app.Delete(rec); err != nil {
			return fmt.Errorf("delete daily-activity template row: %w", err)
		}
	}
	if err := removeScheduledReportsKey(app, "daily_activity"); err != nil {
		return err
	}
	return nil
}

func extendScheduledReportsKey(app core.App, value string) error {
	col, err := app.FindCollectionByNameOrId("scheduled_reports")
	if err != nil {
		return fmt.Errorf("find scheduled_reports: %w", err)
	}
	field := col.Fields.GetByName("report_key")
	if field == nil {
		return fmt.Errorf("scheduled_reports.report_key field missing")
	}
	sel, ok := field.(*core.SelectField)
	if !ok {
		return fmt.Errorf("scheduled_reports.report_key is not a SelectField")
	}
	for _, v := range sel.Values {
		if v == value {
			return nil
		}
	}
	sel.Values = append(sel.Values, value)
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save scheduled_reports: %w", err)
	}
	return nil
}

func removeScheduledReportsKey(app core.App, value string) error {
	col, err := app.FindCollectionByNameOrId("scheduled_reports")
	if err != nil {
		return nil // collection already gone — nothing to do
	}
	field := col.Fields.GetByName("report_key")
	if field == nil {
		return nil
	}
	sel, ok := field.(*core.SelectField)
	if !ok {
		return nil
	}
	out := sel.Values[:0]
	for _, v := range sel.Values {
		if v != value {
			out = append(out, v)
		}
	}
	sel.Values = out
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save scheduled_reports: %w", err)
	}
	return nil
}

func seedDailyActivityTemplate(app core.App) error {
	existing, _ := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeDailyActivity},
	)
	if existing != nil {
		return nil
	}
	col, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return fmt.Errorf("find %s: %w", notifications.CollectionName, err)
	}
	subject, body, ok := notifications.Defaults(notifications.EventTypeDailyActivity)
	if !ok {
		return fmt.Errorf("no defaults for %s", notifications.EventTypeDailyActivity)
	}
	recipients := notifications.DefaultRecipients(notifications.EventTypeDailyActivity)
	recipientsJSON, err := json.Marshal(recipients)
	if err != nil {
		return fmt.Errorf("marshal recipients: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("event_type", notifications.EventTypeDailyActivity)
	rec.Set("name", notifications.DefaultName(notifications.EventTypeDailyActivity))
	rec.Set("enabled", true)
	rec.Set("subject", subject)
	rec.Set("body", body)
	rec.Set("recipients", string(recipientsJSON))
	if err := app.Save(rec); err != nil {
		return fmt.Errorf("seed daily-activity template: %w", err)
	}
	return nil
}
