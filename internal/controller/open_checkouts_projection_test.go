package controller

import (
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// Each test projects a transaction first so ProjectLine has a parent to
// attach to, then drives the open_checkouts projector with one or more
// item.{action} payloads. Assertions are on the open_checkouts table.

func TestProjectOpenCheckouts_QtyOneCheckout(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "HAMMER", "Hammer")

	agg := NewAggregator(app, nil, "")
	completedAt := time.Now().UTC()

	if out := agg.ProjectTransaction(EventPayload{
		TransactionID: "src-tx-1", KioskCode: "KIOSK-A", LocationCode: "WEST",
		UserCode: "WORKER-1", StartedAt: completedAt, CompletedAt: completedAt,
		LinesCount: 1,
	}); out != projectAck {
		t.Fatalf("ProjectTransaction: %v", out)
	}

	checkout := EventPayload{
		LineID:        "src-line-1",
		KioskCode:     "KIOSK-A",
		TransactionID: "src-tx-1",
		ItemCode:      "HAMMER",
		UserCode:      "WORKER-1",
		Action:        "checkout",
		Qty:           1,
		CompletedAt:   completedAt,
	}
	if out := agg.ProjectLine(checkout); out != projectAck {
		t.Fatalf("ProjectLine: %v", out)
	}
	if out := agg.ProjectOpenCheckoutsInsert(checkout); out != projectAck {
		t.Fatalf("ProjectOpenCheckoutsInsert: %v", out)
	}

	rows := listOpenCheckouts(t, app, "KIOSK-A")
	if len(rows) != 1 {
		t.Fatalf("open_checkouts: want 1, got %d", len(rows))
	}
	if rows[0].GetString("kiosk_code") != "KIOSK-A" {
		t.Errorf("kiosk_code: got %q", rows[0].GetString("kiosk_code"))
	}
}

func TestProjectOpenCheckouts_QtyNCheckoutCreatesNRows(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "DRILL", "Drill")

	agg := NewAggregator(app, nil, "")
	completedAt := time.Now().UTC()

	if out := agg.ProjectTransaction(EventPayload{
		TransactionID: "src-tx-1", KioskCode: "KIOSK-A", LocationCode: "WEST",
		UserCode: "WORKER-1",
		StartedAt: completedAt, CompletedAt: completedAt, LinesCount: 1,
	}); out != projectAck {
		t.Fatalf("ProjectTransaction: %v", out)
	}

	payload := EventPayload{
		LineID: "src-line-1", KioskCode: "KIOSK-A", TransactionID: "src-tx-1",
		ItemCode: "DRILL", UserCode: "WORKER-1",
		Action: "checkout", Qty: 3, CompletedAt: completedAt,
	}
	mustProject(t, agg, payload)

	if got := countOpenCheckouts(t, app, "KIOSK-A"); got != 3 {
		t.Fatalf("qty=3 checkout: want 3 rows, got %d", got)
	}

	// Redelivery: must not duplicate.
	if out := agg.ProjectOpenCheckoutsInsert(payload); out != projectAck {
		t.Fatalf("redeliver insert: %v", out)
	}
	if got := countOpenCheckouts(t, app, "KIOSK-A"); got != 3 {
		t.Errorf("after redelivery: want 3 rows, got %d", got)
	}
}

func TestProjectOpenCheckouts_ReturnFIFOByTargetUser(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedUser(t, app, "WORKER-2", "Bob")
	seedItem(t, app, "DRILL", "Drill")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	// Alice checks out 2 drills (oldest).
	mustProjectTx(t, agg, "src-tx-A", "WORKER-1", t0)
	mustProject(t, agg, EventPayload{
		LineID: "src-line-A", KioskCode: "KIOSK-A", TransactionID: "src-tx-A",
		ItemCode: "DRILL", UserCode: "WORKER-1",
		Action: "checkout", Qty: 2, CompletedAt: t0,
	})

	// Bob checks out 1 drill later.
	mustProjectTx(t, agg, "src-tx-B", "WORKER-2", t0.Add(1*time.Minute))
	mustProject(t, agg, EventPayload{
		LineID: "src-line-B", KioskCode: "KIOSK-A", TransactionID: "src-tx-B",
		ItemCode: "DRILL", UserCode: "WORKER-2",
		Action: "checkout", Qty: 1, CompletedAt: t0.Add(1 * time.Minute),
	})

	if got := countOpenCheckouts(t, app, "KIOSK-A"); got != 3 {
		t.Fatalf("setup: want 3 rows, got %d", got)
	}

	// Alice returns 1 → one of HER rows must go (FIFO among her two).
	mustProjectTx(t, agg, "src-tx-R", "WORKER-1", t0.Add(5*time.Minute))
	mustProject(t, agg, EventPayload{
		LineID: "src-line-R", KioskCode: "KIOSK-A", TransactionID: "src-tx-R",
		ItemCode: "DRILL", UserCode: "WORKER-1",
		Action: "return", Qty: 1, CompletedAt: t0.Add(5 * time.Minute),
	})

	rows := listOpenCheckouts(t, app, "KIOSK-A")
	if len(rows) != 2 {
		t.Fatalf("after Alice return: want 2 rows, got %d", len(rows))
	}
	// Bob's row must survive; Alice should have 1 row left.
	aliceLeft, bobLeft := 0, 0
	aliceID := userIDByCode(t, app, "WORKER-1")
	bobID := userIDByCode(t, app, "WORKER-2")
	for _, r := range rows {
		switch r.GetString("user") {
		case aliceID:
			aliceLeft++
		case bobID:
			bobLeft++
		}
	}
	if aliceLeft != 1 || bobLeft != 1 {
		t.Errorf("after return: alice=%d bob=%d, want 1 each", aliceLeft, bobLeft)
	}
}

// TestProjectOpenCheckouts_ReturnNoCrossUserBorrow confirms the projector
// mirrors commit exactly: a return against a user with no open rows closes
// NOTHING and leaves other users' rows intact. Commit stamps such a line
// uncorrelated and never borrows from another worker; borrowing here would
// silently close an innocent worker's checkout and drift the controller's
// view from the kiosk. (This used to assert the opposite — the old fallback
// closed Bob's row — which was the bug.)
func TestProjectOpenCheckouts_ReturnNoCrossUserBorrow(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedUser(t, app, "WORKER-2", "Bob")
	seedItem(t, app, "DRILL", "Drill")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	// Only Bob has a drill out.
	mustProjectTx(t, agg, "src-tx-B", "WORKER-2", t0)
	mustProject(t, agg, EventPayload{
		LineID: "src-line-B", KioskCode: "KIOSK-A", TransactionID: "src-tx-B",
		ItemCode: "DRILL", UserCode: "WORKER-2",
		Action: "checkout", Qty: 1, CompletedAt: t0,
	})

	// Alice returns a drill she doesn't have out. Her target-user query
	// matches nothing; the projector must NOT borrow Bob's row.
	mustProjectTx(t, agg, "src-tx-R", "WORKER-1", t0.Add(5*time.Minute))
	mustProject(t, agg, EventPayload{
		LineID: "src-line-R", KioskCode: "KIOSK-A", TransactionID: "src-tx-R",
		ItemCode: "DRILL", UserCode: "WORKER-1",
		Action: "return", Qty: 1, CompletedAt: t0.Add(5 * time.Minute),
	})

	rows := listOpenCheckouts(t, app, "KIOSK-A")
	if len(rows) != 1 {
		t.Fatalf("after cross-user return: want 1 row (Bob's, untouched), got %d", len(rows))
	}
	if rows[0].GetString("user") != userIDByCode(t, app, "WORKER-2") {
		t.Errorf("surviving row: want Bob's, got user %s", rows[0].GetString("user"))
	}
}

// TestProjectOpenCheckouts_CloseIdempotentOnRedelivery confirms a duplicate
// return delivery (lost-Ack / AckWait-expiry redelivery) does NOT delete a
// second row. Alice has 2 drills out; a qty=1 return delivered twice must
// leave exactly 1 row.
func TestProjectOpenCheckouts_CloseIdempotentOnRedelivery(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "DRILL", "Drill")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	mustProjectTx(t, agg, "src-tx-A", "WORKER-1", t0)
	mustProject(t, agg, EventPayload{
		LineID: "src-line-A", KioskCode: "KIOSK-A", TransactionID: "src-tx-A",
		ItemCode: "DRILL", UserCode: "WORKER-1",
		Action: "checkout", Qty: 2, CompletedAt: t0,
	})
	if got := countOpenCheckouts(t, app, "KIOSK-A"); got != 2 {
		t.Fatalf("setup: want 2 rows, got %d", got)
	}

	ret := EventPayload{
		LineID: "src-line-R", KioskCode: "KIOSK-A", TransactionID: "src-tx-A",
		ItemCode: "DRILL", UserCode: "WORKER-1",
		Action: "return", Qty: 1, CompletedAt: t0.Add(time.Minute),
	}
	if out := agg.ProjectOpenCheckoutsClose(ret); out != projectAck {
		t.Fatalf("first close: %v", out)
	}
	// Redelivery of the same return line.
	if out := agg.ProjectOpenCheckoutsClose(ret); out != projectAck {
		t.Fatalf("redelivered close: %v", out)
	}
	if got := countOpenCheckouts(t, app, "KIOSK-A"); got != 1 {
		t.Errorf("after redelivered return: want 1 row (one close, not two), got %d", got)
	}
}

func TestProjectOpenCheckouts_SerializedExactMatch(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "DRILL", "Drill")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	mustProjectTx(t, agg, "src-tx-A", "WORKER-1", t0)
	mustProject(t, agg, EventPayload{
		LineID: "src-line-A", KioskCode: "KIOSK-A", TransactionID: "src-tx-A",
		ItemCode: "DRILL", UserCode: "WORKER-1",
		Action: "checkout", Qty: 1, ItemInstanceID: "inst-42",
		CompletedAt: t0,
	})

	if got := countOpenCheckouts(t, app, "KIOSK-A"); got != 1 {
		t.Fatalf("setup: want 1 row, got %d", got)
	}

	mustProjectTx(t, agg, "src-tx-R", "WORKER-1", t0.Add(5*time.Minute))
	mustProject(t, agg, EventPayload{
		LineID: "src-line-R", KioskCode: "KIOSK-A", TransactionID: "src-tx-R",
		ItemCode: "DRILL", UserCode: "WORKER-1",
		Action: "return", Qty: 1, ItemInstanceID: "inst-42",
		CompletedAt: t0.Add(5 * time.Minute),
	})

	if got := countOpenCheckouts(t, app, "KIOSK-A"); got != 0 {
		t.Fatalf("after serialized return: want 0 rows, got %d", got)
	}
}

func TestProjectOpenCheckouts_KioskScoped(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "DRILL", "Drill")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	// Same user, same item, two kiosks. Both checkouts should land; a
	// return against kiosk A must not touch kiosk B's row.
	for _, k := range []string{"KIOSK-A", "KIOSK-B"} {
		txID := "src-tx-" + k
		lineID := "src-line-" + k
		if out := agg.ProjectTransaction(EventPayload{
			TransactionID: txID, KioskCode: k, LocationCode: "WEST",
			UserCode: "WORKER-1",
			StartedAt: t0, CompletedAt: t0, LinesCount: 1,
		}); out != projectAck {
			t.Fatalf("ProjectTransaction %s: %v", k, out)
		}
		mustProject(t, agg, EventPayload{
			LineID: lineID, KioskCode: k, TransactionID: txID,
			ItemCode: "DRILL", UserCode: "WORKER-1",
			Action: "checkout", Qty: 1, CompletedAt: t0,
		})
	}
	if got := countOpenCheckouts(t, app, ""); got != 2 {
		t.Fatalf("setup: want 2 rows total, got %d", got)
	}

	// Return at kiosk A only.
	if out := agg.ProjectTransaction(EventPayload{
		TransactionID: "src-tx-R", KioskCode: "KIOSK-A", LocationCode: "WEST",
		UserCode: "WORKER-1",
		StartedAt: t0.Add(time.Minute), CompletedAt: t0.Add(time.Minute), LinesCount: 1,
	}); out != projectAck {
		t.Fatalf("return tx: %v", out)
	}
	mustProject(t, agg, EventPayload{
		LineID: "src-line-R", KioskCode: "KIOSK-A", TransactionID: "src-tx-R",
		ItemCode: "DRILL", UserCode: "WORKER-1",
		Action: "return", Qty: 1, CompletedAt: t0.Add(time.Minute),
	})

	if got := countOpenCheckouts(t, app, "KIOSK-A"); got != 0 {
		t.Errorf("KIOSK-A after return: want 0, got %d", got)
	}
	if got := countOpenCheckouts(t, app, "KIOSK-B"); got != 1 {
		t.Errorf("KIOSK-B (untouched): want 1, got %d", got)
	}
}

func TestProjectOpenCheckouts_AdminCloseDeletesRow(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "DRILL", "Drill")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	mustProjectTx(t, agg, "src-tx-A", "WORKER-1", t0)
	mustProject(t, agg, EventPayload{
		LineID: "src-line-A", KioskCode: "KIOSK-A", TransactionID: "src-tx-A",
		ItemCode: "DRILL", UserCode: "WORKER-1",
		Action: "checkout", Qty: 1, CompletedAt: t0,
	})

	if got := countOpenCheckouts(t, app, "KIOSK-A"); got != 1 {
		t.Fatalf("setup: want 1 row, got %d", got)
	}

	// checkout.admin_close payload — note: the action field is empty since
	// admin_close doesn't ride item.{action}. The projector keys on
	// (kiosk_code, item_code, user_code) for non-serialized. The admin_close
	// LINE id is the idempotency anchor (stable across live + republish).
	adminClose := EventPayload{
		KioskCode: "KIOSK-A", ItemCode: "DRILL", UserCode: "WORKER-1",
		LineID: "ac-line-1",
	}
	if out := agg.ProjectOpenCheckoutsAdminClose(adminClose); out != projectAck {
		t.Fatalf("ProjectOpenCheckoutsAdminClose: %v", out)
	}
	if got := countOpenCheckouts(t, app, "KIOSK-A"); got != 0 {
		t.Errorf("after admin_close: want 0, got %d", got)
	}

	// Redelivery must be a no-op (not re-close a now-unrelated row). Re-seed
	// a fresh checkout for the same user/item, then redeliver the same
	// admin_close: the guard must keep it from deleting the new row.
	mustProjectTx(t, agg, "src-tx-B", "WORKER-1", t0.Add(10*time.Minute))
	mustProject(t, agg, EventPayload{
		LineID: "src-line-B", KioskCode: "KIOSK-A", TransactionID: "src-tx-B",
		ItemCode: "DRILL", UserCode: "WORKER-1",
		Action: "checkout", Qty: 1, CompletedAt: t0.Add(10 * time.Minute),
	})
	if out := agg.ProjectOpenCheckoutsAdminClose(adminClose); out != projectAck {
		t.Fatalf("redelivered admin_close: %v", out)
	}
	if got := countOpenCheckouts(t, app, "KIOSK-A"); got != 1 {
		t.Errorf("redelivered admin_close must not touch the new row: want 1, got %d", got)
	}
}

func TestProjectOpenCheckouts_ConsumeIsNoOp(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "BOLT", "Bolt")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	mustProjectTx(t, agg, "src-tx-A", "WORKER-1", t0)
	mustProject(t, agg, EventPayload{
		LineID: "src-line-A", KioskCode: "KIOSK-A", TransactionID: "src-tx-A",
		ItemCode: "BOLT", UserCode: "WORKER-1",
		Action: "consume", Qty: 10, CompletedAt: t0,
	})

	if got := countOpenCheckouts(t, app, "KIOSK-A"); got != 0 {
		t.Errorf("consume must not create open_checkouts: got %d", got)
	}
}

// --- helpers ---

func mustProject(t *testing.T, agg *Aggregator, p EventPayload) {
	t.Helper()
	if out := agg.ProjectLine(p); out != projectAck {
		t.Fatalf("ProjectLine(%s): %v", p.Action, out)
	}
	if out := agg.projectOpenCheckoutsForItemAction(p); out != projectAck {
		t.Fatalf("projectOpenCheckoutsForItemAction(%s): %v", p.Action, out)
	}
}

func mustProjectTx(t *testing.T, agg *Aggregator, txID, userCode string, when time.Time) {
	t.Helper()
	if out := agg.ProjectTransaction(EventPayload{
		TransactionID: txID, KioskCode: "KIOSK-A", LocationCode: "WEST",
		UserCode: userCode,
		StartedAt: when, CompletedAt: when, LinesCount: 1,
	}); out != projectAck {
		t.Fatalf("ProjectTransaction(%s): %v", txID, out)
	}
}

func listOpenCheckouts(t *testing.T, app core.App, kioskCode string) []*core.Record {
	t.Helper()
	filter := ""
	params := dbx.Params{}
	if kioskCode != "" {
		filter = "kiosk_code = {:k}"
		params["k"] = kioskCode
	}
	rows, err := app.FindRecordsByFilter("open_checkouts", filter, "checked_out_at", 0, 0, params)
	if err != nil {
		t.Fatalf("list open_checkouts: %v", err)
	}
	return rows
}

func countOpenCheckouts(t *testing.T, app core.App, kioskCode string) int {
	return len(listOpenCheckouts(t, app, kioskCode))
}

func userIDByCode(t *testing.T, app core.App, code string) string {
	t.Helper()
	rec, err := app.FindFirstRecordByFilter("users", "code = {:c}", dbx.Params{"c": code})
	if err != nil {
		t.Fatalf("find user by code %q: %v", code, err)
	}
	return rec.Id
}
