package migrations

import (
	"encoding/json"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// Phase 4 of the notifications roadmap:
//
//   1. Creates notification_dedupe — a tiny key/day table that holds the
//      (event_type, ref, day) tuples that have already fired today. The
//      unique composite index is the enforcement mechanism: the notifier's
//      SendIfFirst attempts the insert, and on unique-constraint failure
//      treats it as "already fired, drop." No race between check + send.
//
//   2. Seeds the alert.lowstock template row from the compiled-in default
//      so a fresh install AND an existing database both get the row.
//      SeededEventTypes() now lists it too, but that path only fires on
//      first install; this migration covers the upgrade.
//
// Both changes are additive.

func init() {
	m.Register(addLowStockAlertsUp, addLowStockAlertsDown)
}

func addLowStockAlertsUp(app core.App) error {
	if err := createNotificationDedupeCollection(app); err != nil {
		return err
	}
	return seedLowStockTemplate(app)
}

func addLowStockAlertsDown(app core.App) error {
	if col, err := app.FindCollectionByNameOrId("notification_dedupe"); err == nil {
		if err := app.Delete(col); err != nil {
			return fmt.Errorf("delete notification_dedupe: %w", err)
		}
	}
	if rec, err := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeLowStock},
	); err == nil && rec != nil {
		if err := app.Delete(rec); err != nil {
			return fmt.Errorf("delete lowstock template row: %w", err)
		}
	}
	return nil
}

func createNotificationDedupeCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("notification_dedupe"); err == nil {
		return nil
	}
	col := core.NewBaseCollection("notification_dedupe")
	col.Fields.Add(&core.TextField{Name: "event_type", Required: true})
	col.Fields.Add(&core.TextField{Name: "ref", Required: true})
	col.Fields.Add(&core.TextField{Name: "day", Required: true})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})

	// Unique on the triple — the SendIfFirst path relies on this for race-free
	// "first fire of the day per item" gating.
	col.AddIndex("idx_notification_dedupe_triple", true, "event_type, ref, day", "")
	col.AddIndex("idx_notification_dedupe_created", false, "created", "")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule
	// CreateRule + UpdateRule + DeleteRule nil — only the notifier path
	// writes (via app.Save bypassing collection rules) and the retention
	// cron deletes via app.Delete.

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save notification_dedupe: %w", err)
	}
	return nil
}

func seedLowStockTemplate(app core.App) error {
	existing, _ := app.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": notifications.EventTypeLowStock},
	)
	if existing != nil {
		return nil
	}
	col, err := app.FindCollectionByNameOrId(notifications.CollectionName)
	if err != nil {
		return fmt.Errorf("find %s: %w", notifications.CollectionName, err)
	}
	subject, body, ok := notifications.Defaults(notifications.EventTypeLowStock)
	if !ok {
		return fmt.Errorf("no defaults for %s", notifications.EventTypeLowStock)
	}
	recipients := notifications.DefaultRecipients(notifications.EventTypeLowStock)
	recipientsJSON, err := json.Marshal(recipients)
	if err != nil {
		return fmt.Errorf("marshal recipients: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("event_type", notifications.EventTypeLowStock)
	rec.Set("name", notifications.DefaultName(notifications.EventTypeLowStock))
	rec.Set("enabled", true)
	rec.Set("subject", subject)
	rec.Set("body", body)
	rec.Set("recipients", string(recipientsJSON))
	if err := app.Save(rec); err != nil {
		return fmt.Errorf("seed lowstock template: %w", err)
	}
	return nil
}
