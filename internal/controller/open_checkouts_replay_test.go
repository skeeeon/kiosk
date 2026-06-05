package controller

import (
	"reflect"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/ledger"
)

// These tests exercise the controller's "currently out" view the way it is
// actually computed now: project events into the transaction_lines ledger,
// then reconstruct open rows with ledger.ReplayOpenRows. They replace the old
// open_checkouts_projection_test.go (which asserted against a materialized
// table the controller no longer keeps). Replay is convergent by construction,
// so these double as the regression guard for the kiosk/controller divergence
// that motivated the change.

func mustProjectTx(t *testing.T, agg *Aggregator, txID, userCode, kioskCode string, when time.Time) {
	t.Helper()
	if out := agg.ProjectTransaction(EventPayload{
		TransactionID: txID, KioskCode: kioskCode, LocationCode: "WEST",
		UserCode: userCode, StartedAt: when, CompletedAt: when, LinesCount: 1,
	}); out != projectAck {
		t.Fatalf("ProjectTransaction(%s): %v", txID, out)
	}
}

func mustProjectLine(t *testing.T, agg *Aggregator, p EventPayload) {
	t.Helper()
	if out := agg.ProjectLine(p); out != projectAck {
		t.Fatalf("ProjectLine(%s): %v", p.Action, out)
	}
}

func replayRows(t *testing.T, app core.App, kioskCode string) []ledger.OpenRow {
	t.Helper()
	rows, err := ledger.ReplayOpenRows(app, kioskCode)
	if err != nil {
		t.Fatalf("ReplayOpenRows(%q): %v", kioskCode, err)
	}
	return rows
}

func countRowsByUser(rows []ledger.OpenRow, userID string) int {
	n := 0
	for _, r := range rows {
		if r.User == userID {
			n++
		}
	}
	return n
}

func TestControllerReplay_CheckoutReturnFIFOByTargetUser(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedUser(t, app, "WORKER-2", "Bob")
	seedItem(t, app, "DRILL", "Drill")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	// Alice checks out 2 drills (oldest); Bob 1 later.
	mustProjectTx(t, agg, "src-tx-A", "WORKER-1", "KIOSK-A", t0)
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-A", KioskCode: "KIOSK-A", TransactionID: "src-tx-A",
		ItemCode: "DRILL", UserCode: "WORKER-1", Action: "checkout", Qty: 2, CompletedAt: t0,
	})
	mustProjectTx(t, agg, "src-tx-B", "WORKER-2", "KIOSK-A", t0.Add(time.Minute))
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-B", KioskCode: "KIOSK-A", TransactionID: "src-tx-B",
		ItemCode: "DRILL", UserCode: "WORKER-2", Action: "checkout", Qty: 1, CompletedAt: t0.Add(time.Minute),
	})

	if got := len(replayRows(t, app, "KIOSK-A")); got != 3 {
		t.Fatalf("setup: want 3 rows, got %d", got)
	}

	// Alice returns 1 → one of HER rows goes; Bob's survives.
	mustProjectTx(t, agg, "src-tx-R", "WORKER-1", "KIOSK-A", t0.Add(5*time.Minute))
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-R", KioskCode: "KIOSK-A", TransactionID: "src-tx-R",
		ItemCode: "DRILL", UserCode: "WORKER-1", Action: "return", Qty: 1, CompletedAt: t0.Add(5 * time.Minute),
	})

	rows := replayRows(t, app, "KIOSK-A")
	if len(rows) != 2 {
		t.Fatalf("after Alice return: want 2 rows, got %d", len(rows))
	}
	aliceID := userIDByCode(t, app, "WORKER-1")
	bobID := userIDByCode(t, app, "WORKER-2")
	if a, b := countRowsByUser(rows, aliceID), countRowsByUser(rows, bobID); a != 1 || b != 1 {
		t.Errorf("after return: alice=%d bob=%d, want 1 each", a, b)
	}
}

func TestControllerReplay_NoCrossUserBorrow(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedUser(t, app, "WORKER-2", "Bob")
	seedItem(t, app, "DRILL", "Drill")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	// Only Bob has a drill out.
	mustProjectTx(t, agg, "src-tx-B", "WORKER-2", "KIOSK-A", t0)
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-B", KioskCode: "KIOSK-A", TransactionID: "src-tx-B",
		ItemCode: "DRILL", UserCode: "WORKER-2", Action: "checkout", Qty: 1, CompletedAt: t0,
	})

	// Alice returns a drill she doesn't have out: nothing of hers matches, and
	// replay must NOT borrow Bob's row (mirrors commit's uncorrelated handling).
	mustProjectTx(t, agg, "src-tx-R", "WORKER-1", "KIOSK-A", t0.Add(5*time.Minute))
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-R", KioskCode: "KIOSK-A", TransactionID: "src-tx-R",
		ItemCode: "DRILL", UserCode: "WORKER-1", Action: "return", Qty: 1, CompletedAt: t0.Add(5 * time.Minute),
	})

	rows := replayRows(t, app, "KIOSK-A")
	if len(rows) != 1 {
		t.Fatalf("after cross-user return: want 1 row (Bob's, untouched), got %d", len(rows))
	}
	if rows[0].User != userIDByCode(t, app, "WORKER-2") {
		t.Errorf("surviving row: want Bob's")
	}
}

func TestControllerReplay_SerializedReturn(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "DRILL", "Drill")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	// Serialized matching keys on the instance id, which must survive onto the
	// projected line (source_item_instance_id) for replay to pair the return.
	mustProjectTx(t, agg, "src-tx-A", "WORKER-1", "KIOSK-A", t0)
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-A", KioskCode: "KIOSK-A", TransactionID: "src-tx-A",
		ItemCode: "DRILL", UserCode: "WORKER-1", Action: "checkout", Qty: 1,
		ItemInstanceID: "inst-42", CompletedAt: t0,
	})
	if got := len(replayRows(t, app, "KIOSK-A")); got != 1 {
		t.Fatalf("setup: want 1 row, got %d", got)
	}

	mustProjectTx(t, agg, "src-tx-R", "WORKER-1", "KIOSK-A", t0.Add(5*time.Minute))
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-R", KioskCode: "KIOSK-A", TransactionID: "src-tx-R",
		ItemCode: "DRILL", UserCode: "WORKER-1", Action: "return", Qty: 1,
		ItemInstanceID: "inst-42", CompletedAt: t0.Add(5 * time.Minute),
	})
	if got := len(replayRows(t, app, "KIOSK-A")); got != 0 {
		t.Fatalf("after serialized return: want 0 rows, got %d", got)
	}
}

func TestControllerReplay_KioskScoped(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "DRILL", "Drill")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	// Same user + item out at two kiosks; a return at A must not touch B.
	for _, k := range []string{"KIOSK-A", "KIOSK-B"} {
		mustProjectTx(t, agg, "src-tx-"+k, "WORKER-1", k, t0)
		mustProjectLine(t, agg, EventPayload{
			LineID: "src-line-" + k, KioskCode: k, TransactionID: "src-tx-" + k,
			ItemCode: "DRILL", UserCode: "WORKER-1", Action: "checkout", Qty: 1, CompletedAt: t0,
		})
	}

	mustProjectTx(t, agg, "src-tx-R", "WORKER-1", "KIOSK-A", t0.Add(time.Minute))
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-R", KioskCode: "KIOSK-A", TransactionID: "src-tx-R",
		ItemCode: "DRILL", UserCode: "WORKER-1", Action: "return", Qty: 1, CompletedAt: t0.Add(time.Minute),
	})

	if got := len(replayRows(t, app, "KIOSK-A")); got != 0 {
		t.Errorf("KIOSK-A after return: want 0, got %d", got)
	}
	if got := len(replayRows(t, app, "KIOSK-B")); got != 1 {
		t.Errorf("KIOSK-B (untouched): want 1, got %d", got)
	}
}

func TestControllerReplay_FleetFanout(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "DRILL", "Drill")
	// The fan-out walks the kiosks registry, so the kiosks must be registered
	// for their open rows to surface — mirrors production, where TouchKiosk
	// registers a kiosk on its first transaction. (seedKiosk lives in
	// membership_test.go.)
	seedKiosk(t, app, "KIOSK-A", "WEST")
	seedKiosk(t, app, "KIOSK-B", "EAST")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	// 2 drills out at A, 1 at B.
	mustProjectTx(t, agg, "src-tx-A", "WORKER-1", "KIOSK-A", t0)
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-A", KioskCode: "KIOSK-A", TransactionID: "src-tx-A",
		ItemCode: "DRILL", UserCode: "WORKER-1", Action: "checkout", Qty: 2, CompletedAt: t0,
	})
	mustProjectTx(t, agg, "src-tx-B", "WORKER-1", "KIOSK-B", t0)
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-B", KioskCode: "KIOSK-B", TransactionID: "src-tx-B",
		ItemCode: "DRILL", UserCode: "WORKER-1", Action: "checkout", Qty: 1, CompletedAt: t0,
	})

	if a, b := len(replayRows(t, app, "KIOSK-A")), len(replayRows(t, app, "KIOSK-B")); a != 2 || b != 1 {
		t.Fatalf("per-kiosk replay: want A=2 B=1, got A=%d B=%d", a, b)
	}

	// Both kiosks are offline (empty heartbeat registry), so the NATS-first
	// gather falls back to replaying the controller's projected ledger for
	// each — nc is never touched. This is the offline-fallback path behind the
	// currently-out report/digest when a kiosk can't be reached live.
	h := &Handlers{App: app}
	rows, prov, err := h.gatherOpenCheckouts(nil, NewHeartbeatRegistry(nil), "")
	if err != nil {
		t.Fatalf("gatherOpenCheckouts: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("offline fallback: want 3 DTOs (2 A + 1 B), got %d", len(rows))
	}
	if len(prov.LiveKiosks) != 0 || len(prov.UnavailableKiosks) != 0 {
		t.Errorf("want nothing live/unavailable, got live=%v unavailable=%v",
			prov.LiveKiosks, prov.UnavailableKiosks)
	}
	if want := []string{"KIOSK-A", "KIOSK-B"}; !reflect.DeepEqual(prov.LastKnownKiosks, want) {
		t.Errorf("LastKnownKiosks = %v, want %v", prov.LastKnownKiosks, want)
	}
}

// TestReplayInstanceStatuses covers the maintenance digest's offline fallback:
// current status is the latest transition per instance in
// instance_lifecycle_audit, and only units whose latest status is maintenance
// are returned.
func TestReplayInstanceStatuses(t *testing.T) {
	app := setupApp(t)
	col, err := app.FindCollectionByNameOrId("instance_lifecycle_audit")
	if err != nil {
		t.Fatalf("find instance_lifecycle_audit: %v", err)
	}
	seedAudit := func(instanceID, instanceCode, action, newStatus string, occurred time.Time) {
		rec := core.NewRecord(col)
		rec.Set("kiosk_code", "KIOSK-A")
		rec.Set("instance_id", instanceID)
		rec.Set("instance_code", instanceCode)
		rec.Set("item_code", "DRILL")
		rec.Set("item_name", "Drill")
		rec.Set("action", action)
		rec.Set("new_status", newStatus)
		rec.Set("occurred_at", occurred)
		if serr := app.Save(rec); serr != nil {
			t.Fatalf("seed audit: %v", serr)
		}
	}
	t0 := time.Now().UTC()
	// inst-1: sent to maintenance, then returned to service → current in_service.
	seedAudit("inst-1", "A-1", "to_maintenance", "maintenance", t0)
	seedAudit("inst-1", "A-1", "return_to_service", "in_service", t0.Add(time.Hour))
	// inst-2: currently in maintenance.
	seedAudit("inst-2", "A-2", "to_maintenance", "maintenance", t0.Add(time.Minute))
	// A different kiosk's maintenance unit must not leak into KIOSK-A's set.
	recB := core.NewRecord(col)
	recB.Set("kiosk_code", "KIOSK-B")
	recB.Set("instance_id", "inst-9")
	recB.Set("instance_code", "B-9")
	recB.Set("action", "to_maintenance")
	recB.Set("new_status", "maintenance")
	recB.Set("occurred_at", t0)
	if serr := app.Save(recB); serr != nil {
		t.Fatalf("seed audit B: %v", serr)
	}

	rows, err := replayInstanceStatuses(app, "KIOSK-A")
	if err != nil {
		t.Fatalf("replayInstanceStatuses: %v", err)
	}
	if len(rows) != 1 || rows[0].InstanceCode != "A-2" {
		t.Fatalf("want only A-2 in maintenance, got %+v", rows)
	}
}

func TestControllerReplay_AdminCloseRemovesRowAndIsIdempotent(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "DRILL", "Drill")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	mustProjectTx(t, agg, "src-tx-A", "WORKER-1", "KIOSK-A", t0)
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-A", KioskCode: "KIOSK-A", TransactionID: "src-tx-A",
		ItemCode: "DRILL", UserCode: "WORKER-1", Action: "checkout", Qty: 1, CompletedAt: t0,
	})

	adminClose := EventPayload{
		KioskCode: "KIOSK-A", LocationCode: "WEST",
		TransactionID: "ac-tx-1", LineID: "ac-line-1",
		ItemCode: "DRILL", UserCode: "WORKER-1", CompletedAt: t0.Add(5 * time.Minute),
	}
	if out := agg.ProjectAdminCloseToLedger(adminClose); out != projectAck {
		t.Fatalf("ProjectAdminCloseToLedger: %v", out)
	}
	if got := len(replayRows(t, app, "KIOSK-A")); got != 0 {
		t.Fatalf("after admin_close: want 0 rows, got %d", got)
	}

	// A fresh checkout for the same user/item, then a redelivered admin_close:
	// the redelivery re-projects the same (already-present) tx+line — no second
	// admin_close lands — so the new checkout's row must survive.
	mustProjectTx(t, agg, "src-tx-C", "WORKER-1", "KIOSK-A", t0.Add(10*time.Minute))
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-C", KioskCode: "KIOSK-A", TransactionID: "src-tx-C",
		ItemCode: "DRILL", UserCode: "WORKER-1", Action: "checkout", Qty: 1, CompletedAt: t0.Add(10 * time.Minute),
	})
	if out := agg.ProjectAdminCloseToLedger(adminClose); out != projectAck {
		t.Fatalf("redelivered admin_close: %v", out)
	}
	if got := len(replayRows(t, app, "KIOSK-A")); got != 1 {
		t.Errorf("redelivered admin_close must not touch the new row: want 1, got %d", got)
	}
}

func TestControllerReplay_SerializedAdminClose(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "SPLICE", "Fiber Splicer")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()

	// The bug report's stuck case: a serialized item admin-closed on the kiosk
	// that stayed "out" on the controller. Replay must drop it via the line's
	// instance id.
	mustProjectTx(t, agg, "src-tx-A", "WORKER-1", "KIOSK-A", t0)
	mustProjectLine(t, agg, EventPayload{
		LineID: "src-line-A", KioskCode: "KIOSK-A", TransactionID: "src-tx-A",
		ItemCode: "SPLICE", UserCode: "WORKER-1", Action: "checkout", Qty: 1,
		Serial: "123456", ItemInstanceID: "inst-99", CompletedAt: t0,
	})
	if got := len(replayRows(t, app, "KIOSK-A")); got != 1 {
		t.Fatalf("setup: want 1 row, got %d", got)
	}

	if out := agg.ProjectAdminCloseToLedger(EventPayload{
		KioskCode: "KIOSK-A", LocationCode: "WEST",
		TransactionID: "ac-tx-1", LineID: "ac-line-1",
		ItemCode: "SPLICE", UserCode: "WORKER-1",
		Serial: "123456", ItemInstanceID: "inst-99", CompletedAt: t0.Add(5 * time.Minute),
	}); out != projectAck {
		t.Fatalf("serialized admin_close: %v", out)
	}
	if got := len(replayRows(t, app, "KIOSK-A")); got != 0 {
		t.Errorf("after serialized admin_close: want 0 rows, got %d", got)
	}
}
