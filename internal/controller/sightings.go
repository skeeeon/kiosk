package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/sightings"
)

// SightingIngest is the controller's fleet-wide location aggregator. Like the
// heartbeat registry, it sits on a PLAIN core-NATS subscription (NOT the durable
// consumer): sightings are lossy and last-write-wins, so durability would only
// buffer staleness. It resolves each raw sighting to its owning unit via
// instance_epc_index, upserts the site-wide instance_location view, and mirrors
// the unit's last-observed back down via the last_observed_state KV bucket so
// the owning node sees sightings made by other nodes' gateways.
//
// The work is gated by a movement-not-reads dedup: a sighting whose
// (zone|gps-bucket) matches the last one for that tag is dropped before any DB
// or KV write, so steady-state read volume (a tool sitting still) costs nothing.
type SightingIngest struct {
	app        core.App
	locationKV jetstream.KeyValue // last_observed_state bucket; nil if provisioning failed (mirror skipped)
	logger     *slog.Logger

	mu      sync.Mutex
	lastKey map[string]string // tag_id → last (zone|gps) key seen, for the dedup gate

	sub *nats.Subscription
}

// NewSightingIngest constructs the ingest and provisions the last_observed_state
// KV bucket (best-effort — a failure degrades to no fleet mirror, the
// instance_location view still updates).
func NewSightingIngest(ctx context.Context, app core.App, js jetstream.JetStream) *SightingIngest {
	ing := &SightingIngest{
		app:     app,
		logger:  slog.Default(),
		lastKey: make(map[string]string),
	}
	if js != nil {
		if kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:      sightings.LastObservedStateBucket,
			Description: "Per-unit latest sighting, keyed <kiosk_code>.<instance_id>. Written by the controller's SightingIngest, watched own-slice-only by managed nodes.",
			History:     1,
		}); err != nil {
			slog.Warn("controller.sighting.bucket_failed", "error", err)
		} else {
			ing.locationKV = kv
		}
	}
	return ing
}

// Subscribe wires the ingest to the fleet-wide sighting filter. Plain core NATS,
// like heartbeats — not the durable consumer.
func (s *SightingIngest) Subscribe(nc *nats.Conn) (*nats.Subscription, error) {
	if nc == nil {
		return nil, fmt.Errorf("nats conn is nil")
	}
	sub, err := nc.Subscribe(events.SightingFilter(), s.handle)
	if err != nil {
		return nil, fmt.Errorf("subscribe %s: %w", events.SightingFilter(), err)
	}
	s.sub = sub
	s.logger.Info("controller.sighting.subscribed", "pattern", events.SightingFilter())
	return sub, nil
}

// Unsubscribe drops the subscription (tests; production drains on Close).
func (s *SightingIngest) Unsubscribe() {
	if s.sub != nil {
		_ = s.sub.Unsubscribe()
		s.sub = nil
	}
}

func (s *SightingIngest) handle(msg *nats.Msg) {
	var p events.SightingPayload
	if err := json.Unmarshal(msg.Data, &p); err != nil {
		s.logger.Warn("controller.sighting.bad_payload", "subject", msg.Subject, "error", err)
		return
	}
	if err := s.ProjectSighting(p); err != nil {
		s.logger.Warn("controller.sighting.project_failed",
			"subject", msg.Subject, "tag_id", p.TagID, "error", err)
	}
}

// ProjectSighting resolves one raw sighting to its owning unit and, if it
// represents real movement, upserts instance_location + mirrors it down. Pure
// (app + KV) and NATS-free so tests drive it directly with a nil KV. An unknown
// tag (no index entry, or index lag) is dropped — advisory, self-heals on the
// next read.
func (s *SightingIngest) ProjectSighting(p events.SightingPayload) error {
	tag := normalizeTag(p.TagID)
	if tag == "" {
		return nil
	}
	idx, err := s.app.FindFirstRecordByFilter("instance_epc_index",
		"rfid_epc = {:e}", dbx.Params{"e": tag})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("resolve epc %q: %w", tag, err)
	}
	kioskCode := idx.GetString("kiosk_code")
	instanceID := idx.GetString("instance_id")
	instanceCode := idx.GetString("instance_code")

	when := p.ObservedAt
	if when.IsZero() {
		when = time.Now().UTC()
	}

	// Movement-not-reads dedup: same zone/gps as last time → drop before any write.
	key := dedupKey(p)
	s.mu.Lock()
	if prev, ok := s.lastKey[tag]; ok && prev == key {
		s.mu.Unlock()
		return nil
	}
	s.lastKey[tag] = key
	s.mu.Unlock()

	if err := s.upsertLocation(kioskCode, instanceID, instanceCode, p, when); err != nil {
		return err
	}
	s.writeMirror(kioskCode, instanceID, p, when)
	return nil
}

// upsertLocation monotonically writes the site-wide instance_location row for
// one unit. Skips when the stored observation is newer-or-equal (handles
// out-of-order delivery; the dedup map handles the steady-state repeat case).
func (s *SightingIngest) upsertLocation(kioskCode, instanceID, instanceCode string, p events.SightingPayload, when time.Time) error {
	col, err := s.app.FindCollectionByNameOrId("instance_location")
	if err != nil {
		return fmt.Errorf("instance_location collection: %w", err)
	}
	rec, err := s.app.FindFirstRecordByFilter("instance_location",
		"kiosk_code = {:k} && instance_code = {:c}",
		dbx.Params{"k": kioskCode, "c": instanceCode})
	if err != nil {
		if !isNotFound(err) {
			return fmt.Errorf("find instance_location: %w", err)
		}
		rec = core.NewRecord(col)
		rec.Set("kiosk_code", kioskCode)
		rec.Set("instance_code", instanceCode)
	} else if existing := rec.GetDateTime("last_observed_at"); !existing.IsZero() && !when.After(existing.Time()) {
		return nil // stored sighting is newer-or-equal — monotonic no-op
	}
	rec.Set("instance_id", instanceID)
	rec.Set("last_observed_at", when)
	rec.Set("last_observed_zone", p.Zone)
	rec.Set("last_observed_gateway", p.GatewayID)
	lat, lon := 0.0, 0.0
	if p.Lat != nil {
		lat = *p.Lat
	}
	if p.Lon != nil {
		lon = *p.Lon
	}
	rec.Set("last_observed_lat", lat)
	rec.Set("last_observed_lon", lon)
	if err := s.app.Save(rec); err != nil {
		return fmt.Errorf("save instance_location: %w", err)
	}
	return nil
}

// writeMirror broadcasts the unit's last-observed into last_observed_state so
// the owning node's MirrorWatcher folds it into its local columns. Best-effort,
// keyed <kiosk_code>.<instance_id>; a failure logs and the instance_location
// view still stands.
func (s *SightingIngest) writeMirror(kioskCode, instanceID string, p events.SightingPayload, when time.Time) {
	if s.locationKV == nil || instanceID == "" {
		return
	}
	st := sightings.LastObservedState{Zone: p.Zone, Gateway: p.GatewayID, ObservedAt: when}
	if p.Lat != nil {
		st.Lat = *p.Lat
	}
	if p.Lon != nil {
		st.Lon = *p.Lon
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	key := sightings.LastObservedStateKey(kioskCode, instanceID)
	if _, err := s.locationKV.Put(context.Background(), key, data); err != nil {
		slog.Warn("controller.sighting.mirror_put_failed", "key", key, "error", err)
	}
}

func normalizeTag(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}

// dedupKey collapses a sighting to its location signature: zone plus GPS rounded
// to ~11 m, so a tool sitting still (re-read every cycle) maps to the same key
// and is dropped, while a real move changes it.
func dedupKey(p events.SightingPayload) string {
	lat, lon := 0.0, 0.0
	if p.Lat != nil {
		lat = math.Round(*p.Lat*10000) / 10000
	}
	if p.Lon != nil {
		lon = math.Round(*p.Lon*10000) / 10000
	}
	return fmt.Sprintf("%s|%.4f|%.4f", p.Zone, lat, lon)
}
