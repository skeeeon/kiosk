package scheduler

import (
	"fmt"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/exports"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/timeclock"
)

// runTimeclockDigest builds a TimeclockDigestContext: punches in the cadence
// window paired into per-user daily totals, plus who's clocked in right now.
// Works unmodified on both binaries — the kiosk reads its local punch
// ledger, the controller its fleet projection (kiosk_code on the schedule
// row scopes it; empty = this kiosk / fleet-wide). The window is
// date-bounded (the digest is a human summary; the CSV export is the
// precise payroll record).
func runTimeclockDigest(app core.App, row *core.Record) (string, any, error) {
	cadence := row.GetString("cadence")
	window, err := dailyActivityWindowFor(cadence)
	if err != nil {
		return "", nil, err
	}
	rowKioskCode := row.GetString("kiosk_code")
	windowEnd := time.Now().UTC()
	windowStart := windowEnd.Add(-window)

	punches, pairRows, err := exports.LoadTimeclockPunches(app, exports.TimeclockQueryOptions{
		From:      windowStart.Local().Format("2006-01-02"),
		To:        windowEnd.Local().Format("2006-01-02"),
		KioskCode: rowKioskCode,
	})
	if err != nil {
		return "", nil, fmt.Errorf("load punches: %w", err)
	}
	paired := timeclock.Pair(pairRows, time.Local)

	rows := make([]notifications.TimeclockDigestRow, 0, len(paired.DayTotals))
	for _, t := range paired.DayTotals {
		rows = append(rows, notifications.TimeclockDigestRow{
			UserCode: t.UserCode,
			UserName: t.UserName,
			Date:     t.Date,
			Total:    formatHoursMinutes(t.Seconds),
			Open:     t.Open,
		})
	}

	// "Clocked in now" comes from the ledger merge rule, not the windowed
	// punches — someone clocked in before the window opened still counts.
	// Note this is unscoped by kiosk_code (punch state is per-user, not
	// per-kiosk).
	clockedIn, err := exports.ComputeClockedInNow(app, nil)
	if err != nil {
		return "", nil, fmt.Errorf("compute clocked-in: %w", err)
	}

	ctx := notifications.TimeclockDigestContext{
		Kiosk:        digestKioskInfo(rowKioskCode),
		GeneratedAt:  time.Now().UTC(),
		WindowStart:  windowStart,
		WindowEnd:    windowEnd,
		Cadence:      cadence,
		PunchCount:   len(punches),
		ClockedInNow: len(clockedIn),
		Rows:         rows,
		RowsCount:    len(rows),
		Uncorrelated: len(paired.Uncorrelated),
	}
	return notifications.EventTypeTimeclockDigest, ctx, nil
}

// formatHoursMinutes renders seconds as "7h30m" — template-friendly, no
// trailing seconds noise.
func formatHoursMinutes(seconds int64) string {
	h := seconds / 3600
	m := (seconds % 3600) / 60
	return fmt.Sprintf("%dh%02dm", h, m)
}
