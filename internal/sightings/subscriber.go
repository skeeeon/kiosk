package sightings

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
)

// Subscribe wires a standalone node's OWN sighting subject to local ingest. The
// node subscribes to events.SightingSubject(nodeCode) — its own subject only,
// never the fleet — parses each raw sighting and resolves+stamps it locally via
// the scan resolver's EPC lookup.
//
// Standalone mode only: in managed mode the controller is the sole sighting
// subscriber (it resolves via an EPC index and mirrors last-observed back down
// via KV). Plain core NATS — sightings ride outside the durable stream. The
// handler is a thin wrapper; all logic lives in the pure ApplySighting.
func Subscribe(nc *nats.Conn, app core.App, nodeCode string, lookup EPCLookup) (*nats.Subscription, error) {
	subj := events.SightingSubject(nodeCode)
	sub, err := nc.Subscribe(subj, func(msg *nats.Msg) {
		var p events.SightingPayload
		if err := json.Unmarshal(msg.Data, &p); err != nil {
			slog.Warn("sighting.ingest.bad_payload", "subject", msg.Subject, "error", err)
			return
		}
		if err := ApplySighting(app, lookup, p); err != nil {
			slog.Warn("sighting.ingest.apply_failed",
				"subject", msg.Subject, "tag_id", p.TagID, "error", err)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("subscribe %s: %w", subj, err)
	}
	slog.Info("sighting.subscribed", "subject", subj)
	return sub, nil
}
