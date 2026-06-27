package handlers

import (
	"log"
	"time"

	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/rfid"
	"github.com/skeeeon/kiosk/internal/sightings"
)

// stampObservedSighting records advisory last-observed location for every
// instance the reader resolved this cycle, at the reader's configured zone. A
// custody read is also a location signal — the tool was demonstrably AT this
// reader (docs/location-sightings-plan.md, L1, D8).
//
// It stamps the FULL observed set (resolved straight from the EPCs), not just
// the cart lines: a present-but-unchanged unit was still seen here. No-op when
// the reader has no zone (location not configured) — keeps N=1 invisible.
// Advisory + best-effort: a resolve/stamp error is logged and skipped, never
// failing the read. Does NOT tickle the cart SSE — the read path already
// tickles once, and a silent side-effect shouldn't trigger a refetch.
// LookupInstanceIDByTag resolves a raw sighting tag to a local item_instances id
// for the standalone sighting subscriber (sightings.EPCLookup). Source-agnostic:
// it tries the RFID EPC first, then the BLE beacon id, reusing the exact lookups
// the scan resolver uses — node-side resolution is reuse, not new code. Returns
// ok=false on miss / empty / error (advisory: an unknown tag is dropped).
func (h *Handlers) LookupInstanceIDByTag(tag string) (string, bool) {
	if m, err := h.scanInstanceByRFID(tag); err == nil && m != nil && m.Instance != nil {
		return m.Instance.ID, true
	}
	if m, err := h.scanInstanceByBLE(tag); err == nil && m != nil && m.Instance != nil {
		return m.Instance.ID, true
	}
	return "", false
}

func (h *Handlers) stampObservedSighting(observed []rfid.EPC, zone, gateway string) {
	if zone == "" || len(observed) == 0 {
		return
	}
	now := time.Now().UTC()

	// Local stamp for every resolvable tag (L1 — works with zero NATS).
	for _, epc := range observed {
		m, err := h.scanInstanceByRFID(string(epc))
		if err != nil || m == nil || m.Instance == nil {
			continue
		}
		if err := sightings.StampLastObserved(h.App, m.Instance.ID, zone, gateway, nil, nil, now); err != nil {
			log.Printf("sighting: stamp instance %s: %v", m.Instance.ID, err)
		}
	}

	// Managed mode: also publish each observed tag as a raw sighting so the
	// controller aggregates custody-derived location and mirrors it fleet-wide
	// (L3). Same raw wire shape as an external gateway; the controller resolves
	// via its EPC index. The local stamp above already covered this node, so
	// the mirror-back is an idempotent monotonic no-op.
	if h.Cfg.Controller.Enabled {
		tags := make([]string, len(observed))
		for i, epc := range observed {
			tags[i] = string(epc)
		}
		sightings.PublishCustodyReads(kioskctx.Get().KioskCode, zone, gateway, tags, now)
	}
}
