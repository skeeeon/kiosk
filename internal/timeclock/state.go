// Package timeclock is the single write funnel and derived-state reader for
// the append-only time_punches ledger. It is a LEAF package by design —
// imports only events/kioskctx/dberr and the PB core — so internal/commit
// can consult clocked-in state without an import cycle (same precedent as
// internal/instances/status). Never import cart, commit, or handlers from
// here.
//
// There is deliberately no materialized "open shifts" table: "is this user
// clocked in" is the latest punch per user by occurred_at (created breaks
// ties), merged with the fleet replica in managed mode. One query, nothing
// to keep honest.
package timeclock

import (
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// Collection is the PB collection name of the punch ledger.
const Collection = "time_punches"

// Punch sources. Richer than events.SourceLocal/SourceController because a
// punch carries "who physically initiated it": the worker themselves, a
// foreman punching a crew member (punch-now only), a local admin, or a
// controller admin via the timeclock.punch command. Values match the
// time_punches.source select field.
const (
	SourceSelf            = "self"
	SourceForeman         = "foreman"
	SourceAdmin           = "admin"
	SourceControllerAdmin = "controller_admin"
)

// Punch directions. Values match the time_punches.direction select field.
const (
	DirectionIn  = "in"
	DirectionOut = "out"
)

// PunchStateBucket is the JetStream KV bucket the controller projects
// per-user clocked-in state into, broadcast-keyed by user_code (users are
// org-wide — same keying as catalog_users). Kiosks in managed mode watch it
// into a Fleet.
const PunchStateBucket = "punch_state"

// PunchStatePayload is the KV value shape — single source of truth for both
// the controller's writer and the kiosk's watcher (catalog/payload.go
// precedent, inlined here because it's one struct).
type PunchStatePayload struct {
	UserCode      string    `json:"user_code"`
	ClockedIn     bool      `json:"clocked_in"`
	OccurredAt    time.Time `json:"occurred_at"`
	SourcePunchID string    `json:"source_punch_id"`
}

// Fleet is the kiosk-local in-memory replica of the punch_state bucket.
// Hydrated by the watcher (WatchAll replays the full bucket on start, so a
// process restart recovers fleet state without local persistence). Reads are
// nil-safe so standalone kiosks pass a nil *Fleet everywhere and degrade to
// local-only state by construction.
type Fleet struct {
	mu sync.RWMutex
	m  map[string]PunchStatePayload
}

func NewFleet() *Fleet {
	return &Fleet{m: make(map[string]PunchStatePayload)}
}

// Get returns the replica entry for a user code. Nil-safe.
func (f *Fleet) Get(userCode string) (PunchStatePayload, bool) {
	if f == nil {
		return PunchStatePayload{}, false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	e, ok := f.m[userCode]
	return e, ok
}

// Upsert applies an entry monotonically: an incoming state older than (or
// equal to) what we already hold is dropped, so KV redelivery and
// out-of-order watcher events can never move a user's state backwards.
// Returns whether the entry was applied.
func (f *Fleet) Upsert(e PunchStatePayload) bool {
	if f == nil || e.UserCode == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.m[e.UserCode]; ok && !e.OccurredAt.After(cur.OccurredAt) {
		return false
	}
	f.m[e.UserCode] = e
	return true
}

// Delete removes a user's replica entry (KV key deletion — e.g. a user
// purged fleet-wide).
func (f *Fleet) Delete(userCode string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, userCode)
}

// State is the merged clocked-in view for one user.
type State struct {
	ClockedIn  bool      `json:"clocked_in"`
	OccurredAt time.Time `json:"occurred_at"` // zero when the user has never punched anywhere
	Origin     string    `json:"origin"`      // "local" | "fleet" | "" (never punched)
}

// LatestPunch returns the user's most recent punch row by occurred_at
// (created as tie-break, so a same-timestamp correction recorded later
// wins). nil, nil when the user has never punched at this kiosk.
func LatestPunch(app core.App, userID string) (*core.Record, error) {
	recs, err := app.FindRecordsByFilter(Collection,
		"user = {:u}", "-occurred_at,-created", 1, 0, dbx.Params{"u": userID})
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return recs[0], nil
}

// CurrentState is THE merge rule — every clocked-in decision (commit
// interlock, punch alternation, status endpoint) goes through here. Local
// ledger and fleet replica are compared by occurred_at and the fresher one
// wins. With a nil fleet (standalone) this is just the local latest punch.
func CurrentState(app core.App, fleet *Fleet, userID, userCode string) (State, error) {
	var s State
	p, err := LatestPunch(app, userID)
	if err != nil {
		return s, err
	}
	if p != nil {
		s.ClockedIn = p.GetString("direction") == DirectionIn
		s.OccurredAt = p.GetDateTime("occurred_at").Time()
		s.Origin = "local"
	}
	// A punch made HERE round-trips kiosk → controller → punch_state KV and
	// lands back in the replica. If the fleet entry is the local latest punch
	// itself, it's not "another kiosk" — and a precision mismatch (the DB
	// stores ms; older deployments published the event timestamp at ns) must
	// not make the echo look strictly fresher than its own local row.
	if e, ok := fleet.Get(userCode); ok && e.OccurredAt.After(s.OccurredAt) &&
		(p == nil || e.SourcePunchID != p.Id) {
		s.ClockedIn = e.ClockedIn
		s.OccurredAt = e.OccurredAt
		s.Origin = "fleet"
	}
	return s, nil
}
