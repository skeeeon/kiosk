package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go/jetstream"
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
	items jetstream.KeyValue
	users jetstream.KeyValue
	app   core.App

	// pendingMembershipDelete: kiosk_items.id → "<kiosk_code>.<item_code>"
	// captured in OnRecordDelete so AfterDeleteSuccess has the key to delete
	// even after cascade has wiped the referenced rows.
	pendingMembershipDelete sync.Map
}

// NewCatalogPublisher provisions the two KV buckets (idempotent) and binds
// the PB record hooks on the supplied app.
func NewCatalogPublisher(ctx context.Context, app core.App, js jetstream.JetStream, itemsBucket, usersBucket string) (*CatalogPublisher, error) {
	if itemsBucket == "" {
		itemsBucket = catalog.ItemsBucket
	}
	if usersBucket == "" {
		usersBucket = catalog.UsersBucket
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

	cp := &CatalogPublisher{items: items, users: users, app: app}
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
	payload := catalog.UserPayload{
		Code:   rec.GetString("code"),
		Name:   rec.GetString("name"),
		Email:  rec.GetString("email"),
		Role:   rec.GetString("role"),
		Active: rec.GetBool("active"),
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
