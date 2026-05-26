package events

import (
	"fmt"
	"sync/atomic"
)

// Subject namespace.
//
// Every NATS subject this codebase publishes or subscribes to lives under
// "<prefix>.<kiosk_code>.<family>.<...>" where the family is one of:
//
//   event.<...>     kiosk -> controller, durable, JetStream
//   command.<...>   controller -> kiosk, core NATS request/reply
//   heartbeat       kiosk -> world, core NATS pub/sub, last-write-wins
//
// The family segment is what makes "is this in the stream?" answerable
// without thinking: the stream binds to "<prefix>.*.event.>" and that's it.
// Commands and heartbeats are outside the stream's filter space by
// construction, not by exclusion-list discipline.
//
// Wire reference: docs/wire.md — payload shapes per subject.
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
// event from a specific kiosk:
// "<prefix>.<kiosk_code>.event.transaction.complete".
func TransactionCompleteSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.event.transaction.complete", SubjectPrefix(), kioskCode)
}

// ItemActionSubject builds the subject for a single line's action event:
// "<prefix>.<kiosk_code>.event.item.<action>".
func ItemActionSubject(kioskCode, action string) string {
	return fmt.Sprintf("%s.%s.event.item.%s", SubjectPrefix(), kioskCode, action)
}

// InventoryAdjustSubject builds the subject for an admin stock-adjustment
// event: "<prefix>.<kiosk_code>.event.inventory.adjust". One event per
// accepted adjustment, emitted after the audit row commits.
func InventoryAdjustSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.event.inventory.adjust", SubjectPrefix(), kioskCode)
}

// IntegrityRebuildSubject builds the subject for the destructive
// open_checkouts rebuild event:
// "<prefix>.<kiosk_code>.event.integrity.rebuild". Low-frequency,
// high-signal — used for operator audit and (eventually) surfacing rebuild
// events in the controller's admin UI.
func IntegrityRebuildSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.event.integrity.rebuild", SubjectPrefix(), kioskCode)
}

// AdminCloseSubject builds the subject for an admin-initiated open_checkouts
// close event: "<prefix>.<kiosk_code>.event.checkout.admin_close". One event
// per closed row, emitted after the transaction+line+open-row delete commits.
// Distinct from item.return so reports can separate "worker returned it" from
// "admin closed it without a return" without a discriminator filter.
func AdminCloseSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.event.checkout.admin_close", SubjectPrefix(), kioskCode)
}

// InstanceLifecycleSubject builds the subject for an item_instances lifecycle
// event: "<prefix>.<kiosk_code>.event.instance.lifecycle". Fired by the PB
// record hooks on create, decommission (active true→false), reactivate
// (active false→true), and delete. Cosmetic edits don't publish.
func InstanceLifecycleSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.event.instance.lifecycle", SubjectPrefix(), kioskCode)
}

// ReceiptTransactionSubject builds the subject for a "render and send the
// transaction receipt" notification event:
// "<prefix>.<kiosk_code>.event.receipt.transaction". Payload is a
// notifications.ReceiptContext serialized as JSON. The controller's notifier
// consumes it, looks up the template, and sends. This is the managed-mode
// counterpart to the kiosk's local Notifier.Send call — the kiosk publishes
// structured context, the controller does the rendering and SMTP.
func ReceiptTransactionSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.event.receipt.transaction", SubjectPrefix(), kioskCode)
}

// LowStockAlertSubject builds the subject for a low-stock alert notification:
// "<prefix>.<kiosk_code>.event.alert.lowstock". Payload is a
// notifications.LowStockContext serialized as JSON. The kiosk owns the
// threshold-crossing detection (it has the inventory) and publishes only on
// a real cross; the controller renders + sends.
func LowStockAlertSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.event.alert.lowstock", SubjectPrefix(), kioskCode)
}

// ScanRFIDObservedSubject builds the subject for a "completed RFID read
// window" event: "<prefix>.<kiosk_code>.event.scan.rfid.observed". One
// event per ReadFor call (counter_scan or enclosure_diff), carrying the
// full deduplicated EPC array plus the cart_id the read was scoped to.
// Cheap observability — no projector consumes it today; the stream
// captures it for future drift detection and analytics. See
// docs/rfid.md.
func ScanRFIDObservedSubject(kioskCode string) string {
	return fmt.Sprintf("%s.%s.event.scan.rfid.observed", SubjectPrefix(), kioskCode)
}

// ScanRFIDObservedFilter is the controller-side consumer filter for
// the RFID observed-EPCs subject. Not bound to any projector yet —
// reserved so a future Watch/Audit hook can subscribe without
// re-stringing the pattern.
func ScanRFIDObservedFilter() string {
	return SubjectPrefix() + ".*.event.scan.rfid.observed"
}

// StreamSubjectFilter is the subject pattern the controller's JetStream
// stream binds to: "<prefix>.*.event.>". Only events live in the stream;
// commands and heartbeats are core-NATS-only and ride outside this filter.
func StreamSubjectFilter() string {
	return SubjectPrefix() + ".*.event.>"
}

// TransactionCompleteFilter, ItemActionFilter, InventoryAdjustFilter, and
// IntegrityRebuildFilter are the consumer-side FilterSubjects that select
// only the event shapes the aggregator dispatches. inventory.adjust and
// integrity.rebuild are accepted (and ack'd) by the consumer today but not
// yet projected into ledger state — they reach the controller for future
// reporting hooks and as an audit signal in slog.
func TransactionCompleteFilter() string {
	return SubjectPrefix() + ".*.event.transaction.complete"
}

func ItemActionFilter() string {
	return SubjectPrefix() + ".*.event.item.*"
}

func InventoryAdjustFilter() string {
	return SubjectPrefix() + ".*.event.inventory.adjust"
}

func IntegrityRebuildFilter() string {
	return SubjectPrefix() + ".*.event.integrity.rebuild"
}

// AdminCloseFilter is the controller-side consumer filter for the admin
// force-close subject.
func AdminCloseFilter() string {
	return SubjectPrefix() + ".*.event.checkout.admin_close"
}

// InstanceLifecycleFilter is the controller-side consumer filter for the
// item_instances lifecycle subject.
func InstanceLifecycleFilter() string {
	return SubjectPrefix() + ".*.event.instance.lifecycle"
}

// ReceiptTransactionFilter is the controller-side consumer filter for the
// receipt notification subject.
func ReceiptTransactionFilter() string {
	return SubjectPrefix() + ".*.event.receipt.transaction"
}

// LowStockAlertFilter is the controller-side consumer filter for the
// low-stock notification subject.
func LowStockAlertFilter() string {
	return SubjectPrefix() + ".*.event.alert.lowstock"
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

// Instance command subjects mirror the inventory family. Mutations
// (create, edit, decommission, reactivate) are idempotent via command_id;
// the snapshot read is unconditionally safe to replay. The kiosk's
// dispatcher routes on the suffix — adding a new one means adding a
// handler in internal/commands and (optionally) a builder here for the
// controller side to call by name rather than CommandSubject() with a
// literal.
func InstanceCreateCommandSubject(kioskCode string) string {
	return CommandSubject(kioskCode, "instance.create")
}

func InstanceEditCommandSubject(kioskCode string) string {
	return CommandSubject(kioskCode, "instance.edit")
}

func InstanceDecommissionCommandSubject(kioskCode string) string {
	return CommandSubject(kioskCode, "instance.decommission")
}

func InstanceReactivateCommandSubject(kioskCode string) string {
	return CommandSubject(kioskCode, "instance.reactivate")
}

func InstanceSnapshotCommandSubject(kioskCode string) string {
	return CommandSubject(kioskCode, "instance.snapshot")
}

// RFID enclosure_diff command subjects. cart.start is the external
// trigger (access-control system fires it when a worker badges into
// the enclosure door); read.trigger is the external read trigger
// (camera/occupancy system fires it when the door closes). Both are
// dispatched on the kiosk side by the standard Dispatcher.
func CartStartCommandSubject(kioskCode string) string {
	return CommandSubject(kioskCode, "cart.start")
}

func ReadTriggerCommandSubject(kioskCode string) string {
	return CommandSubject(kioskCode, "read.trigger")
}

// Maintenance command subjects. integrity.rebuild wipes + repopulates the
// kiosk's open_checkouts from its own ledger; ledger.republish re-emits
// every completed transaction's events (optionally clipped to a window)
// so the controller can backfill its projection after a NATS outage.
func IntegrityRebuildCommandSubject(kioskCode string) string {
	return CommandSubject(kioskCode, "integrity.rebuild")
}

func LedgerRepublishCommandSubject(kioskCode string) string {
	return CommandSubject(kioskCode, "ledger.republish")
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
