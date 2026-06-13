package scheduler

import (
	"fmt"
	"sort"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/exports"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/timeclock"
)

// runTimeclockSelfDigest is the per-worker fan-out: one private timesheet email
// per active worker who has punches in the cadence window. It is a fanOutRunner
// — it owns its own sends rather than returning a single context to runOnce.
//
// It works UNMODIFIED on both binaries — the kiosk reads its local punch
// ledger, the controller its fleet projection — because the timeclock ledger
// is fully projected to the controller, so no NATS fan-out is needed (unlike
// the maintenance / open-checkouts digests, which need live per-kiosk
// snapshots and are swapped via RegisterRunner). On the controller a
// fleet-wide row (empty kiosk_code) therefore gives each worker their
// fleet-COMPLETE timesheet across every kiosk they punched at.
//
// kiosk_code on the row scopes the data the same way the admin digest does:
// empty = fleet-wide (controller) / local (standalone); set = one kiosk.
//
// Recipients are implicit: each email is delivered ONLY to the worker it
// summarizes, via Recipients{WorkerEmail:true} + the context's WorkerEmail().
// The row's recipients column is intentionally ignored on this path. Per-worker
// send failures are recorded in the send log (one row each) but do NOT fail the
// schedule — only a structural failure (loading the punches) does, surfaced as
// the runner's returned error.
func runTimeclockSelfDigest(app core.App, row *core.Record, send Sender) error {
	cadence := row.GetString("cadence")
	window, err := dailyActivityWindowFor(cadence)
	if err != nil {
		return err
	}
	rowKioskCode := row.GetString("kiosk_code")
	windowEnd := time.Now().UTC()
	windowStart := windowEnd.Add(-window)

	_, pairRows, err := exports.LoadTimeclockPunches(app, exports.TimeclockQueryOptions{
		From:      windowStart.Local().Format("2006-01-02"),
		To:        windowEnd.Local().Format("2006-01-02"),
		KioskCode: rowKioskCode,
	})
	if err != nil {
		return fmt.Errorf("load punches: %w", err)
	}
	paired := timeclock.Pair(pairRows, time.Local)

	// Group the paired day-totals into one bucket per worker. Key on user_code,
	// not the punch's user FK: user_code is fleet-stable, whereas a projected
	// punch's FK id may not line up with the controller's own users row id.
	type bucket struct {
		rows    []notifications.TimeclockDigestRow
		seconds int64
		open    bool
	}
	byCode := map[string]*bucket{}
	order := make([]string, 0)
	for _, t := range paired.DayTotals {
		b, ok := byCode[t.UserCode]
		if !ok {
			b = &bucket{}
			byCode[t.UserCode] = b
			order = append(order, t.UserCode)
		}
		b.rows = append(b.rows, notifications.TimeclockDigestRow{
			UserCode: t.UserCode,
			UserName: t.UserName,
			Date:     t.Date,
			Total:    formatHoursMinutes(t.Seconds),
			Open:     t.Open,
		})
		b.seconds += t.Seconds
		if t.Open {
			b.open = true
		}
	}
	// Per-worker count of unpaired punches, surfaced as a "needs review" note.
	uncorrelated := map[string]int{}
	for _, u := range paired.Uncorrelated {
		uncorrelated[u.UserCode]++
	}
	sort.Strings(order) // deterministic send order (and test stability)

	kiosk := digestKioskInfo(rowKioskCode)
	generatedAt := time.Now().UTC()
	for _, code := range order {
		b := byCode[code]
		worker, ferr := app.FindFirstRecordByFilter("users", "code = {:c}", dbx.Params{"c": code})
		if ferr != nil || worker == nil {
			continue // worker no longer in the catalog — skip silently
		}
		if !worker.GetBool("active") {
			continue // default audience is active workers only
		}
		ctx := notifications.TimeclockSelfDigestContext{
			Kiosk: kiosk,
			Worker: notifications.UserInfo{
				ID:    worker.Id,
				Code:  code,
				Name:  worker.GetString("name"),
				Email: worker.GetString("email"),
			},
			GeneratedAt:  generatedAt,
			WindowStart:  windowStart,
			WindowEnd:    windowEnd,
			Cadence:      cadence,
			Rows:         b.rows,
			RowsCount:    len(b.rows),
			Total:        formatHoursMinutes(b.seconds),
			ClockedIn:    b.open,
			Uncorrelated: uncorrelated[code],
		}
		// worker_email resolves to this worker via WorkerEmailProvider; a worker
		// with no email address resolves to zero recipients and the notifier
		// logs a "skipped" row. The per-send error is intentionally discarded
		// here — it's already recorded per-recipient in the send log, and one
		// worker's bounce must not fail the whole schedule.
		_ = send(notifications.EventTypeTimeclockSelfDigest, ctx, notifications.Recipients{WorkerEmail: true})
	}
	return nil
}
