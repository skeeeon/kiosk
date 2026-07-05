package controller

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
)

// HeartbeatRegistry is the controller-side store of kiosk liveness. The
// aggregator's durable consumer is intentionally not used for heartbeats:
// beats are last-write-wins and missing one is the entire point of the
// signal — durability would only mask outages. So the registry sits on a
// plain core-NATS subscription and a mutex-guarded map.
//
// Concurrency: every NATS message handler is invoked on a goroutine the
// client owns, so we always lock around the map. Reads (Snapshot) copy out
// to avoid handing live state to HTTP encoders.
type HeartbeatRegistry struct {
	startedAt time.Time

	mu     sync.RWMutex
	beats  map[string]time.Time
	touch  TouchFn // optional auto-register callback, may be nil
	logger *slog.Logger

	sub *nats.Subscription // active subscription, set by Subscribe
}

// TouchFn is the auto-register hook the registry calls on the first beat
// from a previously-unknown kiosk. Today this points at the aggregator's
// TouchKiosk so kiosks that haven't yet completed a transaction still show
// up in the controller's kiosks collection. nil is allowed (tests).
type TouchFn func(kioskCode, locationCode string) error

// NewHeartbeatRegistry constructs an empty registry. controllerStartedAt is
// captured at construction; the SPA reads it back so it can render "unknown"
// instead of "offline" during the ~90s window after a controller restart.
func NewHeartbeatRegistry(touch TouchFn) *HeartbeatRegistry {
	return &HeartbeatRegistry{
		startedAt: time.Now().UTC(),
		beats:     make(map[string]time.Time),
		touch:     touch,
		logger:    slog.Default(),
	}
}

// Subscribe wires the registry to NATS. Returns the underlying subscription
// so the caller can hold a reference (don't unsubscribe on reconnect — the
// nats.go client re-establishes the sub itself).
func (r *HeartbeatRegistry) Subscribe(nc *nats.Conn) (*nats.Subscription, error) {
	if nc == nil {
		return nil, fmt.Errorf("nats conn is nil")
	}
	sub, err := nc.Subscribe(events.HeartbeatFilter(), r.handle)
	if err != nil {
		return nil, fmt.Errorf("subscribe %s: %w", events.HeartbeatFilter(), err)
	}
	r.sub = sub
	r.logger.Info("controller.heartbeat.subscribed", "pattern", events.HeartbeatFilter())
	return sub, nil
}

// Unsubscribe drops the subscription. Tests use this; production usually
// leaves it alone (nats.go drains on Close).
func (r *HeartbeatRegistry) Unsubscribe() {
	if r.sub != nil {
		_ = r.sub.Unsubscribe()
		r.sub = nil
	}
}

// handle is the NATS callback. Parses the payload, records the beat, and
// fires the auto-register hook on the first beat from a kiosk we haven't
// seen this process lifetime. Robust against malformed payloads — they log
// and drop.
func (r *HeartbeatRegistry) handle(msg *nats.Msg) {
	var p struct {
		Ts       time.Time `json:"ts"`
		Code     string    `json:"code"`
		Location string    `json:"location"`

		// Legacy field names from kiosk builds that predate the
		// access-control payload alignment ({code, location, ts}).
		// Drop once the fleet no longer runs those builds.
		KioskCode    string `json:"kiosk_code"`
		LocationCode string `json:"location_code"`
	}
	if err := json.Unmarshal(msg.Data, &p); err != nil {
		r.logger.Warn("controller.heartbeat.bad_payload",
			"subject", msg.Subject, "error", err)
		return
	}
	if p.Code == "" {
		p.Code = p.KioskCode
	}
	if p.Location == "" {
		p.Location = p.LocationCode
	}
	if p.Code == "" {
		r.logger.Warn("controller.heartbeat.missing_kiosk_code", "subject", msg.Subject)
		return
	}
	if p.Ts.IsZero() {
		p.Ts = time.Now().UTC()
	}

	// Auto-register on first beat. Cheap to call on every beat (the aggregator's
	// touchKiosk is an upsert), but checking the map first avoids a DB roundtrip
	// per beat for known kiosks.
	r.mu.Lock()
	_, known := r.beats[p.Code]
	r.beats[p.Code] = p.Ts
	r.mu.Unlock()

	if !known && r.touch != nil {
		if err := r.touch(p.Code, p.Location); err != nil {
			r.logger.Warn("controller.heartbeat.auto_register_failed",
				"kiosk_code", p.Code, "error", err)
		}
	}
}

// RecordBeat lets tests stuff entries in without going through NATS. Real
// callers shouldn't need this.
func (r *HeartbeatRegistry) RecordBeat(kioskCode string, ts time.Time) {
	r.mu.Lock()
	r.beats[kioskCode] = ts
	r.mu.Unlock()
}

// Snapshot returns a copy of the current beats map. Returning a copy avoids
// races with the HTTP encoder if a beat arrives mid-serialization.
func (r *HeartbeatRegistry) Snapshot() map[string]time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]time.Time, len(r.beats))
	for k, v := range r.beats {
		out[k] = v
	}
	return out
}

// StartedAt returns when this registry was constructed. The SPA compares
// "now - startedAt" to the online/stale thresholds so it can show "unknown"
// during the first ~90s of a controller restart.
func (r *HeartbeatRegistry) StartedAt() time.Time { return r.startedAt }

// LastBeat returns the most recent beat for the given kiosk and whether it
// has ever been seen. Used by the inventory adjust endpoint for fast-fail.
func (r *HeartbeatRegistry) LastBeat(kioskCode string) (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.beats[kioskCode]
	return t, ok
}

// IsLikelyOnline returns true when the last beat for the kiosk is within
// the freshness window. Used by command endpoints to short-circuit with
// "kiosk offline" before paying a 5s NATS request timeout.
func (r *HeartbeatRegistry) IsLikelyOnline(kioskCode string, freshness time.Duration) bool {
	t, ok := r.LastBeat(kioskCode)
	if !ok {
		return false
	}
	return time.Since(t) <= freshness
}

// heartbeatResponse is the JSON shape returned by the heartbeats endpoint.
// Per-kiosk timestamps are ISO 8601 (RFC3339) for SPA-friendly parsing.
type heartbeatResponse struct {
	ControllerStartedAt time.Time            `json:"controller_started_at"`
	Kiosks              map[string]time.Time `json:"kiosks"`
}

// HeartbeatsEndpoint returns a request handler for GET /api/controller/kiosks/heartbeats.
// Reuses the controller's requireAdmin gate so only authenticated admins can
// see the fleet's liveness map.
func (h *Handlers) HeartbeatsEndpoint(reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		return re.JSON(http.StatusOK, heartbeatResponse{
			ControllerStartedAt: reg.StartedAt(),
			Kiosks:              reg.Snapshot(),
		})
	}
}
