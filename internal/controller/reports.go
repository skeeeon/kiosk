package controller

import (
	"fmt"
	"net/http"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/exports"
	"github.com/skeeeon/kiosk/internal/ledger"
	"github.com/skeeeon/kiosk/internal/scheduler"
)

// replayFleetOpenRows computes the cross-fleet open set without loading the
// whole transaction_lines table in a single query: it iterates the kiosks
// registry and concatenates each kiosk's bounded replay (ledger.ReplayOpenRows
// scopes the query per kiosk), so peak memory stays at one kiosk's history
// rather than the entire fleet's. Cross-kiosk order is irrelevant — FIFO
// correlation is per-(item,instance,user) within a single kiosk's ledger — and
// ledger.Hydrate surfaces kiosk_code per row, so the concatenated view renders
// correctly. The registry is the same fleet list the heartbeat/touch paths
// maintain; a kiosk that has transacted but isn't registered (shouldn't happen
// — TouchKiosk registers on first transaction) would be omitted here.
func replayFleetOpenRows(app core.App) ([]ledger.OpenRow, error) {
	kiosks, err := app.FindRecordsByFilter("kiosks", "", "kiosk_code", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load kiosks: %w", err)
	}
	var out []ledger.OpenRow
	for _, k := range kiosks {
		code := k.GetString("kiosk_code")
		if code == "" {
			continue
		}
		rows, err := ledger.ReplayOpenRows(app, code)
		if err != nil {
			return nil, fmt.Errorf("replay kiosk %s: %w", code, err)
		}
		out = append(out, rows...)
	}
	return out, nil
}

// replayOpenRows picks the bounded single-kiosk replay when a kiosk_code is
// given, or the per-kiosk fan-out across the fleet when it's empty. Either way
// it never loads the entire transaction_lines table in one query.
func replayOpenRows(app core.App, kioskCode string) ([]ledger.OpenRow, error) {
	if kioskCode != "" {
		return ledger.ReplayOpenRows(app, kioskCode)
	}
	return replayFleetOpenRows(app)
}

// OpenCheckoutsDigestRunner is the controller's override for the scheduler's
// "open_checkouts" report. For a fleet-wide row (empty kiosk_code) it fans out
// per kiosk via replayFleetOpenRows instead of replaying the entire projected
// ledger in one query — otherwise the unattended digest cron would OOM at
// fleet scale; a kiosk-scoped row uses the bounded single-kiosk replay. Wired
// in cmd/controller/main.go via scheduler.RegisterRunner("open_checkouts", …),
// mirroring the maintenance-digest override.
func OpenCheckoutsDigestRunner(app core.App, row *core.Record) (string, any, error) {
	kioskCode := row.GetString("kiosk_code")
	rows, err := replayOpenRows(app, kioskCode)
	if err != nil {
		return "", nil, fmt.Errorf("replay open rows: %w", err)
	}
	return scheduler.BuildOpenChecksDigest(app, kioskCode, rows)
}

// ReportOpenCheckouts mirrors the kiosk's reports endpoint for cross-fleet
// use, computed by replaying the projected transaction_lines ledger — the
// same single code path the kiosk uses (handlers.ReportOpenCheckouts). The
// controller no longer materializes an open_checkouts table; replaying the
// ledger on demand is convergent by construction, so the controller's view
// can't drift from a kiosk's the way an incrementally-maintained projection
// could. The ?kiosk_code= filter slices the fleet view to one kiosk.
func (h *Handlers) ReportOpenCheckouts(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	kioskCode := re.Request.URL.Query().Get("kiosk_code")

	rows, err := replayOpenRows(h.App, kioskCode)
	if err != nil {
		return re.InternalServerError("replay open rows", err)
	}
	dtos, err := ledger.Hydrate(h.App, rows)
	if err != nil {
		return re.InternalServerError("hydrate open rows", err)
	}
	return re.JSON(http.StatusOK, dtos)
}

// ReportOpenCheckoutsCSV mirrors ReportOpenCheckouts as a CSV download. Same
// data path (replay + hydrate) so the export stays consistent with the screen.
func (h *Handlers) ReportOpenCheckoutsCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	kioskCode := re.Request.URL.Query().Get("kiosk_code")

	rows, err := replayOpenRows(h.App, kioskCode)
	if err != nil {
		return re.InternalServerError("replay open rows", err)
	}
	dtos, err := ledger.Hydrate(h.App, rows)
	if err != nil {
		return re.InternalServerError("hydrate open rows", err)
	}

	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"controller-open-checkouts-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteOpenCheckoutsCSV(w, dtos)
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
	if action != "" && action != "create" && action != "decommission" && action != "reactivate" && action != "delete" {
		return re.BadRequestError("action must be create | decommission | reactivate | delete", nil)
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
