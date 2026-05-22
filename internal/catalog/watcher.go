package catalog

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// Watcher subscribes to the catalog KV buckets and projects each update
// into the kiosk's local PB items/users collections. Lifecycle:
//
//	w := catalog.NewWatcher(app, js, "catalog_items", "catalog_users")
//	w.Start(ctx)        // returns once watchers are running
//	...
//	w.Stop()            // call from OnTerminate
type Watcher struct {
	app          core.App
	js           jetstream.JetStream
	itemsBucket  string
	usersBucket  string
	itemsWatcher jetstream.KeyWatcher
	usersWatcher jetstream.KeyWatcher
	cancel       context.CancelFunc
}

// NewWatcher wires a watcher but doesn't connect — call Start to begin
// watching. Bucket names default to catalog.ItemsBucket / UsersBucket if
// empty, so operators with the standard layout can leave the yaml blank.
func NewWatcher(app core.App, js jetstream.JetStream, itemsBucket, usersBucket string) *Watcher {
	if itemsBucket == "" {
		itemsBucket = ItemsBucket
	}
	if usersBucket == "" {
		usersBucket = UsersBucket
	}
	return &Watcher{
		app:         app,
		js:          js,
		itemsBucket: itemsBucket,
		usersBucket: usersBucket,
	}
}

// Start opens KV watchers on both catalog buckets and spawns goroutines
// that project incoming events into PB. Errors from this are startup-level
// (bucket doesn't exist, NATS down). Once Start returns nil, projection
// runs until Stop is called.
func (w *Watcher) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel

	items, err := w.js.KeyValue(ctx, w.itemsBucket)
	if err != nil {
		cancel()
		return fmt.Errorf("open items KV %q: %w", w.itemsBucket, err)
	}
	users, err := w.js.KeyValue(ctx, w.usersBucket)
	if err != nil {
		cancel()
		return fmt.Errorf("open users KV %q: %w", w.usersBucket, err)
	}

	// WatchAll subscribes from the start of the bucket: kiosk receives the
	// current snapshot, then ongoing deltas. IncludeHistory is false by
	// default — we want latest values only, not the full history.
	iw, err := items.WatchAll(ctx)
	if err != nil {
		cancel()
		return fmt.Errorf("watch items: %w", err)
	}
	uw, err := users.WatchAll(ctx)
	if err != nil {
		iw.Stop()
		cancel()
		return fmt.Errorf("watch users: %w", err)
	}
	w.itemsWatcher = iw
	w.usersWatcher = uw

	go w.runItems(ctx, iw)
	go w.runUsers(ctx, uw)

	slog.Info("kiosk.catalog.watcher.started",
		"items_bucket", w.itemsBucket, "users_bucket", w.usersBucket)
	return nil
}

// Stop tears down the watchers. Safe to call multiple times.
func (w *Watcher) Stop() {
	if w.itemsWatcher != nil {
		w.itemsWatcher.Stop()
		w.itemsWatcher = nil
	}
	if w.usersWatcher != nil {
		w.usersWatcher.Stop()
		w.usersWatcher = nil
	}
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
}

func (w *Watcher) runItems(ctx context.Context, kw jetstream.KeyWatcher) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-kw.Updates():
			if !ok {
				return
			}
			if entry == nil {
				// Marker that we've caught up to the latest value in the
				// bucket. Operators sometimes want to know this for liveness.
				slog.Info("kiosk.catalog.items.snapshot_done")
				continue
			}
			w.applyItem(entry)
		}
	}
}

func (w *Watcher) runUsers(ctx context.Context, kw jetstream.KeyWatcher) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-kw.Updates():
			if !ok {
				return
			}
			if entry == nil {
				slog.Info("kiosk.catalog.users.snapshot_done")
				continue
			}
			w.applyUser(entry)
		}
	}
}

func (w *Watcher) applyItem(entry jetstream.KeyValueEntry) {
	code := entry.Key()
	switch entry.Operation() {
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		if err := w.softDelete("items", code); err != nil {
			slog.Warn("kiosk.catalog.items.delete_failed", "code", code, "error", err)
		}
		return
	case jetstream.KeyValuePut:
		payload, err := UnmarshalItem(entry.Value())
		if err != nil {
			slog.Warn("kiosk.catalog.items.bad_payload", "code", code, "error", err)
			return
		}
		if err := w.upsertItem(payload); err != nil {
			slog.Warn("kiosk.catalog.items.upsert_failed", "code", code, "error", err)
		}
	}
}

func (w *Watcher) applyUser(entry jetstream.KeyValueEntry) {
	code := entry.Key()
	switch entry.Operation() {
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		if err := w.softDelete("users", code); err != nil {
			slog.Warn("kiosk.catalog.users.delete_failed", "code", code, "error", err)
		}
		return
	case jetstream.KeyValuePut:
		payload, err := UnmarshalUser(entry.Value())
		if err != nil {
			slog.Warn("kiosk.catalog.users.bad_payload", "code", code, "error", err)
			return
		}
		if err := w.upsertUser(payload); err != nil {
			slog.Warn("kiosk.catalog.users.upsert_failed", "code", code, "error", err)
		}
	}
}

func (w *Watcher) upsertItem(p ItemPayload) error {
	existing, err := w.app.FindFirstRecordByFilter("items",
		"code = {:c}", dbx.Params{"c": p.Code})
	if err != nil && !isNotFound(err) {
		return err
	}

	var rec *core.Record
	if existing != nil {
		rec = existing
	} else {
		col, err := w.app.FindCollectionByNameOrId("items")
		if err != nil {
			return fmt.Errorf("find items collection: %w", err)
		}
		rec = core.NewRecord(col)
	}

	rec.Set("code", p.Code)
	rec.Set("rfid_epc", p.RFIDEPC)
	rec.Set("name", p.Name)
	rec.Set("type", p.Type)
	rec.Set("unit", p.Unit)
	rec.Set("tracking_mode", p.TrackingMode)
	rec.Set("serial", p.Serial)
	rec.Set("category", p.Category)
	rec.Set("active", p.Active)
	rec.Set("notes", p.Notes)
	// quantity_on_hand and reorder_threshold are intentionally NOT touched —
	// they're kiosk-local state owned by the commit hook + admin stock-adjust
	// flow. New records pick up PB's zero default.
	return w.app.Save(rec)
}

func (w *Watcher) upsertUser(p UserPayload) error {
	existing, err := w.app.FindFirstRecordByFilter("users",
		"code = {:c}", dbx.Params{"c": p.Code})
	if err != nil && !isNotFound(err) {
		return err
	}

	var rec *core.Record
	if existing != nil {
		rec = existing
	} else {
		col, err := w.app.FindCollectionByNameOrId("users")
		if err != nil {
			return fmt.Errorf("find users collection: %w", err)
		}
		rec = core.NewRecord(col)
		// PB auth-collection requires a password on insert. Workers don't
		// log in, so a random one nobody knows is fine — central never
		// transmits worker credentials.
		pw, err := randomPassword(16)
		if err != nil {
			return fmt.Errorf("generate password: %w", err)
		}
		rec.SetPassword(pw)
	}

	rec.Set("code", p.Code)
	rec.Set("name", p.Name)
	rec.Set("email", p.Email)
	rec.Set("role", p.Role)
	rec.Set("active", p.Active)
	return w.app.Save(rec)
}

// softDelete sets active=false on the record matching the code. We don't
// hard-delete because transaction_lines and open_checkouts reference the
// record by FK and we'd rather keep history intact than orphan the rows.
// If no record matches, it's a no-op — the controller may have purged a
// code we never saw.
func (w *Watcher) softDelete(collection, code string) error {
	rec, err := w.app.FindFirstRecordByFilter(collection,
		"code = {:c}", dbx.Params{"c": code})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	rec.Set("active", false)
	return w.app.Save(rec)
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, sql.ErrNoRows)
}

func randomPassword(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
