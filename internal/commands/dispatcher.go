// Package commands is the kiosk-side handler for controller→kiosk commands
// arriving over NATS. The controller publishes JSON to
//
//	<prefix>.<kiosk_code>.command.<name>
//
// with a Reply inbox set. The dispatcher routes on <name> and replies with
// a {success, error, data} envelope. Core NATS request/reply (not JetStream)
// — commands are synchronous, single-attempt, and should fail fast when the
// kiosk is offline rather than queue indefinitely.
//
// Built-in commands: inventory.adjust (mutating, idempotent via command_id),
// inventory.snapshot (read-only). Add new commands by registering a Handler
// against a suffix string.
//
// Wire reference: docs/wire.md — payload/reply shapes per command.
package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/handlers"
)

// Reply is the canonical envelope every command handler returns. Encoded to
// JSON and sent on the message's Reply subject. data is whatever the
// specific command returns; the controller endpoint decodes it per command.
type Reply struct {
	Success bool        `json:"success"`
	Error   string      `json:"error,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

// Handler implements a single command. ctx is bound to the dispatcher's
// lifetime — handlers should respect cancellation for any long work.
type Handler func(ctx context.Context, payload []byte) Reply

// Dispatcher owns the NATS subscription and the routing table. One instance
// per kiosk process.
//
// Most commands only need core.App for DB reads and the existing handlers
// package's free functions (PerformStockAdjustment etc.). Phase 4's
// enclosure_diff commands need the cart store, the SSE broker, and the
// LLRP reader — all of which live on *handlers.Handlers. Rather than
// thread a half-dozen pointers through NewDispatcher's signature, we
// expose KioskHandlers as a field set after construction. Commands that
// need it nil-check; counter_scan and inventory commands never touch it.
type Dispatcher struct {
	app       core.App
	kioskCode string
	handlers  map[string]Handler
	logger    *slog.Logger
	sub       *nats.Subscription
	// prefix is the literal command-subject prefix for this kiosk, e.g.
	// "kiosk.K01.command.". Cached at construction so subjectSuffix is a
	// single TrimPrefix with no allocation per message.
	prefix string

	// KioskHandlers carries dependencies (cart store, RFID reader,
	// SSE broker) that Phase-4 enclosure_diff commands reach into.
	// Optional: nil-safe for legacy callers and tests that wire only
	// inventory/instance commands. cmd/kiosk/main.go sets it after
	// constructing both the dispatcher and the handlers.
	KioskHandlers *handlers.Handlers
}

// NewDispatcher constructs a dispatcher with the built-in command handlers
// registered. Caller invokes Register(nc) to actually attach to NATS.
func NewDispatcher(app core.App, kioskCode string) *Dispatcher {
	d := &Dispatcher{
		app:       app,
		kioskCode: kioskCode,
		handlers:  make(map[string]Handler),
		logger:    slog.Default(),
		prefix:    events.CommandSubject(kioskCode, ""),
	}

	d.handlers["inventory.adjust"] = d.handleInventoryAdjust
	d.handlers["inventory.snapshot"] = d.handleInventorySnapshot
	d.handlers["metrics.snapshot"] = d.handleMetricsSnapshot
	d.handlers["checkout.close"] = d.handleCheckoutClose
	d.handlers["instance.create"] = d.handleInstanceCreate
	d.handlers["instance.edit"] = d.handleInstanceEdit
	d.handlers["instance.set_status"] = d.handleInstanceSetStatus
	d.handlers["instance.snapshot"] = d.handleInstanceSnapshot
	d.handlers["integrity.rebuild"] = d.handleIntegrityRebuild
	d.handlers["ledger.republish"] = d.handleLedgerRepublish
	d.handlers["cart.start"] = d.handleCartStart
	d.handlers["read.trigger"] = d.handleReadTrigger
	return d
}

// HandleFunc registers (or overrides) a handler for the given command suffix.
// Tests and future commands use this; production wires the built-ins via the
// constructor.
func (d *Dispatcher) HandleFunc(name string, h Handler) {
	d.handlers[name] = h
}

// Register subscribes the dispatcher to every command targeting this kiosk.
// QueueSubscribe with one queue per kiosk is defense-in-depth: even if a
// second process accidentally connects with the same kiosk code, only one
// of them processes each command.
func (d *Dispatcher) Register(nc *nats.Conn) (*nats.Subscription, error) {
	if nc == nil {
		return nil, fmt.Errorf("nats conn is nil")
	}
	if d.kioskCode == "" {
		return nil, fmt.Errorf("kiosk code is empty")
	}
	pattern := events.CommandSubscribePattern(d.kioskCode)
	queue := "kiosk-" + d.kioskCode
	sub, err := nc.QueueSubscribe(pattern, queue, d.onMessage)
	if err != nil {
		return nil, fmt.Errorf("subscribe %s: %w", pattern, err)
	}
	d.sub = sub
	d.logger.Info("kiosk.commands.subscribed", "pattern", pattern, "queue", queue)
	return sub, nil
}

// Unsubscribe drops the subscription. Tests use this; production usually
// leaves it alone (the nats.go client drains on Close).
func (d *Dispatcher) Unsubscribe() {
	if d.sub != nil {
		_ = d.sub.Unsubscribe()
		d.sub = nil
	}
}

// onMessage is the NATS callback. Resolves the suffix, dispatches, replies.
// Bad-shape messages get a structured error reply rather than silence so
// the controller endpoint can render a useful error.
func (d *Dispatcher) onMessage(msg *nats.Msg) {
	if msg.Reply == "" {
		// No reply inbox — the publisher doesn't expect an answer. Treat as
		// fire-and-forget and skip dispatch entirely; we'd have nowhere to
		// signal validation failures and don't want to mutate state for a
		// caller that isn't listening.
		d.logger.Warn("kiosk.commands.missing_reply",
			"subject", msg.Subject)
		return
	}

	suffix := d.subjectSuffix(msg.Subject)
	h, ok := d.handlers[suffix]
	if !ok {
		d.reply(msg, Reply{Success: false, Error: "unknown command: " + suffix})
		return
	}

	// Defensive: if a handler panics, recover and reply with an error rather
	// than crashing the dispatcher goroutine. Commands are admin-driven and
	// rare; a single bad call shouldn't take the whole subscriber down.
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("kiosk.commands.panic",
				"subject", msg.Subject, "recovered", r)
			d.reply(msg, Reply{Success: false, Error: fmt.Sprintf("internal error: %v", r)})
		}
	}()

	out := h(context.Background(), msg.Data)
	d.reply(msg, out)
}

func (d *Dispatcher) subjectSuffix(subject string) string {
	// Subject is "<prefix>.<code>.command.<name>". TrimPrefix on the cached
	// "<prefix>.<code>.command." piece gives us "<name>" directly.
	return strings.TrimPrefix(subject, d.prefix)
}

func (d *Dispatcher) reply(msg *nats.Msg, r Reply) {
	data, err := json.Marshal(r)
	if err != nil {
		d.logger.Warn("kiosk.commands.marshal_failed", "error", err)
		return
	}
	if err := msg.Respond(data); err != nil {
		d.logger.Warn("kiosk.commands.respond_failed",
			"subject", msg.Subject, "error", err)
	}
}

// Built-in command handlers live in inventory.go (and any future commands.go
// files). The dispatcher routes by subject suffix into those methods.
