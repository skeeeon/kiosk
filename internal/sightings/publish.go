package sightings

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/skeeeon/kiosk/internal/events"
)

// PublishCustodyReads emits one raw sighting per observed tag onto the node's
// OWN sighting subject — used in MANAGED mode so the controller aggregates
// custody-derived location exactly as it does external gateway sightings
// (location/sightings L3). Same raw wire shape (carries tag_id; the controller
// resolves via its EPC index).
//
// Publishes via the raw publisher's PublishBytes, NOT events.Publish: a sighting
// is not an event — it must stay out of the durable stream and must not emit the
// "kiosk.event" slog line. Best-effort: a publish failure logs and drops (the
// node has already stamped its own copy locally, and sightings are lossy).
func PublishCustodyReads(kioskCode, zone, gateway string, tagIDs []string, observedAt time.Time) {
	pub := events.CurrentPublisher()
	if pub == nil || kioskCode == "" || len(tagIDs) == 0 {
		return
	}
	subj := events.SightingSubject(kioskCode)
	for _, tag := range tagIDs {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		data, err := json.Marshal(events.SightingPayload{
			TagID:      tag,
			GatewayID:  gateway,
			Zone:       zone,
			ObservedAt: observedAt,
		})
		if err != nil {
			continue
		}
		if err := pub.PublishBytes(subj, data); err != nil {
			slog.Warn("sighting.publish_failed", "subject", subj, "tag_id", tag, "error", err)
		}
	}
}
