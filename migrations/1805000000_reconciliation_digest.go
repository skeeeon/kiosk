package migrations

import (
	"encoding/json"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// Extends scheduled_reports to host the reconciliation digest: appends
// "reconciliation" to the report_key select values and seeds its
// notification_templates row from the compiled-in defaults
// (docs/location-sightings-plan.md, L4 — the previously-deferred scheduled
// custody-vs-location report).
//
// Sibling of 1798000000_maintenance_digest.go — same shape, same idempotency
// guards, reusing extendScheduledReportsKey / removeScheduledReportsKey. The
// report_key ("reconciliation") names the scheduler runner (standalone, or the
// controller's RegisterRunner override); the seeded template's event_type is
// digest.reconciliation. Runs on both binaries (the controller imports the
// kiosk migrations), so the enum + template exist wherever the scheduler runs.

func init() {
	m.Register(addReconciliationDigestUp, addReconciliationDigestDown)
}

func addReconciliationDigestUp(app core.App) error {
	if err := extendScheduledReportsKey(app, "reconciliation"); err != nil {
		return err
	}
	return seedReconciliationDigestTemplate(app)
}

func addReconciliationDigestDown(app core.App) error {
	if rec, err := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeReconciliationDigest},
	); err == nil && rec != nil {
		if err := app.Delete(rec); err != nil {
			return fmt.Errorf("delete reconciliation-digest template row: %w", err)
		}
	}
	if err := removeScheduledReportsKey(app, "reconciliation"); err != nil {
		return err
	}
	return nil
}

func seedReconciliationDigestTemplate(app core.App) error {
	existing, _ := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeReconciliationDigest},
	)
	if existing != nil {
		return nil
	}
	col, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return fmt.Errorf("find %s: %w", notifications.CollectionName, err)
	}
	subject, body, ok := notifications.Defaults(notifications.EventTypeReconciliationDigest)
	if !ok {
		return fmt.Errorf("no defaults for %s", notifications.EventTypeReconciliationDigest)
	}
	recipients := notifications.DefaultRecipients(notifications.EventTypeReconciliationDigest)
	recipientsJSON, err := json.Marshal(recipients)
	if err != nil {
		return fmt.Errorf("marshal recipients: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("event_type", notifications.EventTypeReconciliationDigest)
	rec.Set("name", notifications.DefaultName(notifications.EventTypeReconciliationDigest))
	rec.Set("enabled", true)
	rec.Set("subject", subject)
	rec.Set("body", body)
	rec.Set("recipients", string(recipientsJSON))
	if err := app.Save(rec); err != nil {
		return fmt.Errorf("seed reconciliation-digest template: %w", err)
	}
	return nil
}
