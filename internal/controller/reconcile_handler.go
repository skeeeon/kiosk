package controller

import (
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/ledger"
	"github.com/skeeeon/kiosk/internal/reconcile"
)

// Reconciliation is the controller's fleet-wide custody-vs-location report
// (docs/location-sightings-plan.md, L4). Admin-gated, observability-only.
// Custody comes from replaying each kiosk's projected ledger; location from the
// site-wide instance_location view. Joined by (kiosk_code, instance_id).
//
// CustodyZones must be set explicitly in the controller's config to enable the
// not-taken flag (the controller has no RFID reader zones to default from); the
// stale and unaccounted flags work without it.
//
// Endpoint: GET /api/controller/reconciliation
func (h *Handlers) Reconciliation(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	now := time.Now().UTC()

	// Location: the whole site-wide view. Also build a (kiosk,instance_id) →
	// display-meta map so custody rows can show a code/name.
	locRows, err := h.App.FindRecordsByFilter("instance_location", "", "", 0, 0)
	if err != nil {
		return re.InternalServerError("load instance_location", err)
	}
	type meta struct{ code, name string }
	metaByKey := make(map[string]meta, len(locRows))
	location := make([]reconcile.LocationState, 0, len(locRows))
	for _, r := range locRows {
		kc := r.GetString("kiosk_code")
		iid := r.GetString("instance_id")
		ls := reconcile.LocationState{
			KioskCode:    kc,
			InstanceID:   iid,
			InstanceCode: r.GetString("instance_code"),
			ItemName:     r.GetString("item_code"),
			Zone:         r.GetString("last_observed_zone"),
			ObservedAt:   r.GetDateTime("last_observed_at").Time(),
		}
		location = append(location, ls)
		metaByKey[kc+"\x00"+iid] = meta{code: ls.InstanceCode, name: ls.ItemName}
	}

	// Custody: replay each kiosk's projected ledger for its open serialized rows.
	kioskRecs, err := h.App.FindRecordsByFilter("kiosks", "", "kiosk_code", 0, 0)
	if err != nil {
		return re.InternalServerError("load kiosks", err)
	}
	var custody []reconcile.CustodyState
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
			m := metaByKey[code+"\x00"+row.ItemInstance]
			custody = append(custody, reconcile.CustodyState{
				KioskCode:    code,
				InstanceID:   row.ItemInstance,
				InstanceCode: m.code,
				ItemName:     m.name,
				Holder:       holder,
				CheckedOutAt: row.CheckedOutAt,
			})
		}
	}

	cfg := reconcile.Config{
		StaleAfter:   h.Cfg.Location.StaleAfter.AsDuration(),
		CustodyZones: reconcile.CustodyZoneSet(h.Cfg.Location.CustodyZones),
	}
	disc := reconcile.Reconcile(custody, location, cfg, now)

	return re.JSON(http.StatusOK, map[string]any{
		"discrepancies":   disc,
		"generated_at":    now,
		"stale_after_hrs": cfg.StaleAfter.Hours(),
	})
}
