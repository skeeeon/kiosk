package sightings_test

import (
	"testing"
	"time"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/sightings"
)

// fakeLookup resolves a single known tag id to a fixed instance id.
func fakeLookup(knownTag, instanceID string) sightings.EPCLookup {
	return func(tag string) (string, bool) {
		if tag == knownTag {
			return instanceID, true
		}
		return "", false
	}
}

func TestApplySighting_StampsResolved(t *testing.T) {
	app := setupApp(t)
	id := seedInstance(t, app) // rfid_epc seeded as "e280-aaa"

	when := time.Date(2026, 6, 27, 8, 0, 0, 0, time.UTC)
	p := events.SightingPayload{
		TagID:      "e280-aaa",
		GatewayID:  "gw-yard-1",
		Zone:       "Yard",
		ObservedAt: when,
	}
	if err := sightings.ApplySighting(app, fakeLookup("e280-aaa", id), p); err != nil {
		t.Fatalf("ApplySighting: %v", err)
	}
	at, zone, gw := observedFields(t, app, id)
	if at == "" || zone != "Yard" || gw != "gw-yard-1" {
		t.Fatalf("want zone=Yard gateway=gw-yard-1 non-empty at; got at=%q zone=%q gw=%q", at, zone, gw)
	}
}

func TestApplySighting_UnknownTagNoOp(t *testing.T) {
	app := setupApp(t)
	id := seedInstance(t, app)

	p := events.SightingPayload{TagID: "no-such-tag", Zone: "Yard", ObservedAt: time.Now().UTC()}
	if err := sightings.ApplySighting(app, fakeLookup("e280-aaa", id), p); err != nil {
		t.Fatalf("ApplySighting: %v", err)
	}
	at, zone, _ := observedFields(t, app, id)
	if at != "" || zone != "" {
		t.Fatalf("unknown tag should stamp nothing; got at=%q zone=%q", at, zone)
	}
}

func TestApplySighting_MissingObservedAtDefaultsNow(t *testing.T) {
	app := setupApp(t)
	id := seedInstance(t, app)

	// No ObservedAt — ApplySighting defaults it to now so the stamp still lands.
	p := events.SightingPayload{TagID: "e280-aaa", Zone: "Bay 3"}
	if err := sightings.ApplySighting(app, fakeLookup("e280-aaa", id), p); err != nil {
		t.Fatalf("ApplySighting: %v", err)
	}
	at, zone, _ := observedFields(t, app, id)
	if at == "" || zone != "Bay 3" {
		t.Fatalf("missing observed_at should default to now and stamp; got at=%q zone=%q", at, zone)
	}
}

func TestApplySighting_MonotonicOlderDropped(t *testing.T) {
	app := setupApp(t)
	id := seedInstance(t, app)

	newer := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	if err := sightings.ApplySighting(app, fakeLookup("e280-aaa", id),
		events.SightingPayload{TagID: "e280-aaa", Zone: "Yard", GatewayID: "gw1", ObservedAt: newer}); err != nil {
		t.Fatalf("ApplySighting newer: %v", err)
	}
	// An older sighting (e.g. redelivered) must not overwrite the newer one.
	if err := sightings.ApplySighting(app, fakeLookup("e280-aaa", id),
		events.SightingPayload{TagID: "e280-aaa", Zone: "Stale", GatewayID: "gw-old", ObservedAt: newer.Add(-time.Hour)}); err != nil {
		t.Fatalf("ApplySighting older: %v", err)
	}
	_, zone, gw := observedFields(t, app, id)
	if zone != "Yard" || gw != "gw1" {
		t.Fatalf("older sighting should be a no-op; got zone=%q gw=%q", zone, gw)
	}
}
