// Package events is the single integration point for the kiosk's emitted
// events. v1 wrote a structured log line per event; v2 adds an optional
// NATS publish using subjects that already followed NATS hierarchy.
//
// The publisher is global because every commit path calls Publish without
// threading a handle. Tests inject a fake via SetPublisher.
package events

import (
	"encoding/json"
	"log/slog"
	"sync/atomic"
)

// publisher holds the active Publisher (or nil when disabled). Atomic so
// tests can swap it without locking; production sets it once at startup.
var publisher atomic.Pointer[Publisher]

// SetPublisher installs the global publisher. Pass nil (or call with no
// publisher set) to keep events slog-only.
func SetPublisher(p Publisher) {
	if p == nil {
		publisher.Store(nil)
		return
	}
	publisher.Store(&p)
}

// CurrentPublisher returns the active publisher or nil. Mainly for tests
// and shutdown wiring.
func CurrentPublisher() Publisher {
	p := publisher.Load()
	if p == nil {
		return nil
	}
	return *p
}

// Publish emits an event. Always logs via slog; if a publisher is set,
// also publishes JSON-encoded payload to the subject. The signature is
// intentionally narrow (no error return) — callers (commit hook,
// post-DB-transaction) cannot meaningfully recover, and we don't want to
// bake retry logic into write paths. Publish failures land in slog at
// warn level.
func Publish(subject string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("kiosk.event.marshal_failed", "subject", subject, "error", err)
		return
	}
	slog.Info("kiosk.event", "subject", subject, "payload", json.RawMessage(raw))

	if p := CurrentPublisher(); p != nil {
		if err := p.PublishJSON(subject, payload); err != nil {
			slog.Warn("kiosk.event.publish_failed", "subject", subject, "error", err)
		}
	}
}
