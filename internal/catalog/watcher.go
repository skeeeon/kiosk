package catalog

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/dberr"
)

// Watcher subscribes to the catalog KV buckets and projects each update
// into the kiosk's local PB items/users/groups collections. Lifecycle:
//
//	w := catalog.NewWatcher(app, js, "catalog_items", "catalog_users", "catalog_groups", "KIOSK01")
//	w.Start(ctx)        // returns once watchers are running
//	...
//	w.Stop()            // call from OnTerminate
//
// `kioskCode` scopes the items watch to this kiosk's slice of the shared
// catalog_items bucket — keys are namespaced `<kiosk_code>.<item_code>` so
// the watcher uses `Watch("<my_code>.>")` to filter on the wire instead of
// receiving everything and discarding. Users and groups are not scoped
// (workers and the groups they belong to are org-wide).
type Watcher struct {
	app           core.App
	js            jetstream.JetStream
	itemsBucket   string
	usersBucket   string
	groupsBucket  string
	kioskCode     string
	itemsWatcher  jetstream.KeyWatcher
	usersWatcher  jetstream.KeyWatcher
	groupsWatcher jetstream.KeyWatcher
	cancel        context.CancelFunc
}

// NewWatcher wires a watcher but doesn't connect — call Start to begin
// watching. Bucket names default to catalog.ItemsBucket / UsersBucket /
// GroupsBucket if empty, so operators with the standard layout can leave
// the yaml blank.
func NewWatcher(app core.App, js jetstream.JetStream, itemsBucket, usersBucket, groupsBucket, kioskCode string) *Watcher {
	if itemsBucket == "" {
		itemsBucket = ItemsBucket
	}
	if usersBucket == "" {
		usersBucket = UsersBucket
	}
	if groupsBucket == "" {
		groupsBucket = GroupsBucket
	}
	return &Watcher{
		app:          app,
		js:           js,
		itemsBucket:  itemsBucket,
		usersBucket:  usersBucket,
		groupsBucket: groupsBucket,
		kioskCode:    kioskCode,
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
	groups, err := w.js.KeyValue(ctx, w.groupsBucket)
	if err != nil {
		cancel()
		return fmt.Errorf("open groups KV %q: %w", w.groupsBucket, err)
	}

	// Watch on the kiosk's prefix: snapshot of this kiosk's slice, then
	// ongoing deltas. IncludeHistory is off — we want latest values only.
	// The prefix-filter is enforced server-side by JetStream's consumer
	// filter; the kiosk never sees other kiosks' keys on the wire.
	if w.kioskCode == "" {
		cancel()
		return fmt.Errorf("kiosk code is empty; refusing to watch the whole catalog")
	}
	iw, err := items.Watch(ctx, w.kioskCode+".>")
	if err != nil {
		cancel()
		return fmt.Errorf("watch items: %w", err)
	}
	// Groups before users: a user payload may carry a GroupCode whose row
	// hasn't projected yet. Starting groups first reduces the rate of
	// out-of-order arrivals; both runtimes still tolerate it (upsertUser
	// will retry FK resolution on the next user-update).
	gw, err := groups.WatchAll(ctx)
	if err != nil {
		iw.Stop()
		cancel()
		return fmt.Errorf("watch groups: %w", err)
	}
	uw, err := users.WatchAll(ctx)
	if err != nil {
		iw.Stop()
		gw.Stop()
		cancel()
		return fmt.Errorf("watch users: %w", err)
	}
	w.itemsWatcher = iw
	w.usersWatcher = uw
	w.groupsWatcher = gw

	go w.runItems(ctx, iw)
	go w.runGroups(ctx, gw)
	go w.runUsers(ctx, uw)

	slog.Info("kiosk.catalog.watcher.started",
		"items_bucket", w.itemsBucket,
		"users_bucket", w.usersBucket,
		"groups_bucket", w.groupsBucket)
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
	if w.groupsWatcher != nil {
		w.groupsWatcher.Stop()
		w.groupsWatcher = nil
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

func (w *Watcher) runGroups(ctx context.Context, kw jetstream.KeyWatcher) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-kw.Updates():
			if !ok {
				return
			}
			if entry == nil {
				slog.Info("kiosk.catalog.groups.snapshot_done")
				continue
			}
			w.applyGroup(entry)
		}
	}
}

func (w *Watcher) applyItem(entry jetstream.KeyValueEntry) {
	// Strip the "<kiosk_code>." prefix before treating the key as an item
	// code. With the prefix filter applied at the server, every key here is
	// guaranteed to belong to this kiosk; but we still defensively skip
	// anything that doesn't match the expected shape.
	code := stripPrefix(entry.Key(), w.kioskCode+".")
	if code == "" {
		slog.Warn("kiosk.catalog.items.unexpected_key", "key", entry.Key())
		return
	}
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

// stripPrefix returns the part of key after the leading prefix, or "" if the
// key doesn't start with prefix or is the prefix alone.
func stripPrefix(key, prefix string) string {
	if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
		return ""
	}
	return key[len(prefix):]
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

func (w *Watcher) applyGroup(entry jetstream.KeyValueEntry) {
	code := entry.Key()
	switch entry.Operation() {
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		if err := w.softDelete("groups", code); err != nil {
			slog.Warn("kiosk.catalog.groups.delete_failed", "code", code, "error", err)
		}
		return
	case jetstream.KeyValuePut:
		payload, err := UnmarshalGroup(entry.Value())
		if err != nil {
			slog.Warn("kiosk.catalog.groups.bad_payload", "code", code, "error", err)
			return
		}
		if err := w.upsertGroup(payload); err != nil {
			slog.Warn("kiosk.catalog.groups.upsert_failed", "code", code, "error", err)
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
	rec.Set("name", p.Name)
	rec.Set("type", p.Type)
	rec.Set("unit", p.Unit)
	rec.Set("tracking_mode", p.TrackingMode)
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
	rec.Set("phone", p.Phone)
	rec.Set("role", p.Role)
	// Resolve group code → local FK. If the group payload hasn't projected
	// yet (out-of-order arrival), set blank — the next user update after the
	// group lands will fill it in. Group is optional anyway, so blank is a
	// valid state, not an error.
	groupID := ""
	if p.GroupCode != "" {
		if g, err := w.app.FindFirstRecordByFilter("groups", "code = {:c}", dbx.Params{"c": p.GroupCode}); err == nil {
			groupID = g.Id
		} else if !isNotFound(err) {
			return fmt.Errorf("resolve group %q: %w", p.GroupCode, err)
		}
	}
	rec.Set("group", groupID)
	rec.Set("active", p.Active)
	return w.app.Save(rec)
}

func (w *Watcher) upsertGroup(p GroupPayload) error {
	existing, err := w.app.FindFirstRecordByFilter("groups",
		"code = {:c}", dbx.Params{"c": p.Code})
	if err != nil && !isNotFound(err) {
		return err
	}

	var rec *core.Record
	if existing != nil {
		rec = existing
	} else {
		col, err := w.app.FindCollectionByNameOrId("groups")
		if err != nil {
			return fmt.Errorf("find groups collection: %w", err)
		}
		rec = core.NewRecord(col)
	}

	rec.Set("code", p.Code)
	rec.Set("name", p.Name)
	rec.Set("contact_email", p.ContactEmail)
	rec.Set("contact_phone", p.ContactPhone)
	rec.Set("notes", p.Notes)
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
	return dberr.IsNotFound(err)
}

func randomPassword(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
