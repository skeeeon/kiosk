package controller

import (
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// Notification dispatch handlers for the aggregator's consume loop. Kept in
// their own file so the projection-heavy consumer.go stays focused on
// ledger work.
//
// Both event subjects carry a JSON-serialized notifications.* context that
// the kiosk built locally and published over JetStream. The controller's
// only job is template lookup + render + SMTP, against its own admin-edited
// rows — the kiosk side already gathered the data because it would have
// rendered locally in standalone mode.
//
// Dedup uses the existing notification_dedupe (event_type, ref, day) table:
//   - Receipts: ref = transaction_id. Day-scoped dedup means a redelivery
//     more than 24h after the first send could resend; in practice
//     JetStream redelivers within seconds of an unack'd message and we
//     treat that edge case as acceptable.
//   - Low-stock: ref = item_id. Day-scoped is the SEMANTIC dedup model for
//     this event — "tell me once per day per item it's low" — not just an
//     idempotency guard.

// handleReceiptTransaction renders + sends a transaction receipt against
// the controller's notification_templates row for receipt.transaction.
// Acks unconditionally: the dedupe row inside SendIfFirst owns retry
// suppression, and rendering/SMTP failures are captured in the send log
// for operator triage rather than driving JetStream redelivery.
func (a *Aggregator) handleReceiptTransaction(msg jetstream.Msg) {
	var ctx notifications.ReceiptContext
	if err := unmarshalMsg(msg, &ctx); err != nil {
		slog.Warn("controller.notifier.receipt_bad_payload",
			"subject", msg.Subject(), "error", err)
		_ = msg.Term()
		return
	}
	if ctx.Transaction.ID == "" || ctx.Kiosk.Code == "" {
		slog.Warn("controller.notifier.receipt_missing_fields",
			"subject", msg.Subject(),
			"transaction_id", ctx.Transaction.ID,
			"kiosk_code", ctx.Kiosk.Code)
		_ = msg.Term()
		return
	}
	if a.notifier == nil {
		// Stream is draining without an active notifier (test env or
		// misconfigured deploy). Ack so the message doesn't redeliver
		// forever; admins see the missing email in their inbox, not
		// JetStream backlog.
		_ = msg.Ack()
		return
	}
	a.notifier.SendIfFirst(notifications.EventTypeReceiptTransaction, ctx.Transaction.ID, ctx)
	_ = msg.Ack()
}

// handleOpenChecksDigest renders + sends a scheduled open-checkouts digest
// using the per-schedule recipients spec embedded in the envelope. The
// kiosk owns the cron timing and the ledger replay; the controller only
// renders + SMTPs + logs.
//
// Synchronous SendTo (not SendIfFirst) because digests are intentionally
// repeating events: daily/weekly schedules SHOULD fire each cadence, so
// any dedupe gate would break the semantics. JetStream redelivery is the
// only re-send risk and is rare; if it bites, add a refKey derived from
// {schedule_id, scheduled_run_time} in a follow-up.
func (a *Aggregator) handleOpenChecksDigest(msg jetstream.Msg) {
	var env notifications.DigestEnvelope
	if err := unmarshalMsg(msg, &env); err != nil {
		slog.Warn("controller.notifier.digest_bad_payload",
			"subject", msg.Subject(), "error", err)
		_ = msg.Term()
		return
	}
	if env.Context.Kiosk.Code == "" {
		slog.Warn("controller.notifier.digest_missing_fields",
			"subject", msg.Subject())
		_ = msg.Term()
		return
	}
	if a.notifier == nil {
		_ = msg.Ack()
		return
	}
	if err := a.notifier.SendTo(notifications.EventTypeOpenChecksDigest, env.Context, env.Recipients); err != nil {
		slog.Warn("controller.notifier.digest_send_failed",
			"kiosk_code", env.Context.Kiosk.Code, "error", err)
	}
	_ = msg.Ack()
}

// handleLowStockAlert renders + sends a low-stock alert. The kiosk owns
// the threshold-crossing detection and only publishes on an actual cross,
// so the controller's job is purely render + send + log.
func (a *Aggregator) handleLowStockAlert(msg jetstream.Msg) {
	var ctx notifications.LowStockContext
	if err := unmarshalMsg(msg, &ctx); err != nil {
		slog.Warn("controller.notifier.lowstock_bad_payload",
			"subject", msg.Subject(), "error", err)
		_ = msg.Term()
		return
	}
	if ctx.Item.ID == "" || ctx.Kiosk.Code == "" {
		slog.Warn("controller.notifier.lowstock_missing_fields",
			"subject", msg.Subject(),
			"item_id", ctx.Item.ID,
			"kiosk_code", ctx.Kiosk.Code)
		_ = msg.Term()
		return
	}
	if a.notifier == nil {
		_ = msg.Ack()
		return
	}
	a.notifier.SendIfFirst(notifications.EventTypeLowStock, ctx.Item.ID, ctx)
	_ = msg.Ack()
}
