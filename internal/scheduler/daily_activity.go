package scheduler

import (
	"fmt"
	"sort"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// dailyActivityWindowFor maps a schedule row's cadence to the lookback
// window for the runner. Keeps the window contract in one place so the
// template subject/body and the test fixtures stay in sync.
func dailyActivityWindowFor(cadence string) (time.Duration, error) {
	switch cadence {
	case "daily":
		return 24 * time.Hour, nil
	case "weekly":
		return 7 * 24 * time.Hour, nil
	case "monthly":
		return 30 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("daily_activity: unsupported cadence %q", cadence)
}

// runDailyActivityDigest aggregates the completed transactions in the
// window defined by the schedule's cadence and returns a populated
// DailyActivityContext. Empty windows return a populated context with
// zero counts (not an error) so the template renders its "no activity"
// branch and the schedule row stamps as sent.
//
// kiosk_code on the schedule row scopes the report. Empty = "this kiosk"
// (standalone) or "fleet-wide" (controller); set = one kiosk in the
// fleet (controller-only path).
func runDailyActivityDigest(app core.App, row *core.Record) (string, any, error) {
	cadence := row.GetString("cadence")
	window, err := dailyActivityWindowFor(cadence)
	if err != nil {
		return "", nil, err
	}
	windowEnd := time.Now().UTC()
	windowStart := windowEnd.Add(-window)

	rowKioskCode := row.GetString("kiosk_code")
	ctx := notifications.DailyActivityContext{
		Kiosk:       digestKioskInfo(rowKioskCode),
		GeneratedAt: time.Now().UTC(),
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		Cadence:     cadence,
	}

	// Transactions in the window. PB stores completed_at as a DateField;
	// the standard ISO-with-Z layout PB itself emits is the filter format.
	txFilter := "status = {:s} && completed_at >= {:from} && completed_at <= {:to}"
	txParams := dbx.Params{
		"s":    "completed",
		"from": windowStart.Format("2006-01-02 15:04:05.000Z"),
		"to":   windowEnd.Format("2006-01-02 15:04:05.000Z"),
	}
	if rowKioskCode != "" {
		txFilter += " && kiosk_code = {:kc}"
		txParams["kc"] = rowKioskCode
	}
	txs, err := app.FindRecordsByFilter("transactions", txFilter,
		"completed_at", 0, 0, txParams)
	if err != nil {
		return "", nil, fmt.Errorf("daily_activity: list transactions: %w", err)
	}

	ctx.TransactionCount = len(txs)
	if len(txs) == 0 {
		return notifications.EventTypeDailyActivity, ctx, nil
	}

	// Build txID → userID so we can attribute each line to a worker
	// without an extra round-trip per line.
	txUser := make(map[string]string, len(txs))
	uniqueUsers := make(map[string]struct{}, len(txs))
	for _, t := range txs {
		uid := t.GetString("user")
		txUser[t.Id] = uid
		if uid != "" {
			uniqueUsers[uid] = struct{}{}
		}
	}
	ctx.UniqueUsers = len(uniqueUsers)

	// Lines via indirect filter on the transaction relation — same trick
	// the group-activity report uses to avoid enumerating tx IDs in a
	// giant OR.
	lineFilter := "transaction.status = {:s} && transaction.completed_at >= {:from} && transaction.completed_at <= {:to}"
	lineParams := dbx.Params{
		"s":    "completed",
		"from": windowStart.Format("2006-01-02 15:04:05.000Z"),
		"to":   windowEnd.Format("2006-01-02 15:04:05.000Z"),
	}
	if rowKioskCode != "" {
		lineFilter += " && transaction.kiosk_code = {:kc}"
		lineParams["kc"] = rowKioskCode
	}
	lines, err := app.FindRecordsByFilter("transaction_lines", lineFilter,
		"", 0, 0, lineParams)
	if err != nil {
		return "", nil, fmt.Errorf("daily_activity: list lines: %w", err)
	}

	itemTally := make(map[string]*tally)
	userTally := make(map[string]*tally)

	for _, l := range lines {
		qty := l.GetInt("qty")
		ctx.LinesCount += qty
		switch l.GetString("action") {
		case "checkout":
			ctx.CheckedOut += qty
		case "return":
			ctx.Returned += qty
		case "consume":
			ctx.Consumed += qty
		}

		itemID := l.GetString("item")
		if itemID != "" {
			t, ok := itemTally[itemID]
			if !ok {
				t = &tally{}
				itemTally[itemID] = t
			}
			t.count++
		}

		txID := l.GetString("transaction")
		if uid := txUser[txID]; uid != "" {
			t, ok := userTally[uid]
			if !ok {
				t = &tally{}
				userTally[uid] = t
			}
			t.count++
		}
	}

	// Hydrate code/name on the entries we'll keep. Loading them all up
	// front is wasteful for big catalogs; sort first, take top N, then
	// look up only those rows.
	ctx.TopItems = topItems(app, itemTally, 5)
	ctx.TopWorkers = topWorkers(app, userTally, 5)

	return notifications.EventTypeDailyActivity, ctx, nil
}

// topItems returns the top-N item tallies, hydrated with item code+name.
// Ties on count fall back to item id ascending so the output is
// deterministic across runs and across test environments.
func topItems(app core.App, m map[string]*tally, n int) []notifications.DailyActivityItemRow {
	ids := rankedIDs(m, n)
	out := make([]notifications.DailyActivityItemRow, 0, len(ids))
	for _, id := range ids {
		row := notifications.DailyActivityItemRow{LineCount: m[id].count}
		if rec, err := app.FindRecordById("items", id); err == nil {
			row.ItemCode = rec.GetString("code")
			row.ItemName = rec.GetString("name")
		}
		out = append(out, row)
	}
	return out
}

func topWorkers(app core.App, m map[string]*tally, n int) []notifications.DailyActivityWorkerRow {
	ids := rankedIDs(m, n)
	out := make([]notifications.DailyActivityWorkerRow, 0, len(ids))
	for _, id := range ids {
		row := notifications.DailyActivityWorkerRow{LineCount: m[id].count}
		if rec, err := app.FindRecordById("users", id); err == nil {
			row.UserCode = rec.GetString("code")
			row.UserName = rec.GetString("name")
		}
		out = append(out, row)
	}
	return out
}

// rankedIDs returns up to n IDs sorted by tally count descending, then by
// ID ascending for a stable tiebreak.
func rankedIDs(m map[string]*tally, n int) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		if m[ids[i]].count != m[ids[j]].count {
			return m[ids[i]].count > m[ids[j]].count
		}
		return ids[i] < ids[j]
	})
	if len(ids) > n {
		ids = ids[:n]
	}
	return ids
}

// tally is the per-key accumulator for items and workers. Kept private to
// the package — the public shape is notifications.DailyActivity*Row.
type tally struct {
	count int
}
