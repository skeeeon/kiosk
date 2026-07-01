package commands

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/handlers"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/timeclock"
)

// timeclock.punch — the controller records a punch AT this kiosk on behalf
// of a controller admin. The kiosk stays the only punch writer: the funnel
// runs here, the event publishes from here, and the controller's own ledger
// learns about it the same way it learns about every punch — by consuming
// the event. Idempotent via command_id.
//
// Both timeclock commands reach config + the fleet replica through
// d.KioskHandlers (the same post-construction wiring the RFID commands use)
// and reply with an error when timeclock is disabled on this kiosk.

type timeclockPunchRequest struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	UserCode          string `json:"user_code"`
	Direction         string `json:"direction"`
	Reason            string `json:"reason"`
	OccurredAt        string `json:"occurred_at,omitempty"` // RFC3339; empty = now
	Force             bool   `json:"force,omitempty"`
	JobCode           string `json:"job_code,omitempty"`
	Note              string `json:"note,omitempty"`
}

func (d *Dispatcher) handleTimeclockPunch(_ context.Context, payload []byte) Reply {
	h := d.KioskHandlers
	if h == nil || !h.Cfg.Timeclock.Enabled {
		return Reply{Success: false, Error: "timeclock is not enabled on this kiosk"}
	}

	var req timeclockPunchRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return Reply{Success: false, Error: "invalid request body: " + err.Error()}
	}
	req.CommandID = strings.TrimSpace(req.CommandID)
	req.ControllerAdminID = strings.TrimSpace(req.ControllerAdminID)
	req.UserCode = strings.TrimSpace(req.UserCode)
	if req.CommandID == "" {
		return Reply{Success: false, Error: "command_id is required"}
	}
	if req.ControllerAdminID == "" {
		return Reply{Success: false, Error: "controller_admin_id is required"}
	}

	in := timeclock.PunchInput{
		TargetUserCode:    req.UserCode,
		Direction:         req.Direction,
		Source:            timeclock.SourceControllerAdmin,
		ControllerAdminID: req.ControllerAdminID,
		Reason:            req.Reason,
		Force:             req.Force,
		CommandID:         req.CommandID,
		JobCode:           req.JobCode,
		Note:              req.Note,
	}
	if req.OccurredAt != "" {
		t, err := time.Parse(time.RFC3339, req.OccurredAt)
		if err != nil {
			return Reply{Success: false, Error: "occurred_at must be RFC3339: " + err.Error()}
		}
		in.OccurredAt = t
	}

	rules := timeclock.Rules{
		BlockClockOutWithOpenCheckouts: h.Cfg.Timeclock.BlockClockOutWithOpenCheckouts,
	}
	res, err := timeclock.PerformPunch(d.app, h.PunchFleet, h.CheckoutFleet, rules, kioskctx.Get(), in)
	if err != nil {
		// Funnel errors are validation outcomes, not transport failures —
		// always reply (within the 5s window) so the controller renders the
		// message instead of "kiosk offline".
		var oc *timeclock.OpenCheckoutsError
		if errors.As(err, &oc) {
			return Reply{Success: false, Error: oc.Error()}
		}
		return Reply{Success: false, Error: err.Error()}
	}
	handlers.PublishPunchEvent(res, in, "")
	return Reply{Success: true, Data: res}
}

// timeclock.republish — re-emit punch events for a window so the controller
// can backfill its projection after a NATS outage. Read-only walk over
// time_punches; the projection's source_punch_id idempotency makes overlap
// harmless.

type timeclockRepublishRequest struct {
	From string `json:"from,omitempty"` // RFC3339
	To   string `json:"to,omitempty"`   // RFC3339
}

func (d *Dispatcher) handleTimeclockRepublish(_ context.Context, payload []byte) Reply {
	h := d.KioskHandlers
	if h == nil || !h.Cfg.Timeclock.Enabled {
		return Reply{Success: false, Error: "timeclock is not enabled on this kiosk"}
	}
	var req timeclockRepublishRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return Reply{Success: false, Error: "invalid request body: " + err.Error()}
		}
	}
	res, err := timeclock.RepublishPunches(d.app, req.From, req.To, events.Publish)
	if err != nil {
		return Reply{Success: false, Error: err.Error()}
	}
	return Reply{Success: true, Data: res}
}
