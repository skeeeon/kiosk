// Package sightings owns advisory "last observed location" state for serialized
// item_instances.
//
// A sighting is a passive observation — a gateway or a custody reader saw a tag
// at a zone (and maybe GPS) at a time. Last-observed is the latest sighting per
// instance, materialized onto item_instances.last_observed_*. It is ADVISORY,
// lossy, last-write-wins: nothing here gates, blocks, or mutates a custody
// transaction. The worst case for a wrong/missing sighting is a stale "last
// seen" cell, never a failed checkout. (Same stance as the fleet-replica
// clock-out gate being fail-open.)
//
// This is a LEAF package (PB core + dbx + types only — no cart/commit/handlers
// imports) so the handler/command/controller layers can drive it without an
// import cycle, mirroring the internal/timeclock and internal/instances/status
// precedent.
package sightings

import (
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// StampLastObserved monotonically records the latest sighting of one instance
// onto item_instances.last_observed_*. It advances only when observedAt is newer
// than the stored value, so an out-of-order or redelivered sighting is a no-op
// (the WHERE clause enforces it). All five columns track the LATEST sighting's
// attributes together: lat/lon are nil for zone-only sightings (e.g. a custody
// reader) and stored as 0 — PB number columns are NOT NULL DEFAULT 0, so 0,0
// reads as "the latest sighting carried no GPS" (the deferred off-site-polygon
// check in L4 treats it as such). A later GPS-less observation correctly resets
// any prior coordinates back to 0,0.
//
// The raw-SQL UPDATE deliberately bypasses the item_instances model hooks:
// last-observed has nothing to do with quantity, so a stamp must not trigger a
// RecomputeItemQuantity. A read can resolve hundreds of tags; we don't want
// hundreds of spurious recomputes. Empty instanceID is a no-op.
func StampLastObserved(app core.App, instanceID, zone, gateway string, lat, lon *float64, observedAt time.Time) error {
	if instanceID == "" {
		return nil
	}

	dt, err := types.ParseDateTime(observedAt.UTC())
	if err != nil {
		return fmt.Errorf("parse observed_at: %w", err)
	}
	ts := dt.String() // PB's canonical "2006-01-02 15:04:05.000Z" — sorts lexically.

	// PB number columns are NOT NULL DEFAULT 0, so absent GPS is stored as 0.
	latV, lonV := 0.0, 0.0
	if lat != nil {
		latV = *lat
	}
	if lon != nil {
		lonV = *lon
	}

	// Monotonic guard: PB stores an unset DateField as "" (not NULL), so test
	// both before comparing lexically.
	_, err = app.DB().NewQuery(`
		UPDATE item_instances
		SET last_observed_at = {:ts},
		    last_observed_zone = {:zone},
		    last_observed_gateway = {:gateway},
		    last_observed_lat = {:lat},
		    last_observed_lon = {:lon}
		WHERE id = {:id}
		  AND (last_observed_at IS NULL OR last_observed_at = '' OR last_observed_at < {:ts})
	`).Bind(dbx.Params{
		"ts":      ts,
		"zone":    zone,
		"gateway": gateway,
		"lat":     latV,
		"lon":     lonV,
		"id":      instanceID,
	}).Execute()
	if err != nil {
		return fmt.Errorf("stamp last_observed for instance %s: %w", instanceID, err)
	}
	return nil
}
