// Package heartbeat is the kiosk-side heartbeat emitter. Every kiosk runs
// one goroutine that publishes a small JSON beacon on a NATS subject at a
// fixed cadence. The controller subscribes plainly (no JetStream) and
// tracks the latest timestamp per kiosk in memory — that map drives the
// "is this kiosk online?" indicator in the controller SPA.
//
// Heartbeat is core NATS, not JetStream, on purpose: a missed beat is the
// signal we care about, so durability/replay would be actively harmful. If
// the kiosk reconnects after a 10-minute outage, the next beat happens at
// the next tick — there's no backlog to drain.
package heartbeat

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/skeeeon/kiosk/internal/events"
)

// DefaultInterval is the publish cadence. 45s plus a 90s "online" window on
// the controller means we tolerate exactly one missed beat before the SPA
// flips a kiosk to "stale". Picked over 30s/60s to keep traffic light.
const DefaultInterval = 45 * time.Second

// Payload is the JSON shape published on each beat. Minimal on purpose —
// the controller's interest is "did this arrive recently?", not what the
// payload says. The field names match the access-control heartbeat shape
// ({code, location, ts}) so fleet tooling can consume both.
type Payload struct {
	Code     string    `json:"code"`
	Location string    `json:"location"`
	Ts       time.Time `json:"ts"`
}

// Start launches the heartbeat goroutine. Returns immediately; the goroutine
// runs until ctx is cancelled. nc may be nil — in that case the function
// is a no-op (the kiosk is allowed to boot without NATS). Errors during
// publish are logged at warn and otherwise swallowed: the next tick retries.
func Start(ctx context.Context, nc *nats.Conn, kioskCode, locationCode string) {
	StartWithInterval(ctx, nc, kioskCode, locationCode, DefaultInterval)
}

// StartWithInterval is Start with a configurable cadence. Tests use this to
// avoid waiting 45 seconds for a single beat.
func StartWithInterval(ctx context.Context, nc *nats.Conn, kioskCode, locationCode string, interval time.Duration) {
	if nc == nil || kioskCode == "" {
		return
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	subject := events.HeartbeatSubject(kioskCode)

	go func() {
		// First beat right away so the controller learns about us on
		// startup, not 45 seconds later.
		publishOne(nc, subject, kioskCode, locationCode)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				publishOne(nc, subject, kioskCode, locationCode)
			}
		}
	}()
}

func publishOne(nc *nats.Conn, subject, kioskCode, locationCode string) {
	payload := Payload{
		Code:     kioskCode,
		Location: locationCode,
		Ts:       time.Now().UTC().Truncate(time.Second),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("kiosk.heartbeat.marshal_failed", "error", err)
		return
	}
	if err := nc.Publish(subject, data); err != nil {
		slog.Warn("kiosk.heartbeat.publish_failed", "subject", subject, "error", err)
	}
}
