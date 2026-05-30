// Package metrics computes a point-in-time operational + activity snapshot for
// a single kiosk. It is the one source of truth so the local HTTP endpoint
// (internal/handlers/metrics.go) and the controller→kiosk metrics.snapshot NATS
// command (internal/commands/metrics.go) return identical shapes.
//
// The package deliberately imports only PocketBase core + stdlib + exports
// (for the shared low-stock predicate). Process-level state — uptime, NATS
// connectivity, RFID reader status, active cart count — is gathered by the
// caller and passed in as Operational, because that state lives behind the
// handlers package and reaching for it here would invert the dependency.
package metrics

import (
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/exports"
)

// Operational is the kiosk's process-level health. Gathered by the caller
// (handlers.OperationalMetrics) and passed into Compute unchanged.
type Operational struct {
	UptimeSeconds int64  `json:"uptime_seconds"`
	NATSConnected bool   `json:"nats_connected"`
	RFIDEnabled   bool   `json:"rfid_enabled"`
	RFIDMode      string `json:"rfid_mode,omitempty"`
	RFIDConnected bool   `json:"rfid_connected"`
	ActiveCarts   int    `json:"active_carts"`
}

// Ledger is the cheap activity snapshot, all derived from the kiosk's local
// tables. None of these require replaying the full ledger — open_checkouts is
// bounded by what's currently out, and the transaction counts are indexed
// date-range scans.
type Ledger struct {
	ItemsOut          int `json:"items_out"`            // COUNT(*) open_checkouts
	UsersWithItemsOut int `json:"users_with_items_out"` // COUNT(DISTINCT user)
	LowStockSKUs      int `json:"low_stock_skus"`
	TransactionsToday int `json:"transactions_today"`
	TransactionsWeek  int `json:"transactions_week"` // rolling 7 days
}

// Snapshot is the full metrics payload returned to both the local UI and the
// controller.
type Snapshot struct {
	KioskCode   string      `json:"kiosk_code"`
	GeneratedAt string      `json:"generated_at"` // RFC3339, UTC
	Operational Operational `json:"operational"`
	Ledger      Ledger      `json:"ledger"`
}

// Compute fills the Ledger half from the DB and assembles the snapshot around
// the caller-supplied Operational half.
func Compute(app core.App, op Operational, kioskCode string) (Snapshot, error) {
	led, err := computeLedger(app, kioskCode)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		KioskCode:   kioskCode,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Operational: op,
		Ledger:      led,
	}, nil
}

func computeLedger(app core.App, kioskCode string) (Ledger, error) {
	var led Ledger

	// Items out + distinct holders in one pass over the small open_checkouts
	// table (consumables and returns never touch it, so every row is a tool
	// currently out, with user always populated).
	var oc struct {
		Total         int `db:"total"`
		DistinctUsers int `db:"distinct_users"`
	}
	if err := app.DB().
		NewQuery("SELECT COUNT(*) AS total, COUNT(DISTINCT user) AS distinct_users FROM open_checkouts").
		One(&oc); err != nil {
		return led, fmt.Errorf("count open_checkouts: %w", err)
	}
	led.ItemsOut = oc.Total
	led.UsersWithItemsOut = oc.DistinctUsers

	// Low-stock SKUs — reuse the CSV report's exact predicate so the count
	// can't drift from what the Reports view shows.
	lowRows, err := exports.ComputeLowStockRows(app, kioskCode)
	if err != nil {
		return led, fmt.Errorf("compute low stock: %w", err)
	}
	led.LowStockSKUs = len(lowRows)

	// Transactions today (since UTC midnight) and over the rolling 7-day
	// window, by completed_at. Cutoffs are formatted to match PB's stored
	// datetime string, same as the retention cron and CSV filters.
	now := time.Now().UTC()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	weekAgo := now.Add(-7 * 24 * time.Hour)

	today, err := app.CountRecords("transactions",
		dbx.NewExp("completed_at >= {:cutoff}", dbx.Params{"cutoff": pbDateTime(startOfToday)}))
	if err != nil {
		return led, fmt.Errorf("count transactions today: %w", err)
	}
	led.TransactionsToday = int(today)

	week, err := app.CountRecords("transactions",
		dbx.NewExp("completed_at >= {:cutoff}", dbx.Params{"cutoff": pbDateTime(weekAgo)}))
	if err != nil {
		return led, fmt.Errorf("count transactions week: %w", err)
	}
	led.TransactionsWeek = int(week)

	return led, nil
}

// pbDateTime formats a time the way PocketBase stores (and filters) datetime
// columns. Matches the format used by the notifications retention cron.
func pbDateTime(t time.Time) string {
	return t.Format("2006-01-02 15:04:05.000Z")
}
