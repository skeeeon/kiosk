package timeclock

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"
)

// Watcher hydrates a Fleet from the controller-written punch_state bucket.
// Small sibling of catalog.Watcher — deliberately its own type rather than
// an extension of the catalog watcher, since punch state projects into an
// in-memory map, not PB records. WatchAll replays the full bucket on start
// (the nil caught-up marker separates snapshot from live deltas), so a
// process restart recovers fleet state without local persistence.
type Watcher struct {
	js     jetstream.JetStream
	fleet  *Fleet
	cancel context.CancelFunc
	kw     jetstream.KeyWatcher
}

// NewWatcher wires a watcher onto an existing Fleet. Call Start to attach.
func NewWatcher(js jetstream.JetStream, fleet *Fleet) *Watcher {
	return &Watcher{js: js, fleet: fleet}
}

// Start opens the bucket and begins projecting entries into the Fleet.
// Errors are startup-level (bucket missing — the controller provisions it,
// so a kiosk booting before the controller's first run will fail here;
// callers log and continue, degraded to local-only state).
func (w *Watcher) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel

	kv, err := w.js.KeyValue(ctx, PunchStateBucket)
	if err != nil {
		cancel()
		return fmt.Errorf("open punch_state KV %q: %w", PunchStateBucket, err)
	}
	kw, err := kv.WatchAll(ctx)
	if err != nil {
		cancel()
		return fmt.Errorf("watch punch_state: %w", err)
	}
	w.kw = kw

	go w.run(ctx, kw)
	slog.Info("kiosk.timeclock.watcher.started", "bucket", PunchStateBucket)
	return nil
}

// Stop tears down the watcher. Safe to call multiple times.
func (w *Watcher) Stop() {
	if w.kw != nil {
		w.kw.Stop()
		w.kw = nil
	}
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
}

func (w *Watcher) run(ctx context.Context, kw jetstream.KeyWatcher) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-kw.Updates():
			if !ok {
				return
			}
			if entry == nil {
				slog.Info("kiosk.timeclock.punch_state.snapshot_done")
				continue
			}
			w.apply(entry)
		}
	}
}

func (w *Watcher) apply(entry jetstream.KeyValueEntry) {
	switch entry.Operation() {
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		w.fleet.Delete(entry.Key())
		return
	}
	var p PunchStatePayload
	if err := json.Unmarshal(entry.Value(), &p); err != nil {
		slog.Warn("kiosk.timeclock.punch_state.bad_payload",
			"key", entry.Key(), "error", err)
		return
	}
	if p.UserCode == "" {
		p.UserCode = entry.Key()
	}
	// Fleet.Upsert is monotonic on occurred_at, so out-of-order watcher
	// deliveries can't move a user's state backwards — and the kiosk's own
	// punches (already in its local ledger) win ties via the merge rule.
	w.fleet.Upsert(p)
}
