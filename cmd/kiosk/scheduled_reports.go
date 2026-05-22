package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/ledger"
	"github.com/skeeeon/kiosk/internal/notifications"
)

// reportRunner builds the context for one scheduled report. Returning the
// payload separately from the send lets the scheduler share status-stamping
// + notifier wiring across every report type.
type reportRunner func(app core.App) (eventType string, data any, err error)

// reportRunners is the dispatch table referenced by every scheduled_reports
// row. Adding a new report key means adding a runner here and a case to
// the SPA's report_key dropdown.
var reportRunners = map[string]reportRunner{
	"open_checkouts": runOpenCheckoutsDigest,
}

// runOpenCheckoutsDigest builds an OpenChecksDigestContext by replaying
// the ledger. Returns an empty-rows context (not an error) when nothing is
// checked out so the digest still fires and the email body renders the
// "no items currently checked out" branch.
func runOpenCheckoutsDigest(app core.App) (string, any, error) {
	rows, err := ledger.ReplayOpenRows(app, "")
	if err != nil {
		return "", nil, fmt.Errorf("replay open rows: %w", err)
	}
	dtos, err := ledger.Hydrate(app, rows)
	if err != nil {
		return "", nil, fmt.Errorf("hydrate open rows: %w", err)
	}
	id := kioskctx.Get()
	out := make([]notifications.OpenChecksDigestRow, 0, len(dtos))
	for _, d := range dtos {
		row := notifications.OpenChecksDigestRow{
			Serial:       d.Serial,
			CheckedOutAt: d.CheckedOutAt,
		}
		if d.Expand.Item != nil {
			row.ItemCode = d.Expand.Item.Code
			row.ItemName = d.Expand.Item.Name
		}
		if d.Expand.User != nil {
			row.UserCode = d.Expand.User.Code
			row.UserName = d.Expand.User.Name
		}
		out = append(out, row)
	}
	ctx := notifications.OpenChecksDigestContext{
		Kiosk: notifications.KioskInfo{
			Code:         id.KioskCode,
			LocationCode: id.LocationCode,
		},
		GeneratedAt: time.Now().UTC(),
		Rows:        out,
		RowsCount:   len(out),
	}
	return notifications.EventTypeOpenChecksDigest, ctx, nil
}

// registerScheduledReports walks scheduled_reports and registers a cron
// entry for every enabled row. PB record hooks (bindScheduledReportsHooks)
// keep the cron table in sync as rows change.
func registerScheduledReports(app core.App, notifier *notifications.Notifier) {
	rows, err := app.FindRecordsByFilter("scheduled_reports", "enabled = true", "", 0, 0)
	if err != nil {
		log.Printf("scheduled reports: list failed at boot — %v", err)
		return
	}
	for _, r := range rows {
		addScheduledReportCron(app, notifier, r)
	}
	log.Printf("scheduled reports: registered %d enabled rows", len(rows))
}

// bindScheduledReportsHooks installs create/update/delete record hooks so
// the cron table never drifts from the DB. Update is the trickiest case:
// the row id stays the same but the cron expression can change, so we
// always Remove + maybe Add on update.
func bindScheduledReportsHooks(app core.App, notifier *notifications.Notifier) {
	app.OnRecordAfterCreateSuccess("scheduled_reports").BindFunc(func(e *core.RecordEvent) error {
		if e.Record.GetBool("enabled") {
			addScheduledReportCron(app, notifier, e.Record)
		}
		return e.Next()
	})
	app.OnRecordAfterUpdateSuccess("scheduled_reports").BindFunc(func(e *core.RecordEvent) error {
		app.Cron().Remove(e.Record.Id)
		if e.Record.GetBool("enabled") {
			addScheduledReportCron(app, notifier, e.Record)
		}
		return e.Next()
	})
	app.OnRecordAfterDeleteSuccess("scheduled_reports").BindFunc(func(e *core.RecordEvent) error {
		app.Cron().Remove(e.Record.Id)
		return e.Next()
	})
}

// addScheduledReportCron registers one row with app.Cron(). Failures here
// are logged + stamped on the row's last_error so admins see them in the
// SPA, but the kiosk keeps running.
func addScheduledReportCron(app core.App, notifier *notifications.Notifier, row *core.Record) {
	expr, err := notifications.CronExpressionFor(
		row.GetString("cadence"),
		row.GetInt("hour"),
		row.GetInt("weekday"),
		row.GetInt("day_of_month"),
	)
	if err != nil {
		log.Printf("scheduled reports: invalid schedule on row %s — %v", row.Id, err)
		markScheduledReportStatus(app, row.Id, "failed", err.Error())
		return
	}
	rowID := row.Id
	if err := app.Cron().Add(rowID, expr, func() {
		runScheduledReport(app, notifier, rowID)
	}); err != nil {
		log.Printf("scheduled reports: cron add failed for row %s — %v", row.Id, err)
		markScheduledReportStatus(app, row.Id, "failed", err.Error())
	}
}

// runScheduledReport is the per-fire job body. It re-reads the row (so
// edits since registration apply), resolves the runner, builds the
// payload, and dispatches the send via notifier.SendTo with the row's
// recipients spec.
func runScheduledReport(app core.App, notifier *notifications.Notifier, rowID string) {
	row, err := app.FindRecordById("scheduled_reports", rowID)
	if err != nil {
		log.Printf("scheduled reports: row %s gone at fire time — %v", rowID, err)
		return
	}
	if !row.GetBool("enabled") {
		return
	}
	runner, ok := reportRunners[row.GetString("report_key")]
	if !ok {
		markScheduledReportStatus(app, rowID, "failed",
			fmt.Sprintf("unknown report key %q", row.GetString("report_key")))
		return
	}

	eventType, data, err := runner(app)
	if err != nil {
		markScheduledReportStatus(app, rowID, "failed", err.Error())
		return
	}

	recipients := recipientsFromRow(row)
	if err := notifier.SendTo(eventType, data, recipients); err != nil {
		markScheduledReportStatus(app, rowID, "failed", err.Error())
		return
	}
	markScheduledReportStatus(app, rowID, "sent", "")
}

func recipientsFromRow(row *core.Record) notifications.Recipients {
	raw := strings.TrimSpace(row.GetString("recipients"))
	if raw == "" || raw == "null" {
		return notifications.DefaultRecipients(notifications.EventTypeOpenChecksDigest)
	}
	var r notifications.Recipients
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		log.Printf("scheduled reports: recipients parse failed on row %s — %v", row.Id, err)
		return notifications.DefaultRecipients(notifications.EventTypeOpenChecksDigest)
	}
	return r
}

// markScheduledReportStatus stamps last_run_at + last_status + last_error
// so admins see the outcome of the most recent fire in the SPA. Best-effort
// — failure to stamp is logged but doesn't propagate.
func markScheduledReportStatus(app core.App, rowID, status, errMsg string) {
	row, err := app.FindRecordById("scheduled_reports", rowID)
	if err != nil {
		log.Printf("scheduled reports: status reload failed for %s — %v", rowID, err)
		return
	}
	row.Set("last_run_at", time.Now().UTC())
	row.Set("last_status", status)
	row.Set("last_error", truncate(errMsg, 500))
	if err := app.Save(row); err != nil {
		log.Printf("scheduled reports: status save failed for %s — %v", rowID, err)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
