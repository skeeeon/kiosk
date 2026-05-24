package controller

import (
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/scheduler"
)

// TestRunOpenCheckoutsDigest_ControllerSide drives the open-checkouts runner
// against the controller's open_checkouts table. The kiosk_code field on
// the schedule row scopes the digest; empty = fleet-wide.
func TestRunOpenCheckoutsDigest_ControllerSide(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	itemAID := seedItem(t, app, "DRILL-A", "Drill A")
	itemBID := seedItem(t, app, "HAMMER", "Hammer")
	userID := userIDByCode(t, app, "WORKER-1")
	lineA := seedFakeProjectedLine(t, app, "src-line-1", "KIOSK-A")
	lineB := seedFakeProjectedLine(t, app, "src-line-2", "KIOSK-B")

	now := time.Now().UTC()
	seedOpenCheckoutRow(t, app, "KIOSK-A", itemAID, userID, lineA, now)
	seedOpenCheckoutRow(t, app, "KIOSK-B", itemBID, userID, lineB, now)

	// Fleet-wide: empty kiosk_code → both kiosks' rows show up.
	_, data, err := scheduler.RunReport(app, "open_checkouts", scheduleRowCtrl(t, app, "open_checkouts", ""))
	if err != nil {
		t.Fatalf("fleet runner: %v", err)
	}
	ctxFleet := data.(notifications.OpenChecksDigestContext)
	if ctxFleet.RowsCount != 2 {
		t.Errorf("fleet-wide: want 2 rows, got %d", ctxFleet.RowsCount)
	}
	if ctxFleet.Kiosk.Code != "" {
		t.Errorf("fleet-wide kiosk.Code: want empty, got %q", ctxFleet.Kiosk.Code)
	}

	// Per-kiosk: kiosk_code=KIOSK-A → only kiosk A's row.
	_, data, err = scheduler.RunReport(app, "open_checkouts", scheduleRowCtrl(t, app, "open_checkouts", "KIOSK-A"))
	if err != nil {
		t.Fatalf("scoped runner: %v", err)
	}
	ctxScoped := data.(notifications.OpenChecksDigestContext)
	if ctxScoped.RowsCount != 1 {
		t.Errorf("scoped: want 1 row, got %d", ctxScoped.RowsCount)
	}
	if ctxScoped.Kiosk.Code != "KIOSK-A" {
		t.Errorf("scoped kiosk.Code: want KIOSK-A, got %q", ctxScoped.Kiosk.Code)
	}
}

// TestRunDailyActivityDigest_ControllerSide drives the daily-activity runner
// against transactions projected onto the controller's table. Same
// kiosk_code gating: empty = fleet-wide, set = one kiosk.
func TestRunDailyActivityDigest_ControllerSide(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	now := time.Now().UTC()

	agg := NewAggregator(app, nil, "")
	for _, k := range []string{"KIOSK-A", "KIOSK-B"} {
		if out := agg.ProjectTransaction(EventPayload{
			TransactionID: "src-tx-" + k,
			KioskCode:     k,
			LocationCode:  "WEST",
			UserCode:      "WORKER-1",
			StartedAt:     now.Add(-30 * time.Minute),
			CompletedAt:   now.Add(-30 * time.Minute),
			LinesCount:    1,
		}); out != projectAck {
			t.Fatalf("ProjectTransaction %s: %v", k, out)
		}
	}

	_, data, err := scheduler.RunReport(app, "daily_activity", scheduleRowCtrl(t, app, "daily_activity", ""))
	if err != nil {
		t.Fatalf("fleet runner: %v", err)
	}
	ctxFleet := data.(notifications.DailyActivityContext)
	if ctxFleet.TransactionCount != 2 {
		t.Errorf("fleet-wide TransactionCount: want 2, got %d", ctxFleet.TransactionCount)
	}

	_, data, err = scheduler.RunReport(app, "daily_activity", scheduleRowCtrl(t, app, "daily_activity", "KIOSK-A"))
	if err != nil {
		t.Fatalf("scoped runner: %v", err)
	}
	ctxScoped := data.(notifications.DailyActivityContext)
	if ctxScoped.TransactionCount != 1 {
		t.Errorf("scoped TransactionCount: want 1, got %d", ctxScoped.TransactionCount)
	}
	if ctxScoped.Kiosk.Code != "KIOSK-A" {
		t.Errorf("scoped Kiosk.Code: want KIOSK-A, got %q", ctxScoped.Kiosk.Code)
	}
}

// --- helpers ---

func scheduleRowCtrl(t *testing.T, app core.App, key, kioskCode string) *core.Record {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("scheduled_reports")
	if err != nil {
		t.Fatalf("find scheduled_reports: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("report_key", key)
	rec.Set("cadence", "daily")
	rec.Set("hour", 9)
	rec.Set("enabled", true)
	rec.Set("kiosk_code", kioskCode)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save scheduled_reports: %v", err)
	}
	return rec
}

// seedFakeProjectedLine writes a stand-in transaction + line so the
// open_checkouts row in seedOpenCheckoutRow has a valid transaction_line FK
// to reference. The transaction's kiosk_code must match the open_checkouts
// row for the per-kiosk filter to work; sourceLineID disambiguates rows.
func seedFakeProjectedLine(t *testing.T, app core.App, sourceLineID, kioskCode string) string {
	t.Helper()
	txCol, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		t.Fatalf("find transactions: %v", err)
	}
	tx := core.NewRecord(txCol)
	tx.Set("kiosk_code", kioskCode)
	tx.Set("location_code", "WEST")
	tx.Set("status", "completed")
	tx.Set("user", userIDByCode(t, app, "WORKER-1"))
	tx.Set("started_at", time.Now().UTC())
	tx.Set("completed_at", time.Now().UTC())
	tx.Set("source_kiosk_code", kioskCode)
	tx.Set("source_transaction_id", "src-tx-"+sourceLineID)
	if err := app.Save(tx); err != nil {
		t.Fatalf("save tx: %v", err)
	}

	linesCol, err := app.FindCollectionByNameOrId("transaction_lines")
	if err != nil {
		t.Fatalf("find transaction_lines: %v", err)
	}
	line := core.NewRecord(linesCol)
	line.Set("transaction", tx.Id)
	if rec, err := app.FindFirstRecordByFilter("items", "code != ''", dbx.Params{}); err == nil && rec != nil {
		line.Set("item", rec.Id)
	}
	line.Set("action", "checkout")
	line.Set("qty", 1)
	line.Set("source_line_id", sourceLineID)
	if err := app.Save(line); err != nil {
		t.Fatalf("save line: %v", err)
	}
	return line.Id
}

func seedOpenCheckoutRow(t *testing.T, app core.App, kioskCode, itemID, userID, lineID string, checkedOutAt time.Time) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("open_checkouts")
	if err != nil {
		t.Fatalf("find open_checkouts: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("kiosk_code", kioskCode)
	rec.Set("item", itemID)
	rec.Set("user", userID)
	rec.Set("transaction_line", lineID)
	rec.Set("checked_out_at", checkedOutAt)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save open_checkout: %v", err)
	}
}
