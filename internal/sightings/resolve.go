package sightings

import (
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
)

// EPCLookup resolves a raw tag id (RFID EPC, BLE beacon id later) to a local
// item_instances row id. ok=false when no instance matches — an unknown tag,
// which is dropped (advisory: a dropped sighting self-heals on the next read).
// On the node it is wired to the same lookup the scan resolver uses
// (Handlers.LookupInstanceIDByEPC → scanInstanceByRFID), so node-side
// resolution is reuse, not new code.
type EPCLookup func(tagID string) (instanceID string, ok bool)

// ApplySighting resolves one raw sighting to a local instance and monotonically
// stamps its last-observed. Pure, app-only, NATS-free — the standalone node
// subscriber and tests drive it directly (the established pattern: logic in a
// pure function, the NATS wrapper thin). Empty/unknown tag → no-op. A sighting
// with no observed_at defaults to now (advisory, lossy).
func ApplySighting(app core.App, lookup EPCLookup, p events.SightingPayload) error {
	tag := strings.TrimSpace(p.TagID)
	if tag == "" || lookup == nil {
		return nil
	}
	instanceID, ok := lookup(tag)
	if !ok || instanceID == "" {
		return nil
	}
	when := p.ObservedAt
	if when.IsZero() {
		when = time.Now().UTC()
	}
	return StampLastObserved(app, instanceID, p.Zone, p.GatewayID, p.Lat, p.Lon, when)
}
