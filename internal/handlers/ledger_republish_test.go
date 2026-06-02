package handlers

import (
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/commit"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// capturedEvent holds one (subject, payload) pair from a republish. The
// payload is the same map we'd JSON-encode, so tests can probe individual
// fields without round-tripping through bytes.
type capturedEvent struct {
	Subject string
	Payload map[string]any
}

type captureSink struct {
	events []capturedEvent
}

func (c *captureSink) publish(subject string, payload any) {
	m, _ := payload.(map[string]any)
	c.events = append(c.events, capturedEvent{Subject: subject, Payload: m})
}

// seedLedger commits one checkout + one consume against a fresh app and
// returns the resulting transaction IDs in commit order. Used as the
// substrate for the republish tests below.
func seedLedger(t *testing.T, app core.App) (string, string) {
	t.Helper()

	users, _ := app.FindCollectionByNameOrId("users")
	alice := core.NewRecord(users)
	alice.Set("email", "alice@test.local")
	alice.Set("name", "Alice")
	alice.Set("code", "EMP-A")
	alice.Set("role", "worker")
	alice.Set("active", true)
	alice.SetPassword("password-aaaaaaaaaaaa")
	if err := app.Save(alice); err != nil {
		t.Fatalf("save alice: %v", err)
	}

	items, _ := app.FindCollectionByNameOrId("items")
	hammer := core.NewRecord(items)
	hammer.Set("code", "HAMMER")
	hammer.Set("name", "Hammer")
	hammer.Set("type", "tool")
	hammer.Set("tracking_mode", "quantity")
	hammer.Set("active", true)
	if err := app.Save(hammer); err != nil {
		t.Fatalf("save hammer: %v", err)
	}
	screws := core.NewRecord(items)
	screws.Set("code", "SCREW-3IN")
	screws.Set("name", "Deck Screws")
	screws.Set("type", "consumable")
	screws.Set("tracking_mode", "quantity")
	screws.Set("active", true)
	if err := app.Save(screws); err != nil {
		t.Fatalf("save screws: %v", err)
	}

	id := kioskctx.Identity{KioskCode: "KIOSK-A", LocationCode: "WEST"}
	noop := func(string, any) {}

	tx1, err := commit.Commit(app, &cart.Cart{
		ID: "c1", UserID: alice.Id, UserCode: "EMP-A", UserName: "Alice",
		Lines: []*cart.Line{{
			ItemID: hammer.Id, ItemCode: "HAMMER", ItemName: "Hammer",
			ItemType: "tool", TrackingMode: "quantity",
			Action: "checkout", Qty: 1,
		}},
	}, id, commit.DefaultPolicy(), noop)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	tx2, err := commit.Commit(app, &cart.Cart{
		ID: "c2", UserID: alice.Id, UserCode: "EMP-A", UserName: "Alice",
		Lines: []*cart.Line{{
			ItemID: screws.Id, ItemCode: "SCREW-3IN", ItemName: "Deck Screws",
			ItemType: "consumable", TrackingMode: "quantity",
			Action: "consume", Qty: 5,
		}},
	}, id, commit.DefaultPolicy(), noop)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}

	return tx1.TransactionID, tx2.TransactionID
}

// TestRepublishLedger_EmitsAllCompletedTransactions confirms the walk
// touches every completed transaction, emits one transaction.complete plus
// one item.{action} per line, and uses the transaction's stored kiosk_code
// for subject building.
func TestRepublishLedger_EmitsAllCompletedTransactions(t *testing.T) {
	app := setupAppInternal(t)
	tx1ID, tx2ID := seedLedger(t, app)

	sink := &captureSink{}
	out, err := republishLedger(app, "status = 'completed'", dbx.Params{}, sink.publish)
	if err != nil {
		t.Fatalf("republishLedger: %v", err)
	}
	if out.TransactionsPublished != 2 {
		t.Errorf("transactions: want 2, got %d", out.TransactionsPublished)
	}
	if out.LinesPublished != 2 {
		t.Errorf("lines: want 2, got %d", out.LinesPublished)
	}

	// Expected events, in walk order (completed_at ASC):
	//   1. kiosk.KIOSK-A.event.transaction.complete  (tx1)
	//   2. kiosk.KIOSK-A.event.item.checkout         (tx1's line)
	//   3. kiosk.KIOSK-A.event.transaction.complete  (tx2)
	//   4. kiosk.KIOSK-A.event.item.consume          (tx2's line)
	if len(sink.events) != 4 {
		t.Fatalf("events: want 4, got %d", len(sink.events))
	}
	if sink.events[0].Subject != "kiosk.KIOSK-A.event.transaction.complete" {
		t.Errorf("event 0 subject: %s", sink.events[0].Subject)
	}
	if sink.events[1].Subject != "kiosk.KIOSK-A.event.item.checkout" {
		t.Errorf("event 1 subject: %s", sink.events[1].Subject)
	}
	if sink.events[3].Subject != "kiosk.KIOSK-A.event.item.consume" {
		t.Errorf("event 3 subject: %s", sink.events[3].Subject)
	}

	// The aggregator's idempotency key (source_kiosk_code,
	// source_transaction_id) must be carried by both complete and line
	// events so a replay reaches the same projection.
	if sink.events[0].Payload["transaction_id"] != tx1ID {
		t.Errorf("event 0 transaction_id: got %v, want %s",
			sink.events[0].Payload["transaction_id"], tx1ID)
	}
	if sink.events[2].Payload["transaction_id"] != tx2ID {
		t.Errorf("event 2 transaction_id: got %v, want %s",
			sink.events[2].Payload["transaction_id"], tx2ID)
	}
	if sink.events[1].Payload["item_code"] != "HAMMER" {
		t.Errorf("checkout line item_code: %v", sink.events[1].Payload["item_code"])
	}
	if sink.events[3].Payload["qty"] != 5 {
		t.Errorf("consume line qty: %v", sink.events[3].Payload["qty"])
	}
}

// TestRepublishLedger_AdminCloseEmitsCheckoutAdminClose is the regression for
// the republish/admin_close fix: an admin force-close must be re-emitted as a
// single checkout.admin_close event (the live shape) — NOT transaction.complete
// + item.admin_close. Emitting the regular pair would re-project the closed
// checkout as still-open on the controller (item.admin_close is a no-op for
// the open_checkouts projector) and add ledger rows the live path never sends.
func TestRepublishLedger_AdminCloseEmitsCheckoutAdminClose(t *testing.T) {
	app := setupAppInternal(t)

	admins, _ := app.FindCollectionByNameOrId("admins")
	admin := core.NewRecord(admins)
	admin.Set("email", "admin@test.local")
	admin.Set("name", "Admin")
	admin.SetPassword("password-adminadmin")
	if err := app.Save(admin); err != nil {
		t.Fatalf("save admin: %v", err)
	}

	users, _ := app.FindCollectionByNameOrId("users")
	worker := core.NewRecord(users)
	worker.Set("email", "w@test.local")
	worker.Set("name", "Worker")
	worker.Set("code", "EMP-W")
	worker.Set("role", "worker")
	worker.Set("active", true)
	worker.SetPassword("password-wwwwwwwwwwww")
	if err := app.Save(worker); err != nil {
		t.Fatalf("save worker: %v", err)
	}

	items, _ := app.FindCollectionByNameOrId("items")
	hammer := core.NewRecord(items)
	hammer.Set("code", "HAMMER")
	hammer.Set("name", "Hammer")
	hammer.Set("type", "tool")
	hammer.Set("tracking_mode", "quantity")
	hammer.Set("active", true)
	if err := app.Save(hammer); err != nil {
		t.Fatalf("save hammer: %v", err)
	}

	id := kioskctx.Identity{KioskCode: "KIOSK-A", LocationCode: "WEST"}
	noop := func(string, any) {}

	if _, err := commit.Commit(app, &cart.Cart{
		ID: "c1", UserID: worker.Id, UserCode: "EMP-W", UserName: "Worker",
		Lines: []*cart.Line{{
			ItemID: hammer.Id, ItemCode: "HAMMER", ItemName: "Hammer",
			ItemType: "tool", TrackingMode: "quantity", Action: "checkout", Qty: 1,
		}},
	}, id, commit.DefaultPolicy(), noop); err != nil {
		t.Fatalf("checkout: %v", err)
	}

	openRows, err := app.FindRecordsByFilter("open_checkouts", "", "", 1, 0)
	if err != nil || len(openRows) != 1 {
		t.Fatalf("expected 1 open row, got %d (err %v)", len(openRows), err)
	}
	if _, err := commit.AdminClose(app, commit.AdminCloseInput{
		OpenCheckoutID: openRows[0].Id,
		ActorID:        admin.Id,
		Source:         "local",
		Reason:         "returned_offline",
		Identity:       id,
	}, noop); err != nil {
		t.Fatalf("admin close: %v", err)
	}

	sink := &captureSink{}
	out, err := republishLedger(app, "status = 'completed'", dbx.Params{}, sink.publish)
	if err != nil {
		t.Fatalf("republishLedger: %v", err)
	}

	// Only the checkout transaction takes the regular path; the admin-close
	// transaction is counted separately and emits no transaction.complete.
	if out.TransactionsPublished != 1 {
		t.Errorf("transactions_published: want 1 (checkout only), got %d", out.TransactionsPublished)
	}
	if out.AdminClosesPublished != 1 {
		t.Errorf("admin_closes_published: want 1, got %d", out.AdminClosesPublished)
	}

	completes, itemAdminCloses, adminCloses := 0, 0, 0
	var adminCloseEv capturedEvent
	for _, ev := range sink.events {
		switch ev.Subject {
		case "kiosk.KIOSK-A.event.transaction.complete":
			completes++
		case "kiosk.KIOSK-A.event.item.admin_close":
			itemAdminCloses++
		case "kiosk.KIOSK-A.event.checkout.admin_close":
			adminCloses++
			adminCloseEv = ev
		}
	}
	if completes != 1 {
		t.Errorf("transaction.complete events: want 1 (checkout only), got %d", completes)
	}
	if itemAdminCloses != 0 {
		t.Errorf("republish must NOT emit item.admin_close: got %d", itemAdminCloses)
	}
	if adminCloses != 1 {
		t.Fatalf("want exactly 1 checkout.admin_close, got %d", adminCloses)
	}

	// The event must carry the line id (the controller's idempotency anchor)
	// and the holder's identity so ProjectAdminCloseToLedger can project the
	// admin_close line for the right holder.
	if lid, _ := adminCloseEv.Payload["line_id"].(string); lid == "" {
		t.Error("checkout.admin_close must carry a non-empty line_id")
	}
	if adminCloseEv.Payload["item_code"] != "HAMMER" {
		t.Errorf("admin_close item_code: %v", adminCloseEv.Payload["item_code"])
	}
	if adminCloseEv.Payload["user_code"] != "EMP-W" {
		t.Errorf("admin_close user_code: %v", adminCloseEv.Payload["user_code"])
	}
}

// TestRepublishLedger_FromToFilter confirms the date-range scoping. Useful
// when only the last day's worth of events is suspect and replaying
// everything would be wasteful.
func TestRepublishLedger_FromToFilter(t *testing.T) {
	app := setupAppInternal(t)
	_, _ = seedLedger(t, app)

	// Move tx2's completed_at well into the past so a from-filter can
	// exclude it. Direct DAO edit (not via commit) is fine here — we're
	// just adjusting a timestamp to set up the test scenario.
	txs, err := app.FindRecordsByFilter("transactions",
		"", "completed_at", 0, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(txs) != 2 {
		t.Fatalf("seed: want 2 txs, got %d", len(txs))
	}
	older := txs[0]
	older.Set("completed_at", time.Now().UTC().Add(-48*time.Hour))
	if err := app.Save(older); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Filter to "last 24h" — should pick up only the newer tx.
	from := time.Now().UTC().Add(-24 * time.Hour)
	sink := &captureSink{}
	out, err := republishLedger(app,
		"status = 'completed' && completed_at >= {:from}",
		dbx.Params{"from": from},
		sink.publish)
	if err != nil {
		t.Fatalf("republishLedger: %v", err)
	}
	if out.TransactionsPublished != 1 {
		t.Errorf("transactions in window: want 1, got %d", out.TransactionsPublished)
	}
}

// TestRepublishLedger_Idempotent confirms calling republish twice emits
// the same event shape both times — the kiosk side just walks the ledger;
// dedup happens on the controller via source_transaction_id.
func TestRepublishLedger_Idempotent(t *testing.T) {
	app := setupAppInternal(t)
	seedLedger(t, app)

	first := &captureSink{}
	if _, err := republishLedger(app, "status = 'completed'", dbx.Params{}, first.publish); err != nil {
		t.Fatalf("first run: %v", err)
	}
	second := &captureSink{}
	if _, err := republishLedger(app, "status = 'completed'", dbx.Params{}, second.publish); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(first.events) != len(second.events) {
		t.Fatalf("event count: first=%d second=%d", len(first.events), len(second.events))
	}
	for i := range first.events {
		if first.events[i].Subject != second.events[i].Subject {
			t.Errorf("event %d subject diverged: first=%s second=%s",
				i, first.events[i].Subject, second.events[i].Subject)
		}
		if first.events[i].Payload["transaction_id"] != second.events[i].Payload["transaction_id"] {
			t.Errorf("event %d tx id diverged", i)
		}
	}
}
