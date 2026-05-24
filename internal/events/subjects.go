package events

import (
	"fmt"
	"sync/atomic"
)

// Default protocol identifiers. The subject prefix is the leading namespace
// of every event published from a kiosk; the stream name is the JetStream
// stream the controller binds to. Override via cfg.NATS only when sharing a
// NATS cluster with another application that already owns these names —
// otherwise the defaults are correct and matched on both sides.
const (
	DefaultSubjectPrefix = "kiosk"
	DefaultStreamName    = "KIOSK_EVENTS"
)

// subjectPrefix is process-global because every commit-path callsite builds a
// subject without a config handle in scope. Set once at startup from
// cfg.NATS.SubjectPrefix; reads are atomic. Empty/unset falls back to
// DefaultSubjectPrefix so tests that don't load config still see the
// "kiosk." namespace.
var subjectPrefix atomic.Value // string

// SetSubjectPrefix installs the prefix used by all subject builders below.
// Empty input falls back to DefaultSubjectPrefix.
func SetSubjectPrefix(p string) {
	if p == "" {
		p = DefaultSubjectPrefix
	}
	subjectPrefix.Store(p)
}

// SubjectPrefix returns the currently installed prefix, or the default if
// SetSubjectPrefix has not been called.
func SubjectPrefix() string {
	v := subjectPrefix.Load()
	if v == nil {
		return DefaultSubjectPrefix
	}
	return v.(string)
}

// TransactionCompleteSubject builds the subject for a transaction-complete
// event from a specific kiosk: "<prefix>.<kiosk_code>.transaction.complete".
func TransactionCompleteSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.transaction.complete", SubjectPrefix(), kioskCode)
}

// ItemActionSubject builds the subject for a single line's action event:
// "<prefix>.<kiosk_code>.item.<action>".
func ItemActionSubject(kioskCode, action string) string {
	return fmt.Sprintf("%s.%s.item.%s", SubjectPrefix(), kioskCode, action)
}

// InventoryAdjustSubject builds the subject for an admin stock-adjustment
// event: "<prefix>.<kiosk_code>.inventory.adjust". One event per accepted
// adjustment, emitted after the audit row commits.
func InventoryAdjustSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.inventory.adjust", SubjectPrefix(), kioskCode)
}

// IntegrityRebuildSubject builds the subject for the destructive
// open_checkouts rebuild event: "<prefix>.<kiosk_code>.integrity.rebuild".
// Low-frequency, high-signal — used for operator audit and (eventually)
// surfacing rebuild events in the controller's admin UI.
func IntegrityRebuildSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.integrity.rebuild", SubjectPrefix(), kioskCode)
}

// AdminCloseSubject builds the subject for an admin-initiated open_checkouts
// close event: "<prefix>.<kiosk_code>.checkout.admin_close". One event per
// closed row, emitted after the transaction+line+open-row delete commits.
// Distinct from item.return so reports can separate "worker returned it" from
// "admin closed it without a return" without a discriminator filter.
func AdminCloseSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.checkout.admin_close", SubjectPrefix(), kioskCode)
}

// InstanceLifecycleSubject builds the subject for an item_instances lifecycle
// event: "<prefix>.<kiosk_code>.instance.lifecycle". Fired by the PB record
// hooks on create, decommission (active true→false), reactivate
// (active false→true), and delete. Cosmetic edits don't publish.
func InstanceLifecycleSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.instance.lifecycle", SubjectPrefix(), kioskCode)
}

// ReceiptTransactionSubject builds the subject for a "render and send the
// transaction receipt" notification event: "<prefix>.<kiosk_code>.receipt.transaction".
// Payload is a notifications.ReceiptContext serialized as JSON. The
// controller's notifier consumes it, looks up the template, and sends. This
// is the managed-mode counterpart to the kiosk's local Notifier.Send call —
// the kiosk publishes structured context, the controller does the rendering
// and SMTP.
func ReceiptTransactionSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.receipt.transaction", SubjectPrefix(), kioskCode)
}

// LowStockAlertSubject builds the subject for a low-stock alert notification:
// "<prefix>.<kiosk_code>.alert.lowstock". Payload is a notifications.LowStockContext
// serialized as JSON. The kiosk owns the threshold-crossing detection (it
// has the inventory) and publishes only on a real cross; the controller
// renders + sends.
func LowStockAlertSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.alert.lowstock", SubjectPrefix(), kioskCode)
}

// OpenChecksDigestSubject builds the subject for a scheduled-digest event:
// "<prefix>.<kiosk_code>.digest.open_checkouts". Payload is a
// notifications.DigestEnvelope that wraps the OpenChecksDigestContext
// computed locally on the kiosk and the per-schedule recipients spec. The
// kiosk owns the cron timing and the open_checkouts replay (controller
// doesn't project open_checkouts); the controller renders + SMTPs +
// logs to its own send_log. Schedule rows themselves remain kiosk-local.
func OpenChecksDigestSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.digest.open_checkouts", SubjectPrefix(), kioskCode)
}

// StreamSubjectFilter is the catch-all subject pattern the controller's
// stream binds to ("<prefix>.>") so every per-kiosk subject lands without
// per-kiosk wiring.
func StreamSubjectFilter() string {
	return SubjectPrefix() + ".>"
}

// TransactionCompleteFilter, ItemActionFilter, InventoryAdjustFilter, and
// IntegrityRebuildFilter are the consumer-side FilterSubjects that select
// only the event shapes the aggregator dispatches. inventory.adjust and
// integrity.rebuild are accepted (and ack'd) by the consumer today but not
// yet projected into ledger state — they reach the controller for future
// reporting hooks and as an audit signal in slog.
func TransactionCompleteFilter() string {
	return SubjectPrefix() + ".*.transaction.complete"
}

func ItemActionFilter() string {
	return SubjectPrefix() + ".*.item.*"
}

func InventoryAdjustFilter() string {
	return SubjectPrefix() + ".*.inventory.adjust"
}

func IntegrityRebuildFilter() string {
	return SubjectPrefix() + ".*.integrity.rebuild"
}

// AdminCloseFilter is the controller-side consumer filter for the admin
// force-close subject.
func AdminCloseFilter() string {
	return SubjectPrefix() + ".*.checkout.admin_close"
}

// InstanceLifecycleFilter is the controller-side consumer filter for the
// item_instances lifecycle subject.
func InstanceLifecycleFilter() string {
	return SubjectPrefix() + ".*.instance.lifecycle"
}

// ReceiptTransactionFilter is the controller-side consumer filter for the
// receipt notification subject.
func ReceiptTransactionFilter() string {
	return SubjectPrefix() + ".*.receipt.transaction"
}

// LowStockAlertFilter is the controller-side consumer filter for the
// low-stock notification subject.
func LowStockAlertFilter() string {
	return SubjectPrefix() + ".*.alert.lowstock"
}

// OpenChecksDigestFilter is the controller-side consumer filter for the
// scheduled open-checkouts digest subject.
func OpenChecksDigestFilter() string {
	return SubjectPrefix() + ".*.digest.open_checkouts"
}

// CommandSubject builds the subject for a controller→kiosk command targeting
// a specific kiosk: "<prefix>.<kiosk_code>.command.<name>". Commands ride
// core NATS request/reply, not JetStream — they are synchronous, single
// attempt, and should fail fast when the kiosk is offline rather than queue
// indefinitely. The kiosk replies on msg.Reply with a JSON outcome.
func CommandSubject(kioskCode, name string) string {
	return fmt.Sprintf("%s.%s.command.%s", SubjectPrefix(), kioskCode, name)
}

// CommandSubscribePattern is the subscription pattern a kiosk uses to receive
// all commands addressed to it. New command names are absorbed by the same
// subscription — the kiosk's dispatcher routes on the suffix.
func CommandSubscribePattern(kioskCode string) string {
	return fmt.Sprintf("%s.%s.command.>", SubjectPrefix(), kioskCode)
}

// InventoryAdjustCommandSubject is the controller→kiosk command that mutates
// the kiosk's items.quantity_on_hand. Idempotent via command_id; same
// underlying business logic as the local /api/kiosk/items/{id}/adjust path.
func InventoryAdjustCommandSubject(kioskCode string) string {
	return CommandSubject(kioskCode, "inventory.adjust")
}

// InventorySnapshotCommandSubject is the controller→kiosk read-only command
// that returns the kiosk's current on-hand quantities for one or more items.
// Used by the controller SPA's inventory panel to display live values
// before/after adjustments.
func InventorySnapshotCommandSubject(kioskCode string) string {
	return CommandSubject(kioskCode, "inventory.snapshot")
}

// HeartbeatSubject is the subject a kiosk publishes a periodic liveness
// beacon on. Core NATS publish (no JetStream) — last-write-wins, no
// persistence. The controller subscribes plainly and tracks the most recent
// beat per kiosk in memory.
func HeartbeatSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.heartbeat", SubjectPrefix(), kioskCode)
}

// HeartbeatFilter is the controller-side subscription pattern over every
// kiosk's heartbeat subject.
func HeartbeatFilter() string {
	return SubjectPrefix() + ".*.heartbeat"
}
