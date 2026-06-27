package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/reconcile"
)

// Reconciliation is the node-local custody-vs-location report
// (docs/location-sightings-plan.md, L4). Admin-gated, observability-only: it
// joins this node's open_checkouts (custody) with its item_instances'
// last_observed_* (location) and returns the flagged discrepancies. No
// enforcement — a discrepancy is a hint, never an action.
//
// Endpoint: GET /api/kiosk/reconciliation
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

// computeReconciliation gathers this node's custody (open_checkouts) and
// location (item_instances.last_observed_*) and runs the pure reconcile join.
// Shared by the HTTP endpoint and the scheduled digest runner so the on-demand
// view and the emailed report can't diverge. Returns the discrepancies and the
// configured stale threshold in hours (for the report header).
func (h *Handlers) computeReconciliation(now time.Time) ([]reconcile.Discrepancy, float64, error) {
	kioskCode := kioskctx.Get().KioskCode
	custody, err := h.localCustodyStates(kioskCode)
	if err != nil {
		return nil, 0, fmt.Errorf("load custody: %w", err)
	}
	location, err := h.localLocationStates(kioskCode)
	if err != nil {
		return nil, 0, fmt.Errorf("load location: %w", err)
	}
	cfg := reconcile.Config{
		StaleAfter:   h.Cfg.Location.StaleAfter.AsDuration(),
		CustodyZones: reconcile.CustodyZoneSet(h.Cfg.CustodyZoneSet()),
	}
	return reconcile.Reconcile(custody, location, cfg, now), cfg.StaleAfter.Hours(), nil
}

// ReconciliationDigestRunner is the STANDALONE reconciliation-digest runner,
// registered from cmd/kiosk via scheduler.RegisterRunner("reconciliation", …).
// It can't be a bare scheduler runner because it needs config (stale threshold
// + custody zones), which the (app, row) signature can't carry — so it's a
// handler-method closure capturing h, the same pattern as the controller's
// MaintenanceDigestRunner. The controller overrides it with a fleet runner.
func (h *Handlers) ReconciliationDigestRunner() func(core.App, *core.Record) (string, any, error) {
	return func(_ core.App, _ *core.Record) (string, any, error) {
		now := time.Now().UTC()
		disc, staleHrs, err := h.computeReconciliation(now)
		if err != nil {
			return "", nil, err
		}
		id := kioskctx.Get()
		kiosk := notifications.KioskInfo{Code: id.KioskCode, LocationCode: id.LocationCode}
		return notifications.EventTypeReconciliationDigest,
			notifications.BuildReconciliationDigest(kiosk, disc, staleHrs, now), nil
	}
}

// localCustodyStates reads the live open_checkouts view for serialized units
// (item_instance set) and hydrates instance code + holder name.
func (h *Handlers) localCustodyStates(kioskCode string) ([]reconcile.CustodyState, error) {
	rows, err := h.App.FindRecordsByFilter("open_checkouts", "item_instance != ''", "", 0, 0, dbx.Params{})
	if err != nil {
		return nil, err
	}
	out := make([]reconcile.CustodyState, 0, len(rows))
	instCache := map[string]*core.Record{}
	userCache := map[string]string{}
	itemCache := map[string]string{}
	for _, r := range rows {
		instID := r.GetString("item_instance")
		inst, ok := instCache[instID]
		if !ok {
			inst, _ = h.App.FindRecordById("item_instances", instID)
			instCache[instID] = inst
		}
		if inst == nil {
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
		itemID := inst.GetString("item")
		itemName, ok := itemCache[itemID]
		if !ok {
			if it, _ := h.App.FindRecordById("items", itemID); it != nil {
				itemName = it.GetString("name")
			}
			itemCache[itemID] = itemName
		}
		out = append(out, reconcile.CustodyState{
			KioskCode:    kioskCode,
			InstanceID:   instID,
			InstanceCode: inst.GetString("code"),
			ItemName:     itemName,
			Holder:       holder,
			CheckedOutAt: r.GetDateTime("checked_out_at").Time(),
		})
	}
	return out, nil
}

// localLocationStates reads every instance that has been observed.
func (h *Handlers) localLocationStates(kioskCode string) ([]reconcile.LocationState, error) {
	rows, err := h.App.FindRecordsByFilter("item_instances", "last_observed_at != ''", "", 0, 0, dbx.Params{})
	if err != nil {
		return nil, err
	}
	out := make([]reconcile.LocationState, 0, len(rows))
	itemCache := map[string]string{}
	for _, r := range rows {
		itemID := r.GetString("item")
		itemName, ok := itemCache[itemID]
		if !ok {
			if it, _ := h.App.FindRecordById("items", itemID); it != nil {
				itemName = it.GetString("name")
			}
			itemCache[itemID] = itemName
		}
		out = append(out, reconcile.LocationState{
			KioskCode:    kioskCode,
			InstanceID:   r.Id,
			InstanceCode: r.GetString("code"),
			ItemName:     itemName,
			Zone:         r.GetString("last_observed_zone"),
			ObservedAt:   r.GetDateTime("last_observed_at").Time(),
		})
	}
	return out, nil
}
