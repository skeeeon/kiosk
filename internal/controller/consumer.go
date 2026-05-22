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
)

// streamName is the JetStream stream the controller consumes from. It binds
// `kiosk.>` so every per-kiosk subject lands here without per-kiosk wiring.
const streamName = "KIOSK_EVENTS"

// consumerName is the durable consumer name. Durability means restarts
// resume from the last-acked sequence — no events lost across controller
// downtime, no replay storm on restart.
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
	CompletedAt   time.Time `json:"completed_at"`

	// transaction.complete fields.
	UserName   string    `json:"user_name,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	LinesCount int       `json:"lines_count,omitempty"`
	CheckedOut int       `json:"checked_out,omitempty"`
	Returned   int       `json:"returned,omitempty"`
	Consumed   int       `json:"consumed,omitempty"`

	// item.{action} fields.
	LineID       string `json:"line_id,omitempty"`
	ItemID       string `json:"item_id,omitempty"`
	ItemCode     string `json:"item_code,omitempty"`
	ItemName     string `json:"item_name,omitempty"`
	Action       string `json:"action,omitempty"`
	Qty          int    `json:"qty,omitempty"`
	Serial       string `json:"serial,omitempty"`
	Uncorrelated bool   `json:"uncorrelated,omitempty"`
}

// Aggregator owns the JetStream consumer lifecycle. One per controller
// process; Start launches the consume loop on a background goroutine, Stop
// drains it cleanly.
type Aggregator struct {
	app core.App
	js  jetstream.JetStream

	cancelCtx context.CancelFunc
	consumeCC jetstream.ConsumeContext
}

// NewAggregator wires the aggregator. Doesn't connect or subscribe yet —
// call Start for that.
func NewAggregator(app core.App, js jetstream.JetStream) *Aggregator {
	return &Aggregator{app: app, js: js}
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

	cc, err := cons.Consume(func(msg jetstream.Msg) {
		a.handle(ctx, msg)
	})
	if err != nil {
		cancel()
		return fmt.Errorf("start consume: %w", err)
	}
	a.consumeCC = cc

	slog.Info("controller.aggregator.started",
		"stream", streamName, "consumer", consumerName)
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
		Name:        streamName,
		Description: "Per-kiosk transaction + item events. Consumed by the controller.",
		Subjects:    []string{"kiosk.>"},
		Retention:   jetstream.LimitsPolicy,
		MaxAge:      7 * 24 * time.Hour,
		Storage:     jetstream.FileStorage,
		Replicas:    1,
	}
	// CreateOrUpdateStream is idempotent for compatible changes. Operators
	// who want different knobs (more replicas, longer retention) can `nats
	// stream edit` out of band — but the defaults work out of the box.
	return a.js.CreateOrUpdateStream(ctx, cfg)
}

func (a *Aggregator) ensureConsumer(ctx context.Context, stream jetstream.Stream) (jetstream.Consumer, error) {
	cfg := jetstream.ConsumerConfig{
		Durable:        consumerName,
		Description:    "kiosk-controller aggregator: projects per-kiosk events into the controller's ledger",
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        30 * time.Second,
		MaxAckPending:  256,
		FilterSubjects: []string{"kiosk.*.transaction.complete", "kiosk.*.item.*"},
	}
	return stream.CreateOrUpdateConsumer(ctx, cfg)
}

// handle dispatches a single message. Acks on success, Naks on transient
// errors (DB hiccup), Acks on logic-level "can't help it" cases (unknown
// user/item) — retrying won't change the outcome.
func (a *Aggregator) handle(ctx context.Context, msg jetstream.Msg) {
	subject := msg.Subject()
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

	// Touch the kiosks registry on every message — this is how operators
	// discover newly-deployed kiosks and how liveness is surfaced.
	if err := a.touchKiosk(payload.KioskCode, payload.LocationCode); err != nil {
		// DB hiccup — let JetStream retry the whole event rather than
		// half-process.
		slog.Warn("controller.aggregator.touch_kiosk_failed",
			"kiosk_code", payload.KioskCode, "error", err)
		_ = msg.Nak()
		return
	}

	switch {
	case strings.HasSuffix(subject, ".transaction.complete"):
		a.handleTransactionComplete(msg, payload)
	case strings.Contains(subject, ".item."):
		a.handleItemAction(msg, payload)
	default:
		// Stream subjects we don't recognize — ack so we don't pile up
		// redeliveries, but log so the operator sees the drift.
		slog.Info("controller.aggregator.unknown_subject", "subject", subject)
		_ = msg.Ack()
	}
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
	rec.Set("user", user.Id)
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

func (a *Aggregator) handleItemAction(msg jetstream.Msg, p EventPayload) {
	switch a.ProjectLine(p) {
	case projectAck:
		_ = msg.Ack()
	case projectRetry:
		// For "parent not yet here", a delay is better than immediate
		// redelivery so the transaction.complete has time to land.
		_ = msg.NakWithDelay(2 * time.Second)
	}
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

// TouchKiosk is the exported wrapper around the internal kiosks-registry
// upsert — exposed for testing the auto-register path.
func (a *Aggregator) TouchKiosk(kioskCode, locationCode string) error {
	return a.touchKiosk(kioskCode, locationCode)
}

// touchKiosk creates the kiosk's registry row if absent (status=unknown) and
// updates last_seen otherwise. Called on every accepted message so a kiosk's
// liveness reflects activity, not just connect/disconnect.
func (a *Aggregator) touchKiosk(kioskCode, locationCode string) error {
	now := time.Now().UTC()
	rec, err := a.app.FindFirstRecordByFilter("kiosks",
		"kiosk_code = {:code}",
		dbx.Params{"code": kioskCode})
	if err != nil && !isNotFound(err) {
		return err
	}
	if rec != nil {
		rec.Set("last_seen", now)
		return a.app.Save(rec)
	}

	col, err := a.app.FindCollectionByNameOrId("kiosks")
	if err != nil {
		return err
	}
	rec = core.NewRecord(col)
	rec.Set("kiosk_code", kioskCode)
	rec.Set("location_code", locationCode)
	rec.Set("last_seen", now)
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
	rec, err := a.app.FindFirstRecordByFilter("users",
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

func (a *Aggregator) findItemByCode(code string) (*core.Record, error) {
	rec, err := a.app.FindFirstRecordByFilter("items",
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
