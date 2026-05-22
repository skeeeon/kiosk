package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/catalog"
)

// CatalogPublisher fans controller-side changes out to the JetStream KV bucket
// that managed kiosks watch.
//
// Items are NOT broadcast: each kiosk's "stock" is governed by membership rows
// in the `kiosk_items` collection. Keys in the shared `catalog_items` bucket
// are namespaced `<kiosk_code>.<item_code>` so each kiosk can subscribe with
// `Watch("<my_code>.>")` and only receive its own slice.
//
// Hook wiring:
//
//	items create/update     → publish to every member kiosk (loop kiosk_items).
//	items delete            → no direct hook; PB cascade-deletes the membership
//	                          rows, and the kiosk_items delete hooks emit the
//	                          per-kiosk KV deletes.
//	kiosk_items create/update → publish that one (kiosk, item) pair.
//	kiosk_items delete      → emit KV delete for that pair. Key is captured in
//	                          OnRecordDelete because by AfterDeleteSuccess the
//	                          cascaded kiosks/items records may already be gone.
//
// CatalogPublisher owns no goroutines — PB invokes hooks synchronously on the
// request path; the NATS conn buffers in-memory if the broker is unreachable.
type CatalogPublisher struct {
	items  jetstream.KeyValue
	users  jetstream.KeyValue
	groups jetstream.KeyValue
	app    core.App

	// pendingMembershipDelete: kiosk_items.id → "<kiosk_code>.<item_code>"
	// captured in OnRecordDelete so AfterDeleteSuccess has the key to delete
	// even after cascade has wiped the referenced rows.
	pendingMembershipDelete sync.Map

	// pendingGroupDelete: groups.id → group code, captured before delete so
	// AfterDeleteSuccess can emit the KV delete by code.
	pendingGroupDelete sync.Map
}

// NewCatalogPublisher provisions the three KV buckets (idempotent) and binds
// the PB record hooks on the supplied app.
func NewCatalogPublisher(ctx context.Context, app core.App, js jetstream.JetStream, itemsBucket, usersBucket, groupsBucket string) (*CatalogPublisher, error) {
	if itemsBucket == "" {
		itemsBucket = catalog.ItemsBucket
	}
	if usersBucket == "" {
		usersBucket = catalog.UsersBucket
	}
	if groupsBucket == "" {
		groupsBucket = catalog.GroupsBucket
	}

	items, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      itemsBucket,
		Description: "Catalog: items, keyed by <kiosk_code>.<item_code>. Membership-driven.",
		History:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("create items KV %q: %w", itemsBucket, err)
	}
	users, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      usersBucket,
		Description: "Catalog: users, keyed by code. Source of truth for managed kiosks.",
		History:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("create users KV %q: %w", usersBucket, err)
	}
	groups, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      groupsBucket,
		Description: "Catalog: groups, keyed by code. Source of truth for managed kiosks.",
		History:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("create groups KV %q: %w", groupsBucket, err)
	}

	cp := &CatalogPublisher{items: items, users: users, groups: groups, app: app}
	cp.bindHooks(app)
	return cp, nil
}

func (p *CatalogPublisher) bindHooks(app core.App) {
	// Items: publish each saved item to every kiosk that stocks it.
	// A freshly-created item has no memberships yet so this is a no-op until
	// an admin assigns it via kiosk_items.
	app.OnRecordAfterCreateSuccess("items").BindFunc(func(e *core.RecordEvent) error {
		p.publishItemToMembers(e.Record)
		return e.Next()
	})
	app.OnRecordAfterUpdateSuccess("items").BindFunc(func(e *core.RecordEvent) error {
		p.publishItemToMembers(e.Record)
		return e.Next()
	})
	// No items-delete hook: CascadeDelete on kiosk_items.item means the
	// membership rows die first and their delete hooks emit the per-kiosk
	// KV deletes.

	// kiosk_items: each row mirrors exactly one KV entry.
	app.OnRecordAfterCreateSuccess("kiosk_items").BindFunc(func(e *core.RecordEvent) error {
		p.publishMembership(e.Record)
		return e.Next()
	})
	app.OnRecordAfterUpdateSuccess("kiosk_items").BindFunc(func(e *core.RecordEvent) error {
		// Updates to a kiosk_items row are unusual (the pair is the identity),
		// but if it happens we re-publish to the current pair.
		p.publishMembership(e.Record)
		return e.Next()
	})
	app.OnRecordDelete("kiosk_items").BindFunc(func(e *core.RecordEvent) error {
		// Capture the KV key before cascade can void the referenced records.
		if key := p.kvKeyForMembership(e.Record); key != "" {
			p.pendingMembershipDelete.Store(e.Record.Id, key)
		}
		return e.Next()
	})
	app.OnRecordAfterDeleteSuccess("kiosk_items").BindFunc(func(e *core.RecordEvent) error {
		v, ok := p.pendingMembershipDelete.LoadAndDelete(e.Record.Id)
		if !ok {
			return e.Next()
		}
		key := v.(string)
		if err := p.items.Delete(context.Background(), key); err != nil {
			slog.Warn("controller.catalog.delete_item_failed",
				"key", key, "error", err)
		} else {
			slog.Info("controller.catalog.delete_item", "key", key)
		}
		return e.Next()
	})

	// Users hooks. Same shape as before; password/auth fields never leave
	// the controller (see catalog.UserPayload).
	app.OnRecordAfterCreateSuccess("users").BindFunc(func(e *core.RecordEvent) error {
		p.publishUser(e.Record)
		return e.Next()
	})
	app.OnRecordAfterUpdateSuccess("users").BindFunc(func(e *core.RecordEvent) error {
		p.publishUser(e.Record)
		return e.Next()
	})
	app.OnRecordAfterDeleteSuccess("users").BindFunc(func(e *core.RecordEvent) error {
		p.deleteUser(e.Record.GetString("code"))
		return e.Next()
	})

	// Groups hooks. Org-wide bucket; one KV entry per group keyed by code.
	app.OnRecordAfterCreateSuccess("groups").BindFunc(func(e *core.RecordEvent) error {
		p.publishGroup(e.Record)
		return e.Next()
	})
	app.OnRecordAfterUpdateSuccess("groups").BindFunc(func(e *core.RecordEvent) error {
		p.publishGroup(e.Record)
		return e.Next()
	})
	// Capture the code before delete: by AfterDeleteSuccess the row is gone
	// and rec.GetString("code") returns empty.
	app.OnRecordDelete("groups").BindFunc(func(e *core.RecordEvent) error {
		if code := e.Record.GetString("code"); code != "" {
			p.pendingGroupDelete.Store(e.Record.Id, code)
		}
		return e.Next()
	})
	app.OnRecordAfterDeleteSuccess("groups").BindFunc(func(e *core.RecordEvent) error {
		v, ok := p.pendingGroupDelete.LoadAndDelete(e.Record.Id)
		if !ok {
			return e.Next()
		}
		p.deleteGroup(v.(string))
		return e.Next()
	})
}

// publishItemToMembers serializes the item once and writes it to every
// member kiosk's slot in the KV bucket. Failures are logged per-kiosk so a
// transient hiccup on one publish doesn't drop the others.
func (p *CatalogPublisher) publishItemToMembers(rec *core.Record) {
	payload := itemPayloadFrom(rec)
	data, err := catalog.MarshalItem(payload)
	if err != nil {
		slog.Warn("controller.catalog.marshal_item_failed",
			"code", payload.Code, "error", err)
		return
	}
	kiosks, err := KiosksForItem(p.app, rec.Id)
	if err != nil {
		slog.Warn("controller.catalog.membership_lookup_failed",
			"item", payload.Code, "error", err)
		return
	}
	for _, k := range kiosks {
		key := k.GetString("kiosk_code") + "." + payload.Code
		if _, err := p.items.Put(context.Background(), key, data); err != nil {
			slog.Warn("controller.catalog.put_item_failed",
				"key", key, "error", err)
			continue
		}
		slog.Info("controller.catalog.put_item", "key", key)
	}
}

// publishMembership writes one KV entry for a (kiosk, item) pair. Resolves
// both FKs to read their codes plus the item payload.
func (p *CatalogPublisher) publishMembership(rec *core.Record) {
	kioskID := rec.GetString("kiosk")
	itemID := rec.GetString("item")
	if kioskID == "" || itemID == "" {
		return
	}
	kiosk, err := p.app.FindRecordById("kiosks", kioskID)
	if err != nil {
		slog.Warn("controller.catalog.membership_resolve_kiosk_failed",
			"kiosk_id", kioskID, "error", err)
		return
	}
	item, err := p.app.FindRecordById("items", itemID)
	if err != nil {
		slog.Warn("controller.catalog.membership_resolve_item_failed",
			"item_id", itemID, "error", err)
		return
	}
	payload := itemPayloadFrom(item)
	data, err := catalog.MarshalItem(payload)
	if err != nil {
		slog.Warn("controller.catalog.marshal_item_failed",
			"code", payload.Code, "error", err)
		return
	}
	key := kiosk.GetString("kiosk_code") + "." + payload.Code
	if _, err := p.items.Put(context.Background(), key, data); err != nil {
		slog.Warn("controller.catalog.put_item_failed",
			"key", key, "error", err)
		return
	}
	slog.Info("controller.catalog.put_item", "key", key)
}

// kvKeyForMembership resolves a kiosk_items record into its KV key
// ("<kiosk_code>.<item_code>") by dereferencing both FKs. Returns "" if
// either record can't be found.
func (p *CatalogPublisher) kvKeyForMembership(rec *core.Record) string {
	kioskID := rec.GetString("kiosk")
	itemID := rec.GetString("item")
	if kioskID == "" || itemID == "" {
		return ""
	}
	kiosk, err := p.app.FindRecordById("kiosks", kioskID)
	if err != nil {
		return ""
	}
	item, err := p.app.FindRecordById("items", itemID)
	if err != nil {
		return ""
	}
	return kiosk.GetString("kiosk_code") + "." + item.GetString("code")
}

func itemPayloadFrom(rec *core.Record) catalog.ItemPayload {
	return catalog.ItemPayload{
		Code:         rec.GetString("code"),
		Name:         rec.GetString("name"),
		Type:         rec.GetString("type"),
		Unit:         rec.GetString("unit"),
		TrackingMode: rec.GetString("tracking_mode"),
		Category:     rec.GetString("category"),
		Active:       rec.GetBool("active"),
		Notes:        rec.GetString("notes"),
	}
}

func (p *CatalogPublisher) publishUser(rec *core.Record) {
	// users.group is an FK to groups.id; the wire carries the human-readable
	// code so the receiving kiosk can resolve to its own local id. Blank when
	// the user is ungrouped or when the group record can't be found.
	groupCode := ""
	if gID := rec.GetString("group"); gID != "" {
		if g, err := p.app.FindRecordById("groups", gID); err == nil {
			groupCode = g.GetString("code")
		}
	}
	payload := catalog.UserPayload{
		Code:      rec.GetString("code"),
		Name:      rec.GetString("name"),
		Email:     rec.GetString("email"),
		Role:      rec.GetString("role"),
		GroupCode: groupCode,
		Active:    rec.GetBool("active"),
	}
	data, err := catalog.MarshalUser(payload)
	if err != nil {
		slog.Warn("controller.catalog.marshal_user_failed",
			"code", payload.Code, "error", err)
		return
	}
	if _, err := p.users.Put(context.Background(), payload.Code, data); err != nil {
		slog.Warn("controller.catalog.put_user_failed",
			"code", payload.Code, "error", err)
		return
	}
	slog.Info("controller.catalog.put_user", "code", payload.Code)
}

func (p *CatalogPublisher) deleteUser(code string) {
	if code == "" {
		return
	}
	if err := p.users.Delete(context.Background(), code); err != nil {
		slog.Warn("controller.catalog.delete_user_failed",
			"code", code, "error", err)
		return
	}
	slog.Info("controller.catalog.delete_user", "code", code)
}

func (p *CatalogPublisher) publishGroup(rec *core.Record) {
	payload := catalog.GroupPayload{
		Code:         rec.GetString("code"),
		Name:         rec.GetString("name"),
		ContactEmail: rec.GetString("contact_email"),
		ContactPhone: rec.GetString("contact_phone"),
		Notes:        rec.GetString("notes"),
		Active:       rec.GetBool("active"),
	}
	data, err := catalog.MarshalGroup(payload)
	if err != nil {
		slog.Warn("controller.catalog.marshal_group_failed",
			"code", payload.Code, "error", err)
		return
	}
	if _, err := p.groups.Put(context.Background(), payload.Code, data); err != nil {
		slog.Warn("controller.catalog.put_group_failed",
			"code", payload.Code, "error", err)
		return
	}
	slog.Info("controller.catalog.put_group", "code", payload.Code)

	// When a group changes, every user that references it needs a fresh
	// payload so receivers re-resolve to (potentially new) local FK ids and
	// pick up changes like contact_email going to receipts. This is a
	// fan-out write keyed off the group; for the GC-renting-to-subs scale
	// (tens of subs, hundreds of users) it's negligible.
	users, err := p.app.FindRecordsByFilter("users", "group = {:g}", "", 0, 0, dbx.Params{"g": rec.Id})
	if err != nil {
		slog.Warn("controller.catalog.group_user_lookup_failed",
			"group", payload.Code, "error", err)
		return
	}
	for _, u := range users {
		p.publishUser(u)
	}
}

func (p *CatalogPublisher) deleteGroup(code string) {
	if code == "" {
		return
	}
	if err := p.groups.Delete(context.Background(), code); err != nil {
		slog.Warn("controller.catalog.delete_group_failed",
			"code", code, "error", err)
		return
	}
	slog.Info("controller.catalog.delete_group", "code", code)
}
