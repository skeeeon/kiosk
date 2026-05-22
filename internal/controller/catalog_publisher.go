package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/catalog"
)

// CatalogPublisher fans changes to the controller's `items` and `users`
// collections out to JetStream KV buckets that managed kiosks watch. It
// owns no goroutines — PB invokes its hooks synchronously on the request
// path, and the underlying NATS conn buffers in-memory if the broker is
// temporarily unreachable.
type CatalogPublisher struct {
	items jetstream.KeyValue
	users jetstream.KeyValue
}

// NewCatalogPublisher provisions the two KV buckets (idempotent) and binds
// the PB record hooks on the supplied app. Bucket names fall back to the
// package defaults (catalog.ItemsBucket / catalog.UsersBucket) when empty,
// so config can leave them blank for the standard layout. Both sides of the
// sync — this publisher and the kiosk's watcher — read from the same
// controller.catalog_*_bucket config keys, so as long as the names match
// in both yamls, things work.
func NewCatalogPublisher(ctx context.Context, app core.App, js jetstream.JetStream, itemsBucket, usersBucket string) (*CatalogPublisher, error) {
	if itemsBucket == "" {
		itemsBucket = catalog.ItemsBucket
	}
	if usersBucket == "" {
		usersBucket = catalog.UsersBucket
	}

	items, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      itemsBucket,
		Description: "Catalog: items, keyed by code. Source of truth for managed kiosks.",
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

	cp := &CatalogPublisher{items: items, users: users}
	cp.bindHooks(app)
	return cp, nil
}

func (p *CatalogPublisher) bindHooks(app core.App) {
	// Items hooks. After-success variants fire only when the DB transaction
	// committed, so we never publish ghost updates that got rolled back.
	app.OnRecordAfterCreateSuccess("items").BindFunc(func(e *core.RecordEvent) error {
		p.publishItem(e.Record)
		return e.Next()
	})
	app.OnRecordAfterUpdateSuccess("items").BindFunc(func(e *core.RecordEvent) error {
		p.publishItem(e.Record)
		return e.Next()
	})
	app.OnRecordAfterDeleteSuccess("items").BindFunc(func(e *core.RecordEvent) error {
		p.deleteItem(e.Record.GetString("code"))
		return e.Next()
	})

	// Users hooks. Same shape; password/auth fields never leave the
	// controller (see catalog.UserPayload).
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

func (p *CatalogPublisher) publishItem(rec *core.Record) {
	payload := catalog.ItemPayload{
		Code:         rec.GetString("code"),
		Name:         rec.GetString("name"),
		Type:         rec.GetString("type"),
		Unit:         rec.GetString("unit"),
		TrackingMode: rec.GetString("tracking_mode"),
		Category:     rec.GetString("category"),
		Active:       rec.GetBool("active"),
		Notes:        rec.GetString("notes"),
	}
	data, err := catalog.MarshalItem(payload)
	if err != nil {
		slog.Warn("controller.catalog.marshal_item_failed",
			"code", payload.Code, "error", err)
		return
	}
	if _, err := p.items.Put(context.Background(), payload.Code, data); err != nil {
		slog.Warn("controller.catalog.put_item_failed",
			"code", payload.Code, "error", err)
		return
	}
	slog.Info("controller.catalog.put_item", "code", payload.Code)
}

func (p *CatalogPublisher) deleteItem(code string) {
	if code == "" {
		return
	}
	if err := p.items.Delete(context.Background(), code); err != nil {
		slog.Warn("controller.catalog.delete_item_failed",
			"code", code, "error", err)
		return
	}
	slog.Info("controller.catalog.delete_item", "code", code)
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
