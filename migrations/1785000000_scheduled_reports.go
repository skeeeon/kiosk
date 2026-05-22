package migrations

import (
	"encoding/json"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// Phase 5 of the notifications roadmap:
//
//   1. Creates scheduled_reports — admin-managed rows that pair a report
//      key with a typed cadence (daily/weekly/monthly + hour, plus weekday
//      or day_of_month) and a recipients spec. The kiosk's boot path
//      walks this table and registers one app.Cron() entry per enabled
//      row; PB record hooks re-register on any save/delete so the cron
//      table stays in sync with the DB.
//
//   2. Seeds the digest.open_checkouts notification_templates row from the
//      compiled-in default, so the editor shows it alongside the receipt
//      and low-stock templates immediately after deploy.
//
// Schedule UI is a typed enum (cadence + hour selectors). Cron strings
// stay server-side; admins never see one.

func init() {
	m.Register(addScheduledReportsUp, addScheduledReportsDown)
}

func addScheduledReportsUp(app core.App) error {
	if err := createScheduledReportsCollection(app); err != nil {
		return err
	}
	return seedOpenChecksDigestTemplate(app)
}

func addScheduledReportsDown(app core.App) error {
	if col, err := app.FindCollectionByNameOrId("scheduled_reports"); err == nil {
		if err := app.Delete(col); err != nil {
			return fmt.Errorf("delete scheduled_reports: %w", err)
		}
	}
	if rec, err := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeOpenChecksDigest},
	); err == nil && rec != nil {
		if err := app.Delete(rec); err != nil {
			return fmt.Errorf("delete digest template row: %w", err)
		}
	}
	return nil
}

func createScheduledReportsCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("scheduled_reports"); err == nil {
		return nil
	}
	col := core.NewBaseCollection("scheduled_reports")
	col.Fields.Add(&core.SelectField{
		Name:      "report_key",
		Values:    []string{"open_checkouts"},
		Required:  true,
		MaxSelect: 1,
	})
	col.Fields.Add(&core.SelectField{
		Name:      "cadence",
		Values:    []string{"daily", "weekly", "monthly"},
		Required:  true,
		MaxSelect: 1,
	})
	col.Fields.Add(&core.NumberField{Name: "hour", OnlyInt: true, Required: true})
	col.Fields.Add(&core.NumberField{Name: "weekday", OnlyInt: true})
	col.Fields.Add(&core.NumberField{Name: "day_of_month", OnlyInt: true})
	col.Fields.Add(&core.BoolField{Name: "enabled"})
	col.Fields.Add(&core.JSONField{Name: "recipients"})
	col.Fields.Add(&core.TextField{Name: "subject_override"})
	col.Fields.Add(&core.DateField{Name: "last_run_at"})
	col.Fields.Add(&core.TextField{Name: "last_status"})
	col.Fields.Add(&core.TextField{Name: "last_error"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	col.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})

	col.AddIndex("idx_scheduled_reports_enabled", false, "enabled", "")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule
	col.CreateRule = &rule
	col.UpdateRule = &rule
	col.DeleteRule = &rule

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save scheduled_reports: %w", err)
	}
	return nil
}

func seedOpenChecksDigestTemplate(app core.App) error {
	existing, _ := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeOpenChecksDigest},
	)
	if existing != nil {
		return nil
	}
	col, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return fmt.Errorf("find %s: %w", notifications.CollectionName, err)
	}
	subject, body, ok := notifications.Defaults(notifications.EventTypeOpenChecksDigest)
	if !ok {
		return fmt.Errorf("no defaults for %s", notifications.EventTypeOpenChecksDigest)
	}
	recipients := notifications.DefaultRecipients(notifications.EventTypeOpenChecksDigest)
	recipientsJSON, err := json.Marshal(recipients)
	if err != nil {
		return fmt.Errorf("marshal recipients: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("event_type", notifications.EventTypeOpenChecksDigest)
	rec.Set("name", notifications.DefaultName(notifications.EventTypeOpenChecksDigest))
	rec.Set("enabled", true)
	rec.Set("subject", subject)
	rec.Set("body", body)
	rec.Set("recipients", string(recipientsJSON))
	if err := app.Save(rec); err != nil {
		return fmt.Errorf("seed digest template: %w", err)
	}
	return nil
}
