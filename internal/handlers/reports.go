// Reports endpoints serve cross-cutting read queries that don't fit cleanly
// into PB's collection REST API. The Currently-out / Aging tabs of the
// admin SPA use this on the controller (where there's no commit-maintained
// open_checkouts table to read directly — events from the fleet land as
// transactions + transaction_lines, which we replay on demand) and on the
// kiosk for symmetry.
package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/exports"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/ledger"
)

// ReportOpenCheckouts returns the rows currently checked out, computed by
// replaying the transaction_lines ledger. Optional ?kiosk_code= filter
// scopes the replay to one kiosk's transactions — useful on the controller
// where projected transactions span the whole fleet.
func (h *Handlers) ReportOpenCheckouts(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	kioskCode := re.Request.URL.Query().Get("kiosk_code")

	rows, err := ledger.ReplayOpenRows(h.App, kioskCode)
	if err != nil {
		return re.InternalServerError("replay open rows", err)
	}
	dtos, err := ledger.Hydrate(h.App, rows)
	if err != nil {
		return re.InternalServerError("hydrate open rows", err)
	}
	return re.JSON(http.StatusOK, dtos)
}

// ReportOpenCheckoutsCSV is the CSV companion to ReportOpenCheckouts. Same
// data path (replay + hydrate); only the response shaping differs.
func (h *Handlers) ReportOpenCheckoutsCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	kioskCode := re.Request.URL.Query().Get("kiosk_code")

	rows, err := ledger.ReplayOpenRows(h.App, kioskCode)
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
		"attachment; filename=\"open-checkouts-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteOpenCheckoutsCSV(w, dtos)
}

// ReportLowStockCSV streams the kiosk-local low-stock rows. Same shape the
// SPA's loadLowStock builds client-side: items + per-item open count + the
// deficit gate. KioskCode comes from kioskctx so the CSV row is meaningful
// even when concatenated with fleet-wide reports downstream.
func (h *Handlers) ReportLowStockCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}

	out, err := exports.ComputeLowStockRows(h.App, kioskctx.Get().KioskCode)
	if err != nil {
		return re.InternalServerError("compute low stock", err)
	}

	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"low-stock-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteLowStockCSV(w, out)
}

// ReportGroupActivityCSV streams the per-group rollup of completed
// transactions for the selected ?from / ?to window (YYYY-MM-DD). Optional
// ?kiosk_code= scopes fleet-wide data on the controller; on a kiosk it's
// redundant but accepted.
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
		"attachment; filename=\"group-activity-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteGroupActivityCSV(h.App, w, exports.GroupActivityOptions{
		From:      from,
		To:        to,
		KioskCode: re.Request.URL.Query().Get("kiosk_code"),
	})
}

// ReportLifecycleAuditCSV streams the kiosk-local instance_audit collection
// as CSV. FK columns (item, item_instance) are resolved via a bulk lookup so
// the output carries denormalized item/instance codes — same shape the
// controller's instance_lifecycle_audit collection emits.
//
// Filters: ?from / ?to (YYYY-MM-DD), ?action, ?source.
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
	// Item / instance search. The kiosk-local instance_audit keeps item +
	// item_instance as FKs (not denormalized), so we match through the
	// relation; the controller's projection has flat columns (see its twin).
	if q := re.Request.URL.Query().Get("q"); q != "" {
		parts = append(parts, "(item.code ~ {:q} || item.name ~ {:q} || item_instance.code ~ {:q})")
		params["q"] = q
	}
	filter := ""
	for i, p := range parts {
		if i > 0 {
			filter += " && "
		}
		filter += p
	}

	recs, err := h.App.FindRecordsByFilter("instance_audit", filter, "-created", 0, 0, params)
	if err != nil {
		return re.InternalServerError("load instance_audit", err)
	}

	itemIDs := map[string]struct{}{}
	instanceIDs := map[string]struct{}{}
	for _, r := range recs {
		if id := r.GetString("item"); id != "" {
			itemIDs[id] = struct{}{}
		}
		if id := r.GetString("item_instance"); id != "" {
			instanceIDs[id] = struct{}{}
		}
	}
	items := map[string]*core.Record{}
	for id := range itemIDs {
		if rec, ferr := h.App.FindRecordById("items", id); ferr == nil {
			items[id] = rec
		}
	}
	instances := map[string]*core.Record{}
	for id := range instanceIDs {
		if rec, ferr := h.App.FindRecordById("item_instances", id); ferr == nil {
			instances[id] = rec
		}
	}

	rows := make([]exports.LifecycleAuditRow, 0, len(recs))
	for _, r := range recs {
		itemID := r.GetString("item")
		instanceID := r.GetString("item_instance")
		var itemCode, itemName, instanceCode string
		if it := items[itemID]; it != nil {
			itemCode = it.GetString("code")
			itemName = it.GetString("name")
		}
		if inst := instances[instanceID]; inst != nil {
			instanceCode = inst.GetString("code")
		}
		rows = append(rows, exports.LifecycleAuditRow{
			Created:      r.GetDateTime("created").Time(),
			ItemCode:     itemCode,
			ItemName:     itemName,
			InstanceID:   instanceID,
			InstanceCode: instanceCode,
			Action:       r.GetString("action"),
			PrevStatus:   r.GetString("prev_status"),
			NewStatus:    r.GetString("new_status"),
			Source:       r.GetString("source"),
			Reason:       r.GetString("reason"),
			AdminID:      r.GetString("admin"),
		})
	}

	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"instance-lifecycle-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteLifecycleAuditCSV(w, rows)
}

// ReportNotificationsCSV streams the notification_send_log for the requested
// ?lookback_days window (default 7, max 90 to match the retention cron's cap).
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
		"attachment; filename=\"notifications-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteNotificationsLogCSV(h.App, w, exports.NotificationsLogOptions{LookbackDays: days})
}

// isLifecycleAction reports whether v is one of the instance-status verbs the
// lifecycle audit records (mirrors the SPA's Action dropdown). The controller's
// reports.go has a twin — the two packages stay decoupled by design, same
// reasoning as the duplicated requireAdmin / parseYMDRange.
func isLifecycleAction(v string) bool {
	switch v {
	case "create", "to_maintenance", "return_to_service", "retire", "unretire":
		return true
	}
	return false
}

// parseLookbackDays validates ?lookback_days. Empty means default; values
// outside [1, 90] are rejected — the retention cron prunes beyond 90 so an
// export past that horizon would silently return zero rows.
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

// parseYMDRange validates ?from / ?to as YYYY-MM-DD. Both are optional;
// either or both may be empty. Returns the canonical strings (re-formatted
// so a date like "2026-1-1" gets normalized) and any 400-error to bubble
// up to the SPA.
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
