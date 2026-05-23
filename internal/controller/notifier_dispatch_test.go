package controller

import (
	"testing"
	"time"

	"github.com/pocketbase/dbx"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// TestControllerNotifier_TemplatesSeeded sanity-checks that the kiosk
// migrations registered via init() side-effects also populate the
// controller's notification_templates collection. If this regresses, the
// downstream send paths would silently fail to find a template — easier
// to catch it here than in production logs.
func TestControllerNotifier_TemplatesSeeded(t *testing.T) {
	app := setupApp(t)

	rows, err := app.FindRecordsByFilter(notifications.CollectionName, "", "event_type", 0, 0)
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("want at least the receipt + lowstock templates seeded, got %d", len(rows))
	}

	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.GetString("event_type")] = true
	}
	for _, want := range []string{
		notifications.EventTypeReceiptTransaction,
		notifications.EventTypeLowStock,
	} {
		if !seen[want] {
			t.Errorf("missing seeded template for %q", want)
		}
	}
}

// TestControllerNotifier_ReceiptDedupes drives a receipt context through
// the SendIfFirst path the aggregator uses internally and verifies a
// notification_dedupe row lands. SendIfFirst spawns a goroutine, so we
// poll briefly rather than asserting synchronously.
//
// We don't try to assert on the send_log here — that depends on the SMTP
// transport actually returning (success or failure) which is too flaky
// for a smoke test. The dedupe row is the controller's idempotency
// anchor and is what protects against JetStream redelivery storms; if
// it gets written, the wiring is correct.
func TestControllerNotifier_ReceiptDedupes(t *testing.T) {
	app := setupApp(t)
	notifier := notifications.New(app)

	ctx := notifications.ReceiptContext{
		Kiosk: notifications.KioskInfo{Code: "BAY-01", LocationCode: "WAREHOUSE-A"},
		User:  notifications.UserInfo{ID: "u1", Code: "W1", Name: "Alex", Email: "alex@example.com"},
		Transaction: notifications.TransactionInfo{
			ID:          "tx-smoke-1",
			StartedAt:   time.Now().Add(-time.Minute),
			CompletedAt: time.Now(),
			LinesCount:  1,
		},
		Lines: []notifications.LineInfo{
			{ItemCode: "ITM-1", ItemName: "Widget", Action: "consume", Qty: 1},
		},
	}

	notifier.SendIfFirst(notifications.EventTypeReceiptTransaction, ctx.Transaction.ID, ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec, _ := app.FindFirstRecordByFilter(
			notifications.DedupeCollectionName,
			"event_type = {:e} && ref = {:r}",
			dbx.Params{"e": notifications.EventTypeReceiptTransaction, "r": ctx.Transaction.ID},
		)
		if rec != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("dedupe row never appeared for tx-smoke-1 — controller notifier wiring is broken")
}

// TestControllerNotifier_DigestEnvelope drives the synchronous SendTo path
// that handleOpenChecksDigest uses, with a per-schedule recipients
// override carried in the wire envelope. Verifies the controller's
// notification_send_log captures the attempt — the SMTP send itself will
// fail in the test (no real mail server), but the row written with
// status=failed proves the dispatch + render + recipients-override flow
// is intact. SendTo is synchronous so no polling needed.
func TestControllerNotifier_DigestEnvelope(t *testing.T) {
	app := setupApp(t)
	notifier := notifications.New(app)

	env := notifications.DigestEnvelope{
		Context: notifications.OpenChecksDigestContext{
			Kiosk:       notifications.KioskInfo{Code: "BAY-01", LocationCode: "WEST"},
			GeneratedAt: time.Now().UTC(),
			Rows:        []notifications.OpenChecksDigestRow{},
			RowsCount:   0,
		},
		// Per-schedule override — not the template's stored recipients.
		// The controller must honor this list, otherwise per-schedule
		// audiences wouldn't work in managed mode.
		Recipients: notifications.Recipients{Extras: []string{"ops@example.com"}},
	}

	// SendTo is what handleOpenChecksDigest calls internally. Calling it
	// directly here lets the test assert on the log row without spinning
	// up a real JetStream consumer.
	_ = notifier.SendTo(notifications.EventTypeOpenChecksDigest, env.Context, env.Recipients)

	rows, err := app.FindRecordsByFilter(
		notifications.SendLogCollectionName,
		"event_type = {:e}",
		"",
		10, 0,
		dbx.Params{"e": notifications.EventTypeOpenChecksDigest},
	)
	if err != nil {
		t.Fatalf("list send log: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("expected a notification_send_log row for digest dispatch, got none")
	}
	if got := rows[0].GetString("recipient"); got != "ops@example.com" {
		t.Errorf("recipient: got %q, want %q (per-schedule override should win over template)", got, "ops@example.com")
	}
}
