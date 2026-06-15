package controller

import (
	"fmt"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/exports"
	"github.com/skeeeon/kiosk/internal/notifications"
)

// ReportOpenCheckouts returns the fleet (or single-kiosk) currently-out view
// for the SPA. NATS-first: online kiosks answer from their own open_checkouts
// (history-independent); offline kiosks fall back to the controller's projected
// ledger (last-known) — see gatherOpenCheckouts. The response is the flat
// hydrated DTO array the SPA expects; per-kiosk provenance is surfaced on the
// scheduled digest rather than this interactive view (the operator sees live
// heartbeat status on the kiosks page). The ?kiosk_code= filter scopes to one
// kiosk. Takes nc + reg so it can fan out, mirroring the other live endpoints.
func (h *Handlers) ReportOpenCheckouts(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		rows, _, err := h.gatherOpenCheckouts(nc, reg, re.Request.URL.Query().Get("kiosk_code"))
		if err != nil {
			return re.InternalServerError("gather open checkouts", err)
		}
		return re.JSON(http.StatusOK, rows)
	}
}

// ReportOpenCheckoutsCSV mirrors ReportOpenCheckouts as a CSV download. Same
// gather path so the export stays consistent with the screen.
func (h *Handlers) ReportOpenCheckoutsCSV(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		rows, _, err := h.gatherOpenCheckouts(nc, reg, re.Request.URL.Query().Get("kiosk_code"))
		if err != nil {
			return re.InternalServerError("gather open checkouts", err)
		}
		w := re.Response
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(
			"attachment; filename=\"controller-open-checkouts-%s.csv\"",
			time.Now().UTC().Format("20060102-150405"),
		))
		return exports.WriteOpenCheckoutsCSV(w, rows)
	}
}

// OpenCheckoutsDigestRunner is the controller's override for the scheduler's
// "open_checkouts" report. NATS-first per kiosk with a replay fallback for
// offline kiosks (gatherOpenCheckouts), and it stamps the provenance breakdown
// onto the context so the digest body flags any last-known or unreachable
// kiosks instead of silently dropping them. Wired in cmd/controller/main.go via
// scheduler.RegisterRunner("open_checkouts", …), mirroring the maintenance
// override; the closure captures nc + the heartbeat registry the scheduler's
// (app, row)-only runner signature can't carry.
func (h *Handlers) OpenCheckoutsDigestRunner(nc *nats.Conn, reg *HeartbeatRegistry) func(core.App, *core.Record) (string, any, error) {
	return func(_ core.App, row *core.Record) (string, any, error) {
		kioskCode := row.GetString("kiosk_code")
		dtos, prov, err := h.gatherOpenCheckouts(nc, reg, kioskCode)
		if err != nil {
			return "", nil, fmt.Errorf("gather open checkouts: %w", err)
		}
		out := make([]notifications.OpenChecksDigestRow, 0, len(dtos))
		for _, d := range dtos {
			r := notifications.OpenChecksDigestRow{Serial: d.Serial, CheckedOutAt: d.CheckedOutAt}
			if d.Expand.Item != nil {
				r.ItemCode = d.Expand.Item.Code
				r.ItemName = d.Expand.Item.Name
			}
			if d.Expand.User != nil {
				r.UserCode = d.Expand.User.Code
				r.UserName = d.Expand.User.Name
			}
			out = append(out, r)
		}
		return notifications.EventTypeOpenChecksDigest, notifications.OpenChecksDigestContext{
			Kiosk:           notifications.KioskInfo{Code: kioskCode},
			GeneratedAt:     time.Now().UTC(),
			Rows:            out,
			RowsCount:       len(out),
			KioskProvenance: prov,
		}, nil
	}
}

// ReportGroupActivityCSV is the controller's CSV companion to the SPA's
// Group Activity tab. Same query/aggregation as the kiosk version; the
// optional ?kiosk_code= filter slices the fleet view to one kiosk.
func (h *Handlers) ReportGroupActivityCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	from, to, err := parseYMDRange(re)
	if err != nil {
		return err
	}

	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"controller-group-activity-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteGroupActivityCSV(h.App, w, exports.GroupActivityOptions{
		From:      from,
		To:        to,
		KioskCode: re.Request.URL.Query().Get("kiosk_code"),
	})
}

// ReportAdjustmentAuditCSV streams the fleet-wide inventory_audit collection
// as CSV. Filters mirror the SPA's Adjustment Audit tab: ?from / ?to
// (YYYY-MM-DD), ?kiosk_code, ?source ("local" | "controller").
func (h *Handlers) ReportAdjustmentAuditCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	from, to, err := parseYMDRange(re)
	if err != nil {
		return err
	}
	source := re.Request.URL.Query().Get("source")
	if source != "" && source != "local" && source != "controller" {
		return re.BadRequestError("source must be 'local' or 'controller'", nil)
	}

	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"controller-adjustment-audit-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteAdjustmentAuditCSV(h.App, w, exports.AdjustmentAuditOptions{
		From:      from,
		To:        to,
		KioskCode: re.Request.URL.Query().Get("kiosk_code"),
		Source:    source,
		Item:      re.Request.URL.Query().Get("q"),
	})
}

// ReportLifecycleAuditCSV streams the fleet-wide instance_lifecycle_audit
// collection. Columns are already denormalized in the projection, so the
// handler is a plain query + map → exports.LifecycleAuditRow. Filters mirror
// the SPA's lifecycle tab: ?from / ?to (YYYY-MM-DD), ?action, ?source,
// ?kiosk_code.
func (h *Handlers) ReportLifecycleAuditCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	from, to, err := parseYMDRange(re)
	if err != nil {
		return err
	}
	action := re.Request.URL.Query().Get("action")
	source := re.Request.URL.Query().Get("source")
	if action != "" && !isLifecycleAction(action) {
		return re.BadRequestError("action must be create | to_maintenance | return_to_service | retire | unretire", nil)
	}
	if source != "" && source != "local" && source != "controller" {
		return re.BadRequestError("source must be 'local' or 'controller'", nil)
	}

	parts := []string{}
	params := dbx.Params{}
	if from != "" {
		parts = append(parts, "created >= {:from}")
		params["from"] = from + " 00:00:00.000Z"
	}
	if to != "" {
		parts = append(parts, "created <= {:to}")
		params["to"] = to + " 23:59:59.999Z"
	}
	if action != "" {
		parts = append(parts, "action = {:a}")
		params["a"] = action
	}
	if source != "" {
		parts = append(parts, "source = {:s}")
		params["s"] = source
	}
	if kc := re.Request.URL.Query().Get("kiosk_code"); kc != "" {
		parts = append(parts, "kiosk_code = {:k}")
		params["k"] = kc
	}
	// Item / instance search against the projection's denormalized columns
	// (the kiosk twin matches through FK relations instead).
	if q := re.Request.URL.Query().Get("q"); q != "" {
		parts = append(parts, "(item_code ~ {:q} || item_name ~ {:q} || instance_code ~ {:q})")
		params["q"] = q
	}
	filter := ""
	for i, p := range parts {
		if i > 0 {
			filter += " && "
		}
		filter += p
	}

	recs, err := h.App.FindRecordsByFilter("instance_lifecycle_audit", filter, "-created", 0, 0, params)
	if err != nil {
		return re.InternalServerError("load instance_lifecycle_audit", err)
	}

	rows := make([]exports.LifecycleAuditRow, 0, len(recs))
	for _, r := range recs {
		rows = append(rows, exports.LifecycleAuditRow{
			Created:      r.GetDateTime("created").Time(),
			KioskCode:    r.GetString("kiosk_code"),
			ItemCode:     r.GetString("item_code"),
			ItemName:     r.GetString("item_name"),
			InstanceID:   r.GetString("instance_id"),
			InstanceCode: r.GetString("instance_code"),
			Action:       r.GetString("action"),
			PrevStatus:   r.GetString("prev_status"),
			NewStatus:    r.GetString("new_status"),
			Source:       r.GetString("source"),
			Reason:       r.GetString("reason"),
			AdminID:      r.GetString("admin_id"),
		})
	}

	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"controller-instance-lifecycle-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteLifecycleAuditCSV(w, rows)
}

// ReportNotificationsCSV streams the fleet-wide notification_send_log for
// the requested ?lookback_days window (default 7, max 90).
func (h *Handlers) ReportNotificationsCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	days, err := parseLookbackDays(re)
	if err != nil {
		return err
	}

	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"controller-notifications-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteNotificationsLogCSV(h.App, w, exports.NotificationsLogOptions{LookbackDays: days})
}

// isLifecycleAction mirrors handlers.isLifecycleAction — the instance-status
// verbs the lifecycle audit records. Duplicated to keep the controller package
// free of the kiosk-side handlers dependency (same reasoning as requireAdmin).
func isLifecycleAction(v string) bool {
	switch v {
	case "create", "to_maintenance", "return_to_service", "retire", "unretire":
		return true
	}
	return false
}

// parseLookbackDays mirrors handlers.parseLookbackDays.
func parseLookbackDays(re *core.RequestEvent) (int, error) {
	raw := re.Request.URL.Query().Get("lookback_days")
	if raw == "" {
		return 0, nil
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, re.BadRequestError("lookback_days must be an integer", err)
	}
	if n < 1 || n > 90 {
		return 0, re.BadRequestError("lookback_days must be between 1 and 90", nil)
	}
	return n, nil
}

// parseYMDRange mirrors handlers.parseYMDRange. Duplicated to keep the
// controller package free of the kiosk-side handlers dependency — same
// reasoning as requireAdmin.
func parseYMDRange(re *core.RequestEvent) (string, string, error) {
	from := re.Request.URL.Query().Get("from")
	to := re.Request.URL.Query().Get("to")
	if from != "" {
		t, err := time.Parse("2006-01-02", from)
		if err != nil {
			return "", "", re.BadRequestError("from must be YYYY-MM-DD", err)
		}
		from = t.Format("2006-01-02")
	}
	if to != "" {
		t, err := time.Parse("2006-01-02", to)
		if err != nil {
			return "", "", re.BadRequestError("to must be YYYY-MM-DD", err)
		}
		to = t.Format("2006-01-02")
	}
	return from, to, nil
}
