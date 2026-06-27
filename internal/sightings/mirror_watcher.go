package sightings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/pocketbase/pocketbase/core"
)

// MirrorWatcher hydrates a managed node's own slice of the controller's
// last_observed_state KV bucket into its local item_instances.last_observed_*
// columns (location/sightings L3). It watches "<kiosk_code>.>" — the
// catalog_items slicing pattern, NOT WatchAll — so the node only ever receives
// keys for its OWN units, never another site's. This is what lets a unit owned
// here but seen by another site's gateway show its true last-seen locally.
//
// Advisory + best-effort: a KV outage degrades to local-gateway data only and
// self-heals on the next sighting. WatchAll-of-its-own-prefix on start replays
// the slice so a restart recovers.
type MirrorWatcher struct {
	app       core.App
	js        jetstream.JetStream
	bucket    string
	kioskCode string
	kw        jetstream.KeyWatcher
	cancel    context.CancelFunc
}

// NewMirrorWatcher wires a watcher; call Start to begin. Empty bucket defaults
// to LastObservedStateBucket.
func NewMirrorWatcher(app core.App, js jetstream.JetStream, kioskCode, bucket string) *MirrorWatcher {
	if bucket == "" {
		bucket = LastObservedStateBucket
	}
	return &MirrorWatcher{app: app, js: js, bucket: bucket, kioskCode: kioskCode}
}

// Start opens the KV bucket, begins watching this node's prefix, and projects
// updates into local item_instances. Errors here are startup-level (bucket
// missing, NATS down).
func (w *MirrorWatcher) Start(parent context.Context) error {
	if w.kioskCode == "" {
		return fmt.Errorf("kiosk code is empty; refusing to watch the whole last_observed_state bucket")
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel

	kv, err := w.js.KeyValue(ctx, w.bucket)
	if err != nil {
		cancel()
		return fmt.Errorf("open %s KV: %w", w.bucket, err)
	}
	// Own slice only — server-side prefix filter (the catalog_items pattern).
	kw, err := kv.Watch(ctx, w.kioskCode+".>")
	if err != nil {
		cancel()
		return fmt.Errorf("watch %s: %w", w.bucket, err)
	}
	w.kw = kw
	go w.run(ctx, kw)
	slog.Info("kiosk.sighting.mirror_watcher.started", "bucket", w.bucket, "prefix", w.kioskCode+".>")
	return nil
}

// Stop tears the watcher down. Safe to call multiple times.
func (w *MirrorWatcher) Stop() {
	if w.kw != nil {
		w.kw.Stop()
		w.kw = nil
	}
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
}

func (w *MirrorWatcher) run(ctx context.Context, kw jetstream.KeyWatcher) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-kw.Updates():
			if !ok {
				return
			}
			if entry == nil {
				slog.Info("kiosk.sighting.mirror_watcher.snapshot_done")
				continue
			}
			w.apply(entry)
		}
	}
}

// apply projects one KV entry onto the local instance. Deletes are ignored —
// last-observed is never cleared, only advanced.
func (w *MirrorWatcher) apply(entry jetstream.KeyValueEntry) {
	if entry.Operation() != jetstream.KeyValuePut {
		return
	}
	if err := applyLastObservedKV(w.app, w.kioskCode, entry.Key(), entry.Value()); err != nil {
		slog.Warn("kiosk.sighting.mirror_watcher.apply_failed", "key", entry.Key(), "error", err)
	}
}

// applyLastObservedKV is the pure projection of one KV entry → a local stamp,
// split out so it's unit-testable without faking a JetStream entry. The key
// suffix is the instance id (the node's own item_instances.id); the stamp is
// monotonic, so a mirror value older than what a local custody read already
// wrote is a no-op. GPS 0,0 is treated as "no coordinates".
func applyLastObservedKV(app core.App, kioskCode, key string, value []byte) error {
	instanceID := stripPrefix(key, kioskCode+".")
	if instanceID == "" {
		return fmt.Errorf("unexpected key %q", key)
	}
	var st LastObservedState
	if err := json.Unmarshal(value, &st); err != nil {
		return fmt.Errorf("bad payload: %w", err)
	}
	var lat, lon *float64
	if st.Lat != 0 || st.Lon != 0 {
		lat, lon = &st.Lat, &st.Lon
	}
	return StampLastObserved(app, instanceID, st.Zone, st.Gateway, lat, lon, st.ObservedAt)
}

// stripPrefix returns the part of key after prefix, or "" if it doesn't match.
func stripPrefix(key, prefix string) string {
	if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
		return ""
	}
	return key[len(prefix):]
}
