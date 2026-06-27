package controller

import (
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/ledger"
	"github.com/skeeeon/kiosk/internal/sightings"
)

// Locations is the controller's fleet-wide advisory location report
// (docs/location-sightings-plan.md, L4) — the inverse of Reconciliation: every
// serialized unit seen across the fleet, with last-seen zone/age and (if out)
// its holder. Admin-gated, observability-only.
//
// Location comes from the site-wide instance_location view; holder from
// replaying each kiosk's projected ledger (the same path Reconciliation uses);
// item identity from instance_epc_index — the resolution record the
// SightingIngest already maintains, and exactly the set of units that can land
// in instance_location (a sighting only persists after resolving through the
// index), so the join covers every rendered row.
//
// Endpoint: GET /api/controller/locations
func (h *Handlers) Locations(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}

	// Item identity per resolved unit, keyed by instance_id (bounded — one row
	// per tagged unit). Best-effort: a missing index row just leaves the item
	// columns blank, the instance_code still identifies the unit.
	type itemMeta struct{ code, name string }
	metaByInstance := map[string]itemMeta{}
	if idxRows, err := h.App.FindRecordsByFilter("instance_epc_index", "", "", 0, 0); err == nil {
		for _, r := range idxRows {
			metaByInstance[r.GetString("instance_id")] = itemMeta{
				code: r.GetString("item_code"),
				name: r.GetString("item_name"),
			}
		}
	}

	// Holder per (kiosk_code, instance_id) from each kiosk's projected ledger.
	holderByKey := map[string]string{}
	kioskRecs, err := h.App.FindRecordsByFilter("kiosks", "", "kiosk_code", 0, 0)
	if err != nil {
		return re.InternalServerError("load kiosks", err)
	}
	userCache := map[string]string{}
	for _, kr := range kioskRecs {
		code := kr.GetString("kiosk_code")
		if code == "" {
			continue
		}
		rows, err := ledger.ReplayOpenRows(h.App, code)
		if err != nil {
			return re.InternalServerError("replay ledger for "+code, err)
		}
		for _, row := range rows {
			if row.ItemInstance == "" {
				continue // serialized units only carry location
			}
			holder, ok := userCache[row.User]
			if !ok {
				if u, _ := h.App.FindRecordById("users", row.User); u != nil {
					holder = u.GetString("name")
				}
				userCache[row.User] = holder
			}
			holderByKey[code+"\x00"+row.ItemInstance] = holder
		}
	}

	locRows, err := h.App.FindRecordsByFilter("instance_location", "", "-last_observed_at", 0, 0)
	if err != nil {
		return re.InternalServerError("load instance_location", err)
	}
	out := make([]sightings.LocationRow, 0, len(locRows))
	for _, r := range locRows {
		kc := r.GetString("kiosk_code")
		iid := r.GetString("instance_id")
		row := sightings.LocationRow{
			KioskCode:    kc,
			InstanceCode: r.GetString("instance_code"),
			Zone:         r.GetString("last_observed_zone"),
			Gateway:      r.GetString("last_observed_gateway"),
			Lat:          r.GetFloat("last_observed_lat"),
			Lon:          r.GetFloat("last_observed_lon"),
			ObservedAt:   r.GetDateTime("last_observed_at").Time(),
			Holder:       holderByKey[kc+"\x00"+iid],
		}
		if m, ok := metaByInstance[iid]; ok {
			row.ItemCode, row.ItemName = m.code, m.name
		}
		out = append(out, row)
	}

	return re.JSON(http.StatusOK, map[string]any{
		"locations":    out,
		"generated_at": time.Now().UTC(),
	})
}
