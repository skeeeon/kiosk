package handlers

import (
	"log"
	"time"

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
func (h *Handlers) stampObservedSighting(observed []rfid.EPC, zone, gateway string) {
	if zone == "" || len(observed) == 0 {
		return
	}
	now := time.Now().UTC()
	for _, epc := range observed {
		m, err := h.scanInstanceByRFID(string(epc))
		if err != nil || m == nil || m.Instance == nil {
			continue
		}
		if err := sightings.StampLastObserved(h.App, m.Instance.ID, zone, gateway, nil, nil, now); err != nil {
			log.Printf("sighting: stamp instance %s: %v", m.Instance.ID, err)
		}
	}
}
