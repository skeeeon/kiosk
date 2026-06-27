package controller

import (
	"fmt"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/ledger"
	"github.com/skeeeon/kiosk/internal/notifications"
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
	disc, staleHrs, err := h.computeReconciliation(now)
	if err != nil {
		return re.InternalServerError("reconcile", err)
	}
	return re.JSON(http.StatusOK, map[string]any{
		"discrepancies":   disc,
		"generated_at":    now,
		"stale_after_hrs": staleHrs,
	})
}

// computeReconciliation gathers fleet-wide location (the site-wide
// instance_location view) and custody (replaying each kiosk's projected ledger)
// and runs the pure reconcile join. Shared by the HTTP endpoint and the
// scheduled digest runner so the on-demand view and the emailed report can't
// diverge. Returns the discrepancies and the configured stale threshold (hrs).
func (h *Handlers) computeReconciliation(now time.Time) ([]reconcile.Discrepancy, float64, error) {
	// Location: the whole site-wide view. Also build a (kiosk,instance_id) →
	// display-meta map so custody rows can show a code/name.
	locRows, err := h.App.FindRecordsByFilter("instance_location", "", "", 0, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("load instance_location: %w", err)
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
		return nil, 0, fmt.Errorf("load kiosks: %w", err)
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
			return nil, 0, fmt.Errorf("replay ledger for %s: %w", code, err)
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
	return reconcile.Reconcile(custody, location, cfg, now), cfg.StaleAfter.Hours(), nil
}

// ReconciliationDigestRunner is the controller's override for the scheduler's
// "reconciliation" report (wired in cmd/controller/main.go via
// scheduler.RegisterRunner). Unlike the maintenance/open-checkouts overrides it
// needs no NATS fan-out: the controller's reconciliation reads its own DB (the
// projected ledger + instance_location), so it just reuses computeReconciliation.
//
// computeReconciliation is whole-fleet, so we deliberately do NOT honor the
// row's kiosk_code — leaving Kiosk.Code empty keeps the subject honest (the
// fleet-wide convention, same as a kiosk_code-less maintenance digest) rather
// than falsely scoping it; per-kiosk reconciliation scoping is deferred. Each
// row still carries its own kiosk via the body's "@ <code>" annotation.
func (h *Handlers) ReconciliationDigestRunner() func(core.App, *core.Record) (string, any, error) {
	return func(_ core.App, _ *core.Record) (string, any, error) {
		now := time.Now().UTC()
		disc, staleHrs, err := h.computeReconciliation(now)
		if err != nil {
			return "", nil, err
		}
		return notifications.EventTypeReconciliationDigest,
			notifications.BuildReconciliationDigest(notifications.KioskInfo{}, disc, staleHrs, now), nil
	}
}
