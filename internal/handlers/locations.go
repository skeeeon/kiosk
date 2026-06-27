package handlers

import (
	"net/http"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/sightings"
)

// Locations is the node-local advisory location report
// (docs/location-sightings-plan.md, L4) — the inverse of Reconciliation: every
// serialized unit this node has ever seen, with its last-seen zone/age and (if
// out) its holder. Admin-gated, observability-only. Empty until a gateway or a
// zoned custody reader reports (the SPA renders an empty-state then).
//
// Endpoint: GET /api/kiosk/locations
func (h *Handlers) Locations(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	kioskCode := kioskctx.Get().KioskCode

	insts, err := h.App.FindRecordsByFilter("item_instances",
		"last_observed_at != ''", "-last_observed_at", 0, 0, dbx.Params{})
	if err != nil {
		return re.InternalServerError("load observed instances", err)
	}

	holderByInstance, err := h.holdersByInstance()
	if err != nil {
		return re.InternalServerError("load holders", err)
	}

	rows := make([]sightings.LocationRow, 0, len(insts))
	itemCache := map[string]*core.Record{}
	for _, r := range insts {
		itemID := r.GetString("item")
		item, ok := itemCache[itemID]
		if !ok {
			item, _ = h.App.FindRecordById("items", itemID)
			itemCache[itemID] = item
		}
		row := sightings.LocationRow{
			KioskCode:    kioskCode,
			InstanceCode: r.GetString("code"),
			Zone:         r.GetString("last_observed_zone"),
			Gateway:      r.GetString("last_observed_gateway"),
			Lat:          r.GetFloat("last_observed_lat"),
			Lon:          r.GetFloat("last_observed_lon"),
			ObservedAt:   r.GetDateTime("last_observed_at").Time(),
			Status:       r.GetString("status"),
			Holder:       holderByInstance[r.Id],
		}
		if item != nil {
			row.ItemCode = item.GetString("code")
			row.ItemName = item.GetString("name")
		}
		rows = append(rows, row)
	}

	return re.JSON(http.StatusOK, map[string]any{
		"locations":    rows,
		"generated_at": time.Now().UTC(),
	})
}

// holdersByInstance maps a serialized item_instance id → holder name from the
// live open_checkouts view, so the location report can annotate a sighting with
// who currently has the unit out. Mirrors localCustodyStates' hydration.
func (h *Handlers) holdersByInstance() (map[string]string, error) {
	rows, err := h.App.FindRecordsByFilter("open_checkouts",
		"item_instance != ''", "", 0, 0, dbx.Params{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	userCache := map[string]string{}
	for _, r := range rows {
		instID := r.GetString("item_instance")
		if instID == "" {
			continue
		}
		userID := r.GetString("user")
		holder, ok := userCache[userID]
		if !ok {
			if u, _ := h.App.FindRecordById("users", userID); u != nil {
				holder = u.GetString("name")
			}
			userCache[userID] = holder
		}
		out[instID] = holder
	}
	return out, nil
}
