package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/exports"
	"github.com/skeeeon/kiosk/internal/timeclock"
)

// Timeclock endpoints. Two families:
//
//   - Remote actions proxy a command to the target kiosk (the kiosk stays
//     the ONLY punch writer; the controller's own ledger learns about the
//     punch the same way it learns about every punch — by consuming the
//     event). Same dispatch shape as inventory.adjust.
//   - Fleet reports read the controller's OWN projected time_punches — no
//     kiosk fan-out, no WAN dependency.

// timeclockPunchCommandPayload mirrors the kiosk dispatcher's
// timeclockPunchRequest (internal/commands/timeclock.go).
type timeclockPunchCommandPayload struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	UserCode          string `json:"user_code"`
	Direction         string `json:"direction"`
	Reason            string `json:"reason"`
	OccurredAt        string `json:"occurred_at,omitempty"`
	Force             bool   `json:"force,omitempty"`
	JobCode           string `json:"job_code,omitempty"`
}

// TimeclockPunch records a manual punch at a kiosk on behalf of a controller
// admin. Backdating + force follow the same admin rules as the kiosk's local
// admin-punch endpoint; the kiosk's funnel enforces them.
//
//	POST /api/controller/kiosks/{code}/timeclock/punch
//	body: { user_code, direction, reason, occurred_at?, force? }
func (h *Handlers) TimeclockPunch(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := re.Request.PathValue("code")
		if kioskCode == "" {
			return re.BadRequestError("kiosk code is required", nil)
		}
		var body struct {
			UserCode   string `json:"user_code"`
			Direction  string `json:"direction"`
			Reason     string `json:"reason"`
			OccurredAt string `json:"occurred_at"`
			Force      bool   `json:"force"`
			JobCode    string `json:"job_code"`
		}
		if err := re.BindBody(&body); err != nil {
			return re.BadRequestError("invalid request body", err)
		}
		if body.OccurredAt != "" {
			if _, err := time.Parse(time.RFC3339, body.OccurredAt); err != nil {
				return re.BadRequestError("occurred_at must be RFC3339", err)
			}
		}

		commandID := uuid.NewString()
		payload, err := json.Marshal(timeclockPunchCommandPayload{
			CommandID:         commandID,
			ControllerAdminID: re.Auth.Id,
			UserCode:          body.UserCode,
			Direction:         body.Direction,
			Reason:            body.Reason,
			OccurredAt:        body.OccurredAt,
			Force:             body.Force,
			JobCode:           body.JobCode,
		})
		if err != nil {
			return re.InternalServerError("marshal command", err)
		}
		return dispatchKioskCommand(re, nc, reg, kioskCode,
			events.TimeclockPunchCommandSubject(kioskCode), commandID, payload)
	}
}

// TimeclockRepublish asks a kiosk to re-emit its punch events for an
// optional RFC3339 window — projection backfill after a NATS outage.
//
//	POST /api/controller/kiosks/{code}/timeclock/republish
//	body: { from?, to? }
func (h *Handlers) TimeclockRepublish(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := re.Request.PathValue("code")
		if kioskCode == "" {
			return re.BadRequestError("kiosk code is required", nil)
		}
		var body struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		_ = re.BindBody(&body)

		payload, err := json.Marshal(map[string]string{
			"from": body.From,
			"to":   body.To,
		})
		if err != nil {
			return re.InternalServerError("marshal command", err)
		}
		return dispatchKioskCommand(re, nc, reg, kioskCode,
			events.TimeclockRepublishCommandSubject(kioskCode), "", payload)
	}
}

// TimeclockNow lists everyone currently clocked in fleet-wide, derived from
// the controller's projected punch ledger (nil fleet — the controller IS the
// source the kiosk replicas mirror).
//
//	GET /api/controller/timeclock/now
func (h *Handlers) TimeclockNow(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	rows, err := exports.ComputeClockedInNow(h.App, nil)
	if err != nil {
		return re.InternalServerError("compute clocked-in", err)
	}
	return re.JSON(http.StatusOK, map[string]any{"rows": rows})
}

// TimeclockHistory returns fleet punches + the paired display view.
//
//	GET /api/controller/timeclock/history?from=&to=&user_code=&kiosk_code=
func (h *Handlers) TimeclockHistory(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	from, to, err := parseYMDRange(re)
	if err != nil {
		return err
	}
	punches, pairRows, err := exports.LoadTimeclockPunches(h.App, exports.TimeclockQueryOptions{
		From:      from,
		To:        to,
		UserCode:  re.Request.URL.Query().Get("user_code"),
		KioskCode: re.Request.URL.Query().Get("kiosk_code"),
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

// ReportTimeclockCSV streams raw fleet punches — the payroll contract, with
// kiosk_code so downstream can demultiplex by site.
//
//	GET /api/controller/reports/timeclock.csv?from=&to=&user_code=&kiosk_code=
func (h *Handlers) ReportTimeclockCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	from, to, err := parseYMDRange(re)
	if err != nil {
		return err
	}
	punches, _, err := exports.LoadTimeclockPunches(h.App, exports.TimeclockQueryOptions{
		From:      from,
		To:        to,
		UserCode:  re.Request.URL.Query().Get("user_code"),
		KioskCode: re.Request.URL.Query().Get("kiosk_code"),
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
