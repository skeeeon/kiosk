package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/exports"
	"github.com/skeeeon/kiosk/internal/timeclock"
)

// Timeclock admin reports. Same conventions as reports.go: JSON for the SPA
// tabs, CSV via internal/exports for download. Day bucketing in the paired
// view uses the serving binary's local timezone; the CSV stays raw UTC
// punches (the payroll contract).

// TimeclockNow lists everyone currently clocked in (merge rule — fleet
// state included on managed kiosks).
//
//	GET /api/kiosk/timeclock/now
func (h *Handlers) TimeclockNow(re *core.RequestEvent) error {
	if err := h.timeclockGate(re); err != nil {
		return err
	}
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	rows, err := exports.ComputeClockedInNow(h.App, h.PunchFleet)
	if err != nil {
		return re.InternalServerError("compute clocked-in", err)
	}
	return re.JSON(http.StatusOK, map[string]any{"rows": rows})
}

// TimeclockHistory returns raw punches plus the paired display view for a
// date range.
//
//	GET /api/kiosk/timeclock/history?from=&to=&user_code=
func (h *Handlers) TimeclockHistory(re *core.RequestEvent) error {
	if err := h.timeclockGate(re); err != nil {
		return err
	}
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	from, to, err := parseYMDRange(re)
	if err != nil {
		return err
	}
	punches, pairRows, err := exports.LoadTimeclockPunches(h.App, exports.TimeclockQueryOptions{
		From:     from,
		To:       to,
		UserCode: re.Request.URL.Query().Get("user_code"),
	})
	if err != nil {
		return re.InternalServerError("load punches", err)
	}
	paired := timeclock.Pair(pairRows, time.Local)
	return re.JSON(http.StatusOK, map[string]any{
		"punches":      punches,
		"intervals":    paired.Intervals,
		"day_totals":   paired.DayTotals,
		"uncorrelated": paired.Uncorrelated,
	})
}

// TimeclockRepublish re-emits punch events for an optional RFC3339 window —
// the punch sibling of RepublishLedger, for controller backfill after a
// NATS outage. The controller projection is idempotent on source_punch_id,
// so overlap is harmless.
//
//	POST /api/kiosk/timeclock/republish  (admin auth required)
//	body: { from?, to? }  // RFC3339
func (h *Handlers) TimeclockRepublish(re *core.RequestEvent) error {
	if err := h.timeclockGate(re); err != nil {
		return err
	}
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	_ = re.BindBody(&body)

	result, err := timeclock.RepublishPunches(h.App, body.From, body.To, events.Publish)
	if err != nil {
		return re.BadRequestError(err.Error(), nil)
	}
	return re.JSON(http.StatusOK, result)
}

// ReportTimeclockCSV streams the raw punches for the window — no pairing,
// no rounding; downstream payroll interprets.
//
//	GET /api/kiosk/reports/timeclock.csv?from=&to=&user_code=
func (h *Handlers) ReportTimeclockCSV(re *core.RequestEvent) error {
	if err := h.timeclockGate(re); err != nil {
		return err
	}
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	from, to, err := parseYMDRange(re)
	if err != nil {
		return err
	}
	punches, _, err := exports.LoadTimeclockPunches(h.App, exports.TimeclockQueryOptions{
		From:     from,
		To:       to,
		UserCode: re.Request.URL.Query().Get("user_code"),
	})
	if err != nil {
		return re.InternalServerError("load punches", err)
	}

	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"timeclock-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteTimeclockCSV(w, punches)
}
