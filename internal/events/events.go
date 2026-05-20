// Package events is the single integration point for the kiosk's emitted
// events. v1 writes a structured log line per event. v2 will add a NATS
// publish here, gated by a config flag introduced at that time. Subject
// names already follow NATS-style hierarchical naming so the transition
// requires no schema or contract changes upstream.
package events

import (
	"encoding/json"
	"log/slog"
)

// Publish emits an event. In v1 this is a slog.Info; in v2 it will also
// publish to NATS. The signature is intentionally narrow (no error return)
// because callers cannot meaningfully recover from a log failure and
// shouldn't bake retry logic into transaction commit paths.
func Publish(subject string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("kiosk.event.marshal_failed", "subject", subject, "error", err)
		return
	}
	slog.Info("kiosk.event", "subject", subject, "payload", json.RawMessage(raw))
}
