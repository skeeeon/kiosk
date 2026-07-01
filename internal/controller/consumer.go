// Package controller contains the kiosk-controller's server-side logic:
// the JetStream consumer that aggregates per-kiosk transaction events into
// the controller's own ledger, and the catalog publisher that pushes item
// and user records down to managed kiosks via JetStream KV.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/timeclock"
)

// consumerName is the durable consumer name. Durability means restarts
// resume from the last-acked sequence — no events lost across controller
// downtime, no replay storm on restart. Not configurable: durable consumers
// are scoped per-stream, so the stream name (which IS configurable) is the
// real collision boundary.
const consumerName = "controller-aggregator"

// EventPayload mirrors what cmd/kiosk publishes in internal/commit/commit.go.
// Field types are deliberately permissive (JSON numbers, RFC3339 strings)
// so we can decode both event variants with a single struct.
type EventPayload struct {
	// Common to both event types.
	TransactionID string    `json:"transaction_id"`
	KioskCode     string    `json:"kiosk_code"`
	LocationCode  string    `json:"location_code"`
	UserID        string    `json:"user_id"`
	UserCode      string    `json:"user_code"`
	UserGroup     string    `json:"user_group,omitempty"`
	CompletedAt   time.Time `json:"completed_at"`

	// transaction.complete fields. TerminalID (accepting screen) and
	// EnclosureID (enclosure_diff cabinet) are optional attribution tags
	// (omitted on the wire when empty, so old kiosks decode to "" with no
	// behavior change).
	UserName    string    `json:"user_name,omitempty"`
	TerminalID  string    `json:"terminal_id,omitempty"`
	EnclosureID string    `json:"enclosure_id,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	LinesCount  int       `json:"lines_count,omitempty"`
	CheckedOut  int       `json:"checked_out,omitempty"`
	Returned    int       `json:"returned,omitempty"`
	Consumed    int       `json:"consumed,omitempty"`

	// item.{action} fields. OriginalCheckoutUserCode populates the
	// projected line's original_checkout_user FK (looked up against the
	// controller's catalog-synced users by code, since IDs differ across
	// binaries); ItemInstanceID is opaque text matching the kiosk's
	// item_instances.id, used to pair serialized checkout/return rows
	// during open_checkouts projection.
	LineID                   string `json:"line_id,omitempty"`
	ItemID                   string `json:"item_id,omitempty"`
	ItemCode                 string `json:"item_code,omitempty"`
	ItemName                 string `json:"item_name,omitempty"`
	Action                   string `json:"action,omitempty"`
	Qty                      int    `json:"qty,omitempty"`
	Serial                   string `json:"serial,omitempty"`
	Uncorrelated             bool   `json:"uncorrelated,omitempty"`
	OriginalCheckoutUserCode string `json:"original_checkout_user_code,omitempty"`
	ItemInstanceID           string `json:"item_instance_id,omitempty"`

	// inventory.adjust fields. AdminID is shared with integrity.rebuild;
	// ControllerAdminID is populated only when source=controller so the
	// audit projection can record the right actor population. Source is
	// the "local" / "controller" enum the kiosk-side publisher stamps.
	AdjustmentID      string `json:"adjustment_id,omitempty"`
	AdminID           string `json:"admin_id,omitempty"`
	ControllerAdminID string `json:"controller_admin_id,omitempty"`
	Mode              string `json:"mode,omitempty"`
	Value             int    `json:"value,omitempty"`
	Delta             int    `json:"delta,omitempty"`
	PrevQuantity      int    `json:"prev_quantity,omitempty"`
	NewQuantity       int    `json:"new_quantity,omitempty"`
	Reason            string `json:"reason,omitempty"`
	Source            string `json:"source,omitempty"`
	CommandID         string `json:"command_id,omitempty"`

	// integrity.rebuild fields.
	Deleted  int `json:"deleted,omitempty"`
	Inserted int `json:"inserted,omitempty"`

	// checkout.admin_close fields. Reuses AdminID / ControllerAdminID /
	// Source / Reason / CommandID above; the admin_close-specific bits are
	// the closure_reason (separate from inventory.adjust's reason text)
	// and the open_checkout_id snapshot for downstream reporting.
	OpenCheckoutID string `json:"open_checkout_id,omitempty"`
	ClosureReason  string `json:"closure_reason,omitempty"`
	Notes          string `json:"notes,omitempty"`

	// instance.lifecycle fields. SourceAuditID carries the kiosk-side
	// instance_audit.id so the controller's projection is idempotent under
	// JetStream redelivery (same pattern as AdjustmentID for inventory_audit).
	InstanceID    string `json:"instance_id,omitempty"`
	InstanceCode  string `json:"instance_code,omitempty"`
	PrevStatus    string `json:"prev_status,omitempty"`
	NewStatus     string `json:"new_status,omitempty"`
	SourceAuditID string `json:"source_audit_id,omitempty"`
	// RFIDEPC feeds the controller's instance_epc_index (location/sightings L3).
	RFIDEPC string `json:"rfid_epc,omitempty"`

	// timeclock.punch fields. PunchID is the kiosk-side time_punches.id —
	// the idempotency anchor (projected as source_punch_id). Source here is
	// the punch-source enum (self/foreman/admin/controller_admin), wider
	// than the local/controller pair the other events carry — both decode
	// into the same string field. OccurredAt is the business timestamp the
	// KV punch-state projection orders on.
	PunchID            string    `json:"punch_id,omitempty"`
	Direction          string    `json:"direction,omitempty"`
	OccurredAt         time.Time `json:"occurred_at,omitempty"`
	RecordedByUserCode string    `json:"recorded_by_user_code,omitempty"`
	Force              bool      `json:"force,omitempty"`
	JobCode            string    `json:"job_code,omitempty"`
	Note               string    `json:"note,omitempty"`
	RecordedAt         time.Time `json:"recorded_at,omitempty"`
}

// Aggregator owns the JetStream consumer lifecycle. One per controller
// process; Start launches the consume loop on a background goroutine, Stop
// drains it cleanly.
type Aggregator struct {
	app        core.App
	js         jetstream.JetStream
	streamName string

	// notifier is optional. When set, receipt.transaction and alert.lowstock
	// events on the stream are dispatched to it for rendering + SMTP send
	// against the controller's centrally-edited template rows. Wired via
	// SetNotifier from cmd/controller/main after the app is bootstrapped.
	notifier *notifications.Notifier

	// punchKV is the punch_state bucket the aggregator broadcasts per-user
	// clocked-in state into after projecting a timeclock.punch. Provisioned
	// in Start; nil until then (writePunchState nil-checks). The replica is
	// advisory — KV failures log and the event still acks; the kiosk merge
	// rule + the next punch self-heal.
	punchKV jetstream.KeyValue

	// checkoutKV is the open_checkouts_state bucket the aggregator recomputes
	// per-user after projecting a checkout/return/admin_close line. Feeds the
	// cross-kiosk clock-out gate at managed kiosks + the virtual terminal.
	// Provisioned in Start; nil until then (refreshOpenCheckoutsState
	// nil-checks). Advisory, same posture as punchKV.
	checkoutKV jetstream.KeyValue

	cancelCtx context.CancelFunc
	consumeCC jetstream.ConsumeContext
}

// SetNotifier installs the notifier that handles receipt.transaction and
// alert.lowstock events. Must be called before Start. Nil disables the
// notifier dispatch (the consumer still ack/Term's those events to keep the
// stream draining; no email goes out).
func (a *Aggregator) SetNotifier(n *notifications.Notifier) {
	a.notifier = n
}

// NewAggregator wires the aggregator. Doesn't connect or subscribe yet —
// call Start for that. Empty streamName falls back to events.DefaultStreamName
// so operators with the standard layout can leave cfg.NATS.StreamName blank.
func NewAggregator(app core.App, js jetstream.JetStream, streamName string) *Aggregator {
	if streamName == "" {
		streamName = events.DefaultStreamName
	}
	return &Aggregator{app: app, js: js, streamName: streamName}
}

// Start provisions the stream + consumer (idempotent) and begins consuming.
// Returns once Consume is running on a goroutine; errors here are
// startup-fatal (broker unreachable, permissions wrong).
func (a *Aggregator) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	a.cancelCtx = cancel

	stream, err := a.ensureStream(ctx)
	if err != nil {
		cancel()
		return fmt.Errorf("ensure stream: %w", err)
	}

	cons, err := a.ensureConsumer(ctx, stream)
	if err != nil {
		cancel()
		return fmt.Errorf("ensure consumer: %w", err)
	}

	// punch_state bucket — best-effort: a KV provisioning failure degrades
	// managed kiosks to local-only punch state, it doesn't stop the ledger
	// projection.
	if kv, kvErr := a.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      timeclock.PunchStateBucket,
		Description: "Per-user clocked-in state, broadcast-keyed by user_code. Written by the controller aggregator, watched by managed kiosks.",
		History:     1,
	}); kvErr != nil {
		slog.Warn("controller.aggregator.punch_state_bucket_failed", "error", kvErr)
	} else {
		a.punchKV = kv
	}

	// open_checkouts_state bucket — same best-effort posture as punch_state.
	if kv, kvErr := a.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      timeclock.OpenCheckoutsStateBucket,
		Description: "Per-user fleet-wide open-checkout summary, broadcast-keyed by user_code. Written by the controller aggregator, watched by managed kiosks for the cross-kiosk clock-out gate.",
		History:     1,
	}); kvErr != nil {
		slog.Warn("controller.aggregator.open_checkouts_state_bucket_failed", "error", kvErr)
	} else {
		a.checkoutKV = kv
	}

	cc, err := cons.Consume(func(msg jetstream.Msg) {
		a.handle(ctx, msg)
	})
	if err != nil {
		cancel()
		return fmt.Errorf("start consume: %w", err)
	}
	a.consumeCC = cc

	slog.Info("controller.aggregator.started",
		"stream", a.streamName, "consumer", consumerName)
	return nil
}

// Stop tears down the consume loop. Safe to call multiple times.
func (a *Aggregator) Stop() {
	if a.consumeCC != nil {
		a.consumeCC.Stop()
		a.consumeCC = nil
	}
	if a.cancelCtx != nil {
		a.cancelCtx()
		a.cancelCtx = nil
	}
}

func (a *Aggregator) ensureStream(ctx context.Context) (jetstream.Stream, error) {
	cfg := jetstream.StreamConfig{
		Name:        a.streamName,
		Description: "Per-kiosk events (kiosk.*.event.>). Consumed by the controller.",
		// Stream owns only the event subject space. Commands
		// (kiosk.*.command.>) and heartbeats (kiosk.*.heartbeat) ride core
		// NATS and are outside this filter by construction.
		Subjects:  []string{events.StreamSubjectFilter()},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    7 * 24 * time.Hour,
		Storage:   jetstream.FileStorage,
		Replicas:  1,
	}
	// CreateOrUpdateStream is idempotent for compatible changes. Narrowing
	// Subjects on a stream that already holds messages outside the new
	// pattern will fail — operators upgrading from the old "kiosk.>" stream
	// must `nats stream rm KIOSK_EVENTS` once so this call recreates it.
	return a.js.CreateOrUpdateStream(ctx, cfg)
}

func (a *Aggregator) ensureConsumer(ctx context.Context, stream jetstream.Stream) (jetstream.Consumer, error) {
	cfg := jetstream.ConsumerConfig{
		Durable:       consumerName,
		Description:   "kiosk-controller aggregator: projects per-kiosk events into the controller's ledger",
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
		// 120s, not 30s: each handler wraps its projection in a SQLite
		// RunInTransaction, and SQLite is single-writer — under concurrent
		// HTTP/admin writes or a CSV import a projection can briefly block on
		// the write lock. A handler that exceeds AckWait is redelivered while
		// still in flight, causing duplicate work. Comfortable headroom over
		// worst-case projection latency keeps redelivery to the genuine
		// crash/partition cases the idempotency anchors already cover (every
		// projection is a lookup-first idempotent insert keyed on a source id).
		AckWait:       120 * time.Second,
		MaxAckPending: 256,
		FilterSubjects: []string{
			events.TransactionCompleteFilter(),
			events.ItemActionFilter(),
			events.InventoryAdjustFilter(),
			events.IntegrityRebuildFilter(),
			// Admin-initiated close + instance lifecycle land here too so
			// the controller has a single durable cursor over every event
			// shape. Today the dispatcher ack-and-logs both; future
			// projectors can light up without re-subscribing.
			events.AdminCloseFilter(),
			events.InstanceLifecycleFilter(),
			// Notification subjects published by managed kiosks. The
			// controller renders + sends from its own template rows so
			// SMTP credentials and recipient lists live centrally rather
			// than on every kiosk. Digests no longer ride NATS — the
			// controller owns the scheduler in managed mode, so the
			// digest envelopes are gone.
			events.ReceiptTransactionFilter(),
			events.LowStockAlertFilter(),
			events.MaintenanceAlertFilter(),
			// Timeclock punches project into the controller's own
			// time_punches and drive the punch_state KV broadcast.
			events.TimeclockPunchFilter(),
		},
	}
	return stream.CreateOrUpdateConsumer(ctx, cfg)
}

// handle dispatches a single message. Acks on success, Naks on transient
// errors (DB hiccup), Acks on logic-level "can't help it" cases (unknown
// user/item) — retrying won't change the outcome.
func (a *Aggregator) handle(ctx context.Context, msg jetstream.Msg) {
	subject := msg.Subject()

	// Notification subjects carry nested context payloads (ReceiptContext,
	// LowStockContext) that don't fit the flat EventPayload shape used by
	// ledger projection. Dispatch them here, before the flat decode + the
	// KioskCode guard that would otherwise reject them.
	switch {
	case strings.HasSuffix(subject, ".receipt.transaction"):
		a.handleReceiptTransaction(msg)
		return
	case strings.HasSuffix(subject, ".alert.lowstock"):
		a.handleLowStockAlert(msg)
		return
	case strings.HasSuffix(subject, ".alert.maintenance"):
		a.handleMaintenanceAlert(msg)
		return
	}

	var payload EventPayload
	if err := unmarshalMsg(msg, &payload); err != nil {
		slog.Warn("controller.aggregator.bad_payload",
			"subject", subject, "error", err)
		_ = msg.Term()
		return
	}

	if payload.KioskCode == "" {
		slog.Warn("controller.aggregator.missing_kiosk_code", "subject", subject)
		_ = msg.Term()
		return
	}

	// touchKiosk is now narrowed to transaction.complete only: the kiosks
	// row's last_transaction_at reflects the field's name, and the in-memory
	// heartbeat registry owns general liveness. Auto-registration for kiosks
	// that haven't transacted yet happens on first heartbeat instead (see
	// internal/controller/heartbeats.go).
	switch {
	case strings.HasSuffix(subject, ".transaction.complete"):
		if err := a.touchKiosk(payload.KioskCode, payload.LocationCode, payload.CompletedAt); err != nil {
			slog.Warn("controller.aggregator.touch_kiosk_failed",
				"kiosk_code", payload.KioskCode, "error", err)
			_ = msg.Nak()
			return
		}
		a.handleTransactionComplete(msg, payload)
	case strings.Contains(subject, ".item."):
		a.handleItemAction(ctx, msg, payload)
	case strings.HasSuffix(subject, ".inventory.adjust"):
		a.handleInventoryAdjust(msg, payload)
	case strings.HasSuffix(subject, ".integrity.rebuild"):
		// Audit-only today: log and ack. Surfaced here so a future ops view
		// can list "kiosks that recently rebuilt their projection" without
		// changing the publisher.
		slog.Info("controller.aggregator.integrity_rebuild",
			"subject", subject,
			"kiosk_code", payload.KioskCode,
			"admin_id", payload.AdminID,
			"deleted", payload.Deleted,
			"inserted", payload.Inserted)
		_ = msg.Ack()
	case strings.HasSuffix(subject, ".checkout.admin_close"):
		// Log first so the audit trail survives even if the deletion path
		// retries. The kiosk emits one of these per admin-driven close; the
		// transaction_line row itself rides transaction.complete (the kiosk
		// doesn't publish item.admin_close as an item action), so this is
		// the only subject that can close out the projected open_checkouts
		// row on the controller's side.
		slog.Info("controller.aggregator.checkout_admin_close",
			"subject", subject,
			"kiosk_code", payload.KioskCode,
			"transaction_id", payload.TransactionID,
			"open_checkout_id", payload.OpenCheckoutID,
			"closure_reason", payload.ClosureReason,
			"source", payload.Source,
			"admin_id", payload.AdminID,
			"controller_admin_id", payload.ControllerAdminID)
		a.handleCheckoutAdminClose(ctx, msg, payload)
	case strings.HasSuffix(subject, ".instance.lifecycle"):
		a.handleInstanceLifecycle(msg, payload)
	case strings.HasSuffix(subject, ".timeclock.punch"):
		a.handleTimeclockPunch(ctx, msg, payload)
	default:
		// Stream subjects we don't recognize — ack so we don't pile up
		// redeliveries, but log so the operator sees the drift.
		slog.Info("controller.aggregator.unknown_subject", "subject", subject)
		_ = msg.Ack()
	}
}

// handleInventoryAdjust is the JetStream-side dispatcher for
// inventory.adjust. Splits to ProjectInventoryAudit (pure projection +
// outcome) so tests can drive the side effect without a real message.
func (a *Aggregator) handleInventoryAdjust(msg jetstream.Msg, p EventPayload) {
	switch a.ProjectInventoryAudit(p) {
	case projectAck:
		_ = msg.Ack()
	case projectRetry:
		_ = msg.Nak()
	}
}

// ProjectInventoryAudit upserts the audit row for one inventory.adjust
// event. Idempotent via the unique source_adjustment_id index — a
// JetStream redelivery finds the existing row and returns projectAck
// without writing.
func (a *Aggregator) ProjectInventoryAudit(p EventPayload) projectOutcome {
	if p.AdjustmentID == "" {
		// Should never happen — the kiosk's publisher always sets it
		// from stock_adjustments.id. Ack rather than retry; a bad
		// payload won't improve on redelivery.
		slog.Warn("controller.aggregator.inventory_adjust.missing_adjustment_id",
			"kiosk_code", p.KioskCode, "item_code", p.ItemCode)
		return projectAck
	}

	existing, err := a.findInventoryAuditByAdjustmentID(p.AdjustmentID)
	if err != nil {
		slog.Warn("controller.aggregator.inventory_audit.lookup_failed", "error", err)
		return projectRetry
	}
	if existing != nil {
		return projectAck
	}

	col, err := a.app.FindCollectionByNameOrId("inventory_audit")
	if err != nil {
		slog.Warn("controller.aggregator.inventory_audit.collection_missing", "error", err)
		return projectRetry
	}

	rec := core.NewRecord(col)
	rec.Set("kiosk_code", p.KioskCode)
	rec.Set("item_code", p.ItemCode)
	rec.Set("item_name", p.ItemName)
	rec.Set("source_adjustment_id", p.AdjustmentID)
	// Whichever admin field the publisher set is the actor. We collapse
	// both into one column and disambiguate via `source` so the report
	// table doesn't need a coalesce-on-render.
	switch p.Source {
	case "controller":
		rec.Set("admin_id", p.ControllerAdminID)
	default:
		rec.Set("admin_id", p.AdminID)
	}
	rec.Set("mode", p.Mode)
	rec.Set("delta", p.Delta)
	rec.Set("prev_quantity", p.PrevQuantity)
	rec.Set("new_quantity", p.NewQuantity)
	rec.Set("reason", p.Reason)
	if p.Source != "" {
		rec.Set("source", p.Source)
	}
	rec.Set("command_id", p.CommandID)
	if !p.CompletedAt.IsZero() {
		rec.Set("occurred_at", p.CompletedAt)
	}
	if err := a.app.Save(rec); err != nil {
		if isUniqueViolation(err) {
			// Concurrent insert collision — vanishingly rare with one
			// durable consumer, but be safe and treat as already-projected.
			return projectAck
		}
		slog.Warn("controller.aggregator.inventory_audit.save_failed", "error", err)
		return projectRetry
	}
	return projectAck
}

func (a *Aggregator) findInventoryAuditByAdjustmentID(adjustmentID string) (*core.Record, error) {
	rec, err := a.app.FindFirstRecordByFilter("inventory_audit",
		"source_adjustment_id = {:id}",
		dbx.Params{"id": adjustmentID})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return rec, nil
}

// handleInstanceLifecycle is the JetStream-side dispatcher for
// instance.lifecycle. Splits to ProjectInstanceLifecycle (pure projection +
// outcome) so tests can drive the side effect without a real message.
func (a *Aggregator) handleInstanceLifecycle(msg jetstream.Msg, p EventPayload) {
	switch a.ProjectInstanceLifecycle(p) {
	case projectAck:
		_ = msg.Ack()
	case projectRetry:
		_ = msg.Nak()
	}
}

// ProjectInstanceLifecycle upserts the audit row for one instance.lifecycle
// event. Idempotent via the unique source_audit_id index — a JetStream
// redelivery finds the existing row and returns projectAck without writing.
func (a *Aggregator) ProjectInstanceLifecycle(p EventPayload) projectOutcome {
	// Keep the EPC → owning-unit index current (location/sightings L3). Done
	// first and unconditionally (even on redelivery / missing source_audit_id)
	// because it's an idempotent upsert and the SightingIngest depends on it.
	// Best-effort — a failure logs and never changes the audit projection's
	// outcome.
	a.upsertInstanceEPCIndex(p)

	if p.SourceAuditID == "" {
		// Older kiosks (pre source_audit_id) don't carry the idempotency
		// anchor. Without it we can't safely dedupe redeliveries, so ack
		// rather than write a row that might duplicate later. A warn surfaces
		// the drift so the operator knows to update kiosks.
		slog.Warn("controller.aggregator.instance_lifecycle.missing_source_audit_id",
			"kiosk_code", p.KioskCode, "instance_id", p.InstanceID)
		return projectAck
	}

	existing, err := a.findInstanceLifecycleAuditBySourceID(p.SourceAuditID)
	if err != nil {
		slog.Warn("controller.aggregator.instance_lifecycle_audit.lookup_failed", "error", err)
		return projectRetry
	}
	if existing != nil {
		return projectAck
	}

	col, err := a.app.FindCollectionByNameOrId("instance_lifecycle_audit")
	if err != nil {
		slog.Warn("controller.aggregator.instance_lifecycle_audit.collection_missing", "error", err)
		return projectRetry
	}

	rec := core.NewRecord(col)
	rec.Set("kiosk_code", p.KioskCode)
	rec.Set("item_code", p.ItemCode)
	rec.Set("item_name", p.ItemName)
	rec.Set("instance_id", p.InstanceID)
	rec.Set("instance_code", p.InstanceCode)
	rec.Set("action", p.Action)
	rec.Set("prev_status", p.PrevStatus)
	rec.Set("new_status", p.NewStatus)
	rec.Set("reason", p.Reason)
	// Same collapse-into-one-column treatment as inventory_audit so the
	// report table doesn't need to coalesce two fields on render.
	switch p.Source {
	case events.SourceController:
		rec.Set("admin_id", p.ControllerAdminID)
	default:
		rec.Set("admin_id", p.AdminID)
	}
	if p.Source != "" {
		rec.Set("source", p.Source)
	}
	rec.Set("command_id", p.CommandID)
	rec.Set("source_audit_id", p.SourceAuditID)
	if !p.CompletedAt.IsZero() {
		rec.Set("occurred_at", p.CompletedAt)
	}
	if err := a.app.Save(rec); err != nil {
		if isUniqueViolation(err) {
			return projectAck
		}
		slog.Warn("controller.aggregator.instance_lifecycle_audit.save_failed", "error", err)
		return projectRetry
	}
	return projectAck
}

// upsertInstanceEPCIndex keeps instance_epc_index current so the SightingIngest
// can resolve a raw gateway sighting to its owning (instance, kiosk). Upsert by
// rfid_epc (the unique key). Best-effort: logs and returns on any error — it
// must never affect the lifecycle audit projection's outcome. No-op for an
// untagged unit.
func (a *Aggregator) upsertInstanceEPCIndex(p EventPayload) {
	epc := strings.ToLower(strings.TrimSpace(p.RFIDEPC))
	if epc == "" || p.KioskCode == "" {
		return
	}
	col, err := a.app.FindCollectionByNameOrId("instance_epc_index")
	if err != nil {
		slog.Warn("controller.aggregator.epc_index.collection_missing", "error", err)
		return
	}
	rec, err := a.app.FindFirstRecordByFilter("instance_epc_index",
		"rfid_epc = {:epc}", dbx.Params{"epc": epc})
	if err != nil {
		if !isNotFound(err) {
			slog.Warn("controller.aggregator.epc_index.lookup_failed", "epc", epc, "error", err)
			return
		}
		rec = core.NewRecord(col)
		rec.Set("rfid_epc", epc)
	}
	rec.Set("instance_id", p.InstanceID)
	rec.Set("instance_code", p.InstanceCode)
	rec.Set("kiosk_code", p.KioskCode)
	// Item identity rides the same payload (the lifecycle event carries it);
	// stamping it here lets the fleet location report name the seen unit.
	rec.Set("item_code", p.ItemCode)
	rec.Set("item_name", p.ItemName)
	if err := a.app.Save(rec); err != nil {
		if isUniqueViolation(err) {
			return // concurrent insert won the race; the row exists, good enough
		}
		slog.Warn("controller.aggregator.epc_index.save_failed", "epc", epc, "error", err)
	}
}

func (a *Aggregator) findInstanceLifecycleAuditBySourceID(sourceAuditID string) (*core.Record, error) {
	rec, err := a.app.FindFirstRecordByFilter("instance_lifecycle_audit",
		"source_audit_id = {:id}",
		dbx.Params{"id": sourceAuditID})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return rec, nil
}

func (a *Aggregator) handleTransactionComplete(msg jetstream.Msg, p EventPayload) {
	switch a.ProjectTransaction(p) {
	case projectAck:
		_ = msg.Ack()
	case projectRetry:
		_ = msg.Nak()
	}
}

// projectOutcome controls how the dispatcher acks the underlying JetStream
// message. Pulled out so projection logic is testable without conjuring a
// fake jetstream.Msg.
type projectOutcome int

const (
	projectAck   projectOutcome = iota // success or terminal-skip — drop from the queue
	projectRetry                       // transient failure — let JS redeliver
)

// ProjectTransaction is the pure-state effect of a transaction.complete event:
// idempotently upserts a controller-side transactions row. Returns projectAck
// for success, duplicate, or terminal-skip (e.g., unknown user); projectRetry
// for DB hiccups.
func (a *Aggregator) ProjectTransaction(p EventPayload) projectOutcome {
	existing, err := a.findTransaction(p.KioskCode, p.TransactionID)
	if err != nil {
		slog.Warn("controller.aggregator.tx_lookup_failed", "error", err)
		return projectRetry
	}
	if existing != nil {
		return projectAck // duplicate delivery
	}

	user, err := a.findUserByCode(p.UserCode)
	if err != nil {
		slog.Warn("controller.aggregator.user_lookup_failed",
			"user_code", p.UserCode, "error", err)
		return projectRetry
	}
	if user == nil {
		// Catalog hasn't caught up (or this kiosk has unmanaged catalog).
		// Ack — retrying won't help; reconciliation is phase 2.
		slog.Warn("controller.aggregator.unknown_user",
			"user_code", p.UserCode, "kiosk_code", p.KioskCode)
		return projectAck
	}

	col, err := a.app.FindCollectionByNameOrId("transactions")
	if err != nil {
		slog.Warn("controller.aggregator.find_transactions_collection_failed", "error", err)
		return projectRetry
	}
	rec := core.NewRecord(col)
	rec.Set("kiosk_code", p.KioskCode)
	rec.Set("location_code", p.LocationCode)
	rec.Set("terminal_id", p.TerminalID)
	rec.Set("enclosure_id", p.EnclosureID)
	rec.Set("user", user.Id)
	rec.Set("user_group", p.UserGroup)
	rec.Set("started_at", p.StartedAt)
	rec.Set("completed_at", p.CompletedAt)
	rec.Set("status", "completed")
	rec.Set("lines_count", p.LinesCount)
	rec.Set("source_kiosk_code", p.KioskCode)
	rec.Set("source_transaction_id", p.TransactionID)
	if err := a.app.Save(rec); err != nil {
		// Concurrent insert collision (shouldn't happen with one durable
		// consumer, but be safe): treat as already-projected.
		if isUniqueViolation(err) {
			return projectAck
		}
		slog.Warn("controller.aggregator.save_transaction_failed", "error", err)
		return projectRetry
	}
	return projectAck
}

func (a *Aggregator) handleItemAction(ctx context.Context, msg jetstream.Msg, p EventPayload) {
	// Item actions only project the ledger line. "What's currently out" is
	// derived on demand by replaying transaction_lines (ledger.ReplayOpenRows),
	// so there's no separate open_checkouts row to maintain here — the line IS
	// the source of truth. ProjectLine is idempotent on source_line_id, so a
	// redelivery is a no-op.
	switch a.ProjectLine(p) {
	case projectAck:
		// The line changed (or could have changed) the affected user's open
		// set — refresh the fleet open-checkouts replica that feeds the
		// cross-kiosk clock-out gate. Best-effort; never blocks the ack.
		a.refreshOpenCheckoutsForLine(ctx, p)
		_ = msg.Ack()
	case projectRetry:
		// Usually "parent transaction.complete not here yet" — delay so it has
		// time to land before we retry.
		_ = msg.NakWithDelay(2 * time.Second)
	}
}

// handleCheckoutAdminClose dispatches a checkout.admin_close event. The audit
// log lives in the parent handle(); this projects the close into the ledger.
func (a *Aggregator) handleCheckoutAdminClose(ctx context.Context, msg jetstream.Msg, p EventPayload) {
	switch a.ProjectAdminCloseToLedger(p) {
	case projectAck:
		// An admin close removes the holder's open row — refresh their fleet
		// replica too (p.UserCode is the holder for admin_close).
		a.refreshOpenCheckoutsForLine(ctx, p)
		_ = msg.Ack()
	case projectRetry:
		_ = msg.NakWithDelay(2 * time.Second)
	}
}

// ProjectAdminCloseToLedger records an admin force-close as a completed
// transaction + one admin_close line, mirroring what the kiosk writes to its
// own ledger in commit.AdminClose. admin_close rides its own subject (never
// transaction.complete / item.{action}), so without this the controller's
// ledger would have no record of the close and ledger.ReplayOpenRows would
// leave the holder's row open forever — the divergence this whole approach
// removes. There is no separate open_checkouts mutation: the line IS the close,
// and replay drops the row.
//
// The holder (p.UserCode) becomes both the transaction user and the line's
// original_checkout_user — exactly as commit.AdminClose stamps them — so
// replay's removeRows targets the holder's row whether it reads the line FK or
// falls back to the transaction user. Both writes are idempotent on their
// source ids (source_transaction_id / source_line_id), so redelivery no-ops.
func (a *Aggregator) ProjectAdminCloseToLedger(p EventPayload) projectOutcome {
	p.Action = "admin_close"
	if p.Qty < 1 {
		p.Qty = 1 // admin_close always closes exactly one row
	}
	if p.LinesCount == 0 {
		p.LinesCount = 1
	}
	if p.StartedAt.IsZero() {
		p.StartedAt = p.CompletedAt
	}
	p.OriginalCheckoutUserCode = p.UserCode

	if out := a.ProjectTransaction(p); out != projectAck {
		return out
	}
	return a.ProjectLine(p)
}

// ProjectLine is the pure-state effect of an item.{action} event: idempotently
// upserts a controller-side transaction_lines row, linking it to the parent
// transaction (looked up by source key). Returns projectRetry when the parent
// isn't here yet — JS will redeliver after a short backoff.
func (a *Aggregator) ProjectLine(p EventPayload) projectOutcome {
	if p.LineID == "" {
		slog.Warn("controller.aggregator.missing_line_id")
		return projectAck // bad payload; nothing to do
	}

	existing, err := a.findLine(p.LineID)
	if err != nil {
		slog.Warn("controller.aggregator.line_lookup_failed", "error", err)
		return projectRetry
	}
	if existing != nil {
		return projectAck
	}

	parent, err := a.findTransaction(p.KioskCode, p.TransactionID)
	if err != nil {
		slog.Warn("controller.aggregator.parent_lookup_failed", "error", err)
		return projectRetry
	}
	if parent == nil {
		// transaction.complete hasn't landed yet — retry with delay so it
		// catches up.
		return projectRetry
	}

	item, err := a.findItemByCode(p.ItemCode)
	if err != nil {
		slog.Warn("controller.aggregator.item_lookup_failed", "error", err)
		return projectRetry
	}
	if item == nil {
		slog.Warn("controller.aggregator.unknown_item",
			"item_code", p.ItemCode, "kiosk_code", p.KioskCode)
		return projectAck
	}

	col, err := a.app.FindCollectionByNameOrId("transaction_lines")
	if err != nil {
		slog.Warn("controller.aggregator.find_tx_lines_collection_failed", "error", err)
		return projectRetry
	}
	rec := core.NewRecord(col)
	rec.Set("transaction", parent.Id)
	rec.Set("item", item.Id)
	rec.Set("action", p.Action)
	rec.Set("qty", p.Qty)
	if p.Serial != "" {
		rec.Set("serial", p.Serial)
	}
	if p.Uncorrelated {
		rec.Set("uncorrelated", true)
	}
	if p.OriginalCheckoutUserCode != "" {
		// Best-effort: an unknown user_code (catalog drift) drops the FK
		// rather than failing projection. Same posture as the unknown-item
		// branch above.
		if u, err := a.findUserByCode(p.OriginalCheckoutUserCode); err == nil && u != nil {
			rec.Set("original_checkout_user", u.Id)
		}
	}
	// We deliberately don't write the item_instance RelationField — instances
	// are kiosk-local, so the FK would fail to resolve against the controller's
	// own (always-empty) item_instances collection. Instead we persist the
	// kiosk-local instance id in the parallel source_item_instance_id text
	// column, so ledger.ReplayOpenRows can match a serialized checkout to its
	// return when it reconstructs the open-checkouts view.
	if p.ItemInstanceID != "" {
		rec.Set("source_item_instance_id", p.ItemInstanceID)
	}
	rec.Set("source_line_id", p.LineID)
	if err := a.app.Save(rec); err != nil {
		if isUniqueViolation(err) {
			return projectAck
		}
		slog.Warn("controller.aggregator.save_line_failed", "error", err)
		return projectRetry
	}
	return projectAck
}

// TouchKiosk is the exported wrapper used by HeartbeatRegistry's auto-register
// on first beat. A heartbeat is not a transaction, so it passes the zero time —
// last_transaction_at stays empty and honestly reads "no transactions yet" for
// a kiosk that's online but hasn't transacted.
func (a *Aggregator) TouchKiosk(kioskCode, locationCode string) error {
	return a.touchKiosk(kioskCode, locationCode, time.Time{})
}

// touchKiosk creates the kiosk's registry row if absent (status=unknown) and
// advances last_transaction_at from lastTxAt — the event's completed_at, NOT
// wall-clock now() — and only ever forward (monotonic max). That keeps the
// field truthful under JetStream redelivery and ledger.republish: replaying
// an old transaction can't drag it backward, and re-projecting the same event
// is a no-op rather than a bump to "now." A zero lastTxAt (the heartbeat
// path) leaves last_transaction_at alone; live online status comes from the
// heartbeat registry, not this row.
func (a *Aggregator) touchKiosk(kioskCode, locationCode string, lastTxAt time.Time) error {
	rec, err := a.app.FindFirstRecordByFilter("kiosks",
		"kiosk_code = {:code}",
		dbx.Params{"code": kioskCode})
	if err != nil && !isNotFound(err) {
		return err
	}
	if rec != nil {
		if !lastTxAt.IsZero() && lastTxAt.After(rec.GetDateTime("last_transaction_at").Time()) {
			rec.Set("last_transaction_at", lastTxAt)
			return a.app.Save(rec)
		}
		return nil
	}

	col, err := a.app.FindCollectionByNameOrId("kiosks")
	if err != nil {
		return err
	}
	rec = core.NewRecord(col)
	rec.Set("kiosk_code", kioskCode)
	rec.Set("location_code", locationCode)
	if !lastTxAt.IsZero() {
		rec.Set("last_transaction_at", lastTxAt)
	}
	rec.Set("status", "unknown")
	return a.app.Save(rec)
}

func (a *Aggregator) findTransaction(kioskCode, sourceTxID string) (*core.Record, error) {
	rec, err := a.app.FindFirstRecordByFilter("transactions",
		"source_kiosk_code = {:k} && source_transaction_id = {:t}",
		dbx.Params{"k": kioskCode, "t": sourceTxID})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return rec, nil
}

func (a *Aggregator) findLine(sourceLineID string) (*core.Record, error) {
	rec, err := a.app.FindFirstRecordByFilter("transaction_lines",
		"source_line_id = {:l}",
		dbx.Params{"l": sourceLineID})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return rec, nil
}

func (a *Aggregator) findUserByCode(code string) (*core.Record, error) {
	return findUserByCodeOnApp(a.app, code)
}

func (a *Aggregator) findItemByCode(code string) (*core.Record, error) {
	return findItemByCodeOnApp(a.app, code)
}

// findUserByCodeOnApp / findItemByCodeOnApp accept an explicit core.App so
// callers inside RunInTransaction closures (where the tx's app must be used
// instead of a.app) can share the same lookup shape as the Aggregator-method
// path. Returns nil record / nil error when no row matches.
func findUserByCodeOnApp(app core.App, code string) (*core.Record, error) {
	rec, err := app.FindFirstRecordByFilter("users",
		"code = {:c}",
		dbx.Params{"c": code})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return rec, nil
}

func findItemByCodeOnApp(app core.App, code string) (*core.Record, error) {
	rec, err := app.FindFirstRecordByFilter("items",
		"code = {:c}",
		dbx.Params{"c": code})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return rec, nil
}
