// Package handlers holds HTTP handlers for /api/kiosk/*. Each route is a
// method on the Handlers struct so it can access shared dependencies (the
// PocketBase app for DB reads, the in-memory cart store, and config).
package handlers

import (
	"database/sql"
	"errors"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/scan"
)

type Handlers struct {
	App   core.App
	Cfg   *config.Config
	Carts *cart.Store
}

func New(app core.App, cfg *config.Config, carts *cart.Store) *Handlers {
	return &Handlers{App: app, Cfg: cfg, Carts: carts}
}

// requireAdmin enforces that the request carries a valid token for the
// admins auth collection. Returns a 401/403 RequestEvent error on miss.
// PocketBase populates re.Auth from a `Authorization: Bearer ...` header.
func (h *Handlers) requireAdmin(re *core.RequestEvent) error {
	if re.Auth == nil {
		return re.UnauthorizedError("authentication required", nil)
	}
	if re.Auth.Collection() == nil || re.Auth.Collection().Name != "admins" {
		return re.ForbiddenError("admin access required", nil)
	}
	return nil
}

// isNotFound reports whether an error means "no record matched" — either a
// PB sql.ErrNoRows or our local notFoundErr sentinel from multi-source
// resolvers like resolveScannableForCart.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	var nf *notFoundErr
	return errors.As(err, &nf)
}

// userFromRecord projects a PB users record into the SPA-facing shape.
// OpenCount is left at zero; callers that need it (the scan handler)
// populate it via countOpenCheckoutsForUser.
func userFromRecord(r *core.Record) *scan.User {
	return &scan.User{
		ID:    r.Id,
		Code:  r.GetString("code"),
		Name:  r.GetString("name"),
		Role:  r.GetString("role"),
		Email: r.GetString("email"),
	}
}

// itemFromRecord projects a PB items record into the SPA-facing shape.
// Active and QuantityOnHand are filled from the record; OpenCount and
// Holder default to zero/empty and are populated by the scan handler when
// it returns the item to the SPA (cart browse skips them — it doesn't need
// identify metadata).
func itemFromRecord(r *core.Record) *scan.Item {
	return &scan.Item{
		ID:             r.Id,
		Code:           r.GetString("code"),
		Name:           r.GetString("name"),
		Type:           r.GetString("type"),
		Unit:           r.GetString("unit"),
		TrackingMode:   r.GetString("tracking_mode"),
		Serial:         r.GetString("serial"),
		Category:       r.GetString("category"),
		Active:         r.GetBool("active"),
		QuantityOnHand: r.GetInt("quantity_on_hand"),
	}
}
