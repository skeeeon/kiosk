// Package reconcile computes custody-vs-location discrepancies — the product
// value of the location/sightings feature (docs/location-sightings-plan.md, L4).
//
// Custody (who has what, from the append-only ledger) and location (where a unit
// was last seen, from advisory sightings) are orthogonal; the gap between them
// is the alert. This package is the PURE join + flagging logic — no I/O, no PB,
// no time.Now (the caller passes `now`) — so it is exhaustively table-testable
// and runs identically on a standalone node (local data) and the controller
// (fleet data). It is OBSERVABILITY ONLY: it surfaces discrepancies, it never
// enforces, blocks, or mutates anything.
package reconcile

import "time"

// Kind enumerates the discrepancy classes v1 surfaces.
const (
	// KindNotTaken: a unit is checked out to a worker but its last sighting is
	// still in a custody zone (cabinet/counter) — maybe never physically taken.
	KindNotTaken = "not_taken"
	// KindStale: a unit is checked out but hasn't been seen anywhere in longer
	// than the staleness threshold — maybe lost.
	KindStale = "stale"
	// KindUnaccounted: a unit was observed somewhere that is NOT a custody zone
	// while NOT in custody — unaccounted movement / theft signal.
	KindUnaccounted = "unaccounted"
)

// CustodyState is one unit currently checked out (from the ledger).
type CustodyState struct {
	KioskCode    string
	InstanceID   string // the owning node's item_instances.id — the join key
	InstanceCode string
	ItemName     string
	Holder       string // worker name/code holding it
	CheckedOutAt time.Time
}

// LocationState is one unit's latest advisory sighting.
type LocationState struct {
	KioskCode    string
	InstanceID   string
	InstanceCode string
	ItemName     string
	Zone         string
	ObservedAt   time.Time
}

// Config tunes the flags. CustodyZones is the set of zone labels that count as
// "still in storage" (normalize keys to how zones are stored). StaleAfter is the
// out-but-unseen threshold.
type Config struct {
	StaleAfter   time.Duration
	CustodyZones map[string]bool
}

// Discrepancy is one flagged row for the report/view.
type Discrepancy struct {
	Kind         string    `json:"kind"`
	KioskCode    string    `json:"kiosk_code"`
	InstanceCode string    `json:"instance_code"`
	ItemName     string    `json:"item_name"`
	Holder       string    `json:"holder,omitempty"`
	Zone         string    `json:"zone,omitempty"`
	ObservedAt   time.Time `json:"observed_at,omitempty"`
}

func key(kioskCode, instanceID string) string { return kioskCode + "\x00" + instanceID }

// Reconcile joins custody and location by (kiosk_code, instance_id) and returns
// the discrepancies. Pure: `now` and `cfg` are supplied. A unit out but with no
// location data at all is intentionally NOT flagged (sparse gateways are common;
// "out and never seen" would be noise). Precedence: not-taken before stale.
func Reconcile(custody []CustodyState, location []LocationState, cfg Config, now time.Time) []Discrepancy {
	locByKey := make(map[string]LocationState, len(location))
	for _, l := range location {
		locByKey[key(l.KioskCode, l.InstanceID)] = l
	}
	inCustody := make(map[string]struct{}, len(custody))
	for _, c := range custody {
		inCustody[key(c.KioskCode, c.InstanceID)] = struct{}{}
	}

	var out []Discrepancy

	// Custody-driven flags: not-taken and stale.
	for _, c := range custody {
		loc, ok := locByKey[key(c.KioskCode, c.InstanceID)]
		if !ok {
			continue // no location data — not a discrepancy we can assert
		}
		switch {
		case loc.Zone != "" && cfg.CustodyZones[loc.Zone]:
			out = append(out, Discrepancy{
				Kind: KindNotTaken, KioskCode: c.KioskCode, InstanceCode: c.InstanceCode,
				ItemName: c.ItemName, Holder: c.Holder, Zone: loc.Zone, ObservedAt: loc.ObservedAt,
			})
		case !loc.ObservedAt.IsZero() && cfg.StaleAfter > 0 && now.Sub(loc.ObservedAt) > cfg.StaleAfter:
			out = append(out, Discrepancy{
				Kind: KindStale, KioskCode: c.KioskCode, InstanceCode: c.InstanceCode,
				ItemName: c.ItemName, Holder: c.Holder, Zone: loc.Zone, ObservedAt: loc.ObservedAt,
			})
		}
	}

	// Location-driven flag: unaccounted (seen off a custody zone, not in custody).
	for _, l := range location {
		if _, out2 := inCustody[key(l.KioskCode, l.InstanceID)]; out2 {
			continue
		}
		if l.Zone == "" || cfg.CustodyZones[l.Zone] {
			continue // in a custody zone (or zoneless) while idle is normal
		}
		out = append(out, Discrepancy{
			Kind: KindUnaccounted, KioskCode: l.KioskCode, InstanceCode: l.InstanceCode,
			ItemName: l.ItemName, Zone: l.Zone, ObservedAt: l.ObservedAt,
		})
	}

	return out
}

// CustodyZoneSet builds a normalized set from a slice of zone labels.
func CustodyZoneSet(zones []string) map[string]bool {
	set := make(map[string]bool, len(zones))
	for _, z := range zones {
		if z != "" {
			set[z] = true
		}
	}
	return set
}
