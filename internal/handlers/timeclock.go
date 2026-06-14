package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/exports"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/scan"
	"github.com/skeeeon/kiosk/internal/timeclock"
)

// Timeclock endpoints. Anonymous like the rest of /api/kiosk/* (the kiosk
// box is the trust boundary; identity arrives by badge scan) except the
// admin-punch path. Everything 404s when timeclock.enabled is false — the
// feature doesn't exist on kiosks that haven't opted in.

// timeclockGate short-circuits every timeclock route when the feature is
// off.
func (h *Handlers) timeclockGate(re *core.RequestEvent) error {
	if !h.Cfg.Timeclock.Enabled {
		return re.NotFoundError("timeclock is not enabled on this kiosk", nil)
	}
	return nil
}

// punchRules maps config onto the funnel's leaf-package policy struct.
func (h *Handlers) punchRules() timeclock.Rules {
	return timeclock.Rules{
		BlockClockOutWithOpenCheckouts: h.Cfg.Timeclock.BlockClockOutWithOpenCheckouts,
	}
}

// mergedOpenCheckoutsForUser is the display list for the clock-out gate: this
// kiosk's local open_checkouts (stamped with the local kiosk code) plus the
// CheckoutFleet replica's rows for OTHER kiosks, partitioned by kiosk_code so
// the two views never double-count. It mirrors exactly the merge the punch
// funnel blocks on, so the SPA's "return it at <building>" list matches the
// gate. A nil/empty replica (standalone, or KV down) yields just the local
// rows — fail-open for the cross-kiosk portion.
func (h *Handlers) mergedOpenCheckoutsForUser(userID, userCode string) []scan.OpenCheckoutDetail {
	selfKiosk := kioskctx.Get().KioskCode
	local := h.openCheckoutsForUser(userID)
	out := make([]scan.OpenCheckoutDetail, 0, len(local))
	for _, d := range local {
		d.KioskCode = selfKiosk
		out = append(out, d)
	}
	for _, r := range h.CheckoutFleet.RowsForOtherKiosks(userCode, selfKiosk) {
		out = append(out, scan.OpenCheckoutDetail{
			ItemCode:       r.ItemCode,
			ItemName:       r.ItemName,
			InstanceSerial: r.Serial,
			Qty:            1,
			KioskCode:      r.KioskCode,
		})
	}
	return out
}

// TimeclockStatus returns the merged clocked-in state for one user plus the
// context the SPA timeclock screen renders: last-punch origin, the user's
// open checkouts (shown when a clock-out would be blocked), and whether the
// block is configured at all.
//
//	GET /api/kiosk/timeclock/status?user_code=…
func (h *Handlers) TimeclockStatus(re *core.RequestEvent) error {
	if err := h.timeclockGate(re); err != nil {
		return err
	}
	userCode := re.Request.URL.Query().Get("user_code")
	if userCode == "" {
		return re.BadRequestError("user_code is required", nil)
	}
	user, err := h.App.FindFirstRecordByFilter("users", "code = {:c}", dbx.Params{"c": userCode})
	if isNotFound(err) {
		return re.NotFoundError("user not found", nil)
	}
	if err != nil {
		return err
	}
	state, err := timeclock.CurrentState(h.App, h.PunchFleet, user.Id, userCode)
	if err != nil {
		return re.InternalServerError("read punch state", err)
	}
	return re.JSON(http.StatusOK, map[string]any{
		"user_id":         user.Id,
		"user_code":       userCode,
		"user_name":       user.GetString("name"),
		"user_role":       user.GetString("role"),
		"clocked_in":      state.ClockedIn,
		"since":           state.OccurredAt,
		"origin":          state.Origin,
		"open_checkouts":  h.mergedOpenCheckoutsForUser(user.Id, userCode),
		"block_clock_out": h.Cfg.Timeclock.BlockClockOutWithOpenCheckouts,
		"today_seconds":   h.todayWorkedSeconds(userCode),
	})
}

// todayWorkedSeconds is the worker's CLOSED interval time for the current
// local calendar day — the kiosk panel's compact "X today" readout. The panel
// adds the live (still-open) session on top, so an open interval contributing
// 0 here is correct, not lossy. Scope is THIS kiosk's punch ledger: a managed
// kiosk doesn't hold other kiosks' punch rows, so the figure reads as "time at
// this kiosk today." Best-effort — a load error yields 0 rather than failing
// the whole status response.
//
// The query window is widened a day on each side because LoadTimeclockPunches
// filters occurred_at by UTC instants while Pair buckets by local day; the
// widening guarantees no local-day punch is clipped at the UTC boundary, and
// we read back only today's local bucket.
func (h *Handlers) todayWorkedSeconds(userCode string) int64 {
	nowLocal := time.Now().In(time.Local)
	today := nowLocal.Format("2006-01-02")
	_, pairRows, err := exports.LoadTimeclockPunches(h.App, exports.TimeclockQueryOptions{
		From:     nowLocal.AddDate(0, 0, -1).Format("2006-01-02"),
		To:       nowLocal.AddDate(0, 0, 1).Format("2006-01-02"),
		UserCode: userCode,
	})
	if err != nil {
		return 0
	}
	for _, dt := range timeclock.Pair(pairRows, time.Local).DayTotals {
		if dt.Date == today {
			return dt.Seconds
		}
	}
	return 0
}

// TimeclockPunch records a live punch: the worker's own (self) or a
// foreman's punch-now for a crew member when target_user_code differs from
// user_code. Backdating and force are NOT accepted here — that's the
// admin-punch endpoint.
//
//	POST /api/kiosk/timeclock/punch
//	body: { user_code, direction: "in"|"out", target_user_code? }
func (h *Handlers) TimeclockPunch(re *core.RequestEvent) error {
	if err := h.timeclockGate(re); err != nil {
		return err
	}
	var body struct {
		UserCode       string `json:"user_code"`
		TargetUserCode string `json:"target_user_code"`
		Direction      string `json:"direction"`
		// Acknowledge is the worker/foreman "clock out anyway" past the
		// open-checkouts block. Maps to the funnel's Force (which for a
		// non-admin source bypasses ONLY that block — see PunchInput.Force).
		Acknowledge bool `json:"acknowledge"`
	}
	if err := re.BindBody(&body); err != nil {
		return re.BadRequestError("invalid request body", err)
	}
	if body.UserCode == "" {
		return re.BadRequestError("user_code is required", nil)
	}

	in := timeclock.PunchInput{
		TargetUserCode: body.UserCode,
		Direction:      body.Direction,
		Source:         timeclock.SourceSelf,
		Force:          body.Acknowledge,
	}
	recordedByUserCode := ""
	if body.TargetUserCode != "" && body.TargetUserCode != body.UserCode {
		// Foreman path: the acting user is resolved server-side; the funnel
		// re-enforces the foreman+group gate inside the transaction.
		actor, err := h.App.FindFirstRecordByFilter("users", "code = {:c}", dbx.Params{"c": body.UserCode})
		if isNotFound(err) {
			return re.NotFoundError("user not found", nil)
		}
		if err != nil {
			return err
		}
		in.TargetUserCode = body.TargetUserCode
		in.Source = timeclock.SourceForeman
		in.ActorUserID = actor.Id
		recordedByUserCode = body.UserCode
	}

	res, err := timeclock.PerformPunch(h.App, h.PunchFleet, h.CheckoutFleet, h.punchRules(), kioskctx.Get(), in)
	if err != nil {
		return h.punchError(re, err, in.TargetUserCode)
	}
	PublishPunchEvent(res, in, recordedByUserCode)
	return re.JSON(http.StatusOK, res)
}

// TimeclockForemanOptions powers the foreman's "punch a crew member" picker:
// ACTIVE workers in the foreman's group (unlike foreman-return, punching is
// about people who are present), each with their merged clocked-in state so
// the dialog can offer the right verb.
//
//	GET /api/kiosk/timeclock/foreman/options?user_code=…
func (h *Handlers) TimeclockForemanOptions(re *core.RequestEvent) error {
	if err := h.timeclockGate(re); err != nil {
		return err
	}
	userCode := re.Request.URL.Query().Get("user_code")
	if userCode == "" {
		return re.BadRequestError("user_code is required", nil)
	}
	actor, err := h.App.FindFirstRecordByFilter("users", "code = {:c}", dbx.Params{"c": userCode})
	if isNotFound(err) {
		return re.NotFoundError("user not found", nil)
	}
	if err != nil {
		return err
	}
	if actor.GetString("role") != "foreman" {
		return re.ForbiddenError("only a foreman can punch crew members", nil)
	}
	groupID := actor.GetString("group")
	if groupID == "" {
		return re.ForbiddenError("foreman has no group set", nil)
	}
	group, err := h.App.FindRecordById("groups", groupID)
	if err != nil {
		return re.InternalServerError("find group", err)
	}
	members, err := h.App.FindRecordsByFilter("users",
		"group = {:g} && id != {:self} && active = true",
		"name", 0, 0,
		dbx.Params{"g": groupID, "self": actor.Id})
	if err != nil {
		return re.InternalServerError("find group members", err)
	}

	type crewMember struct {
		UserID    string    `json:"user_id"`
		UserCode  string    `json:"user_code"`
		UserName  string    `json:"user_name"`
		ClockedIn bool      `json:"clocked_in"`
		Since     time.Time `json:"since,omitzero"`
	}
	crew := make([]crewMember, 0, len(members))
	for _, m := range members {
		state, serr := timeclock.CurrentState(h.App, h.PunchFleet, m.Id, m.GetString("code"))
		if serr != nil {
			return re.InternalServerError("read punch state", serr)
		}
		crew = append(crew, crewMember{
			UserID:    m.Id,
			UserCode:  m.GetString("code"),
			UserName:  m.GetString("name"),
			ClockedIn: state.ClockedIn,
			Since:     state.OccurredAt,
		})
	}
	return re.JSON(http.StatusOK, map[string]any{
		"group_code": group.GetString("code"),
		"workers":    crew,
	})
}

// TimeclockAdminPunch records a manual/corrective punch: backdating (with
// reason) and force clock-out (the "drove home with a tool" escape hatch)
// are admin-only powers.
//
//	POST /api/kiosk/timeclock/admin-punch  (admin auth required)
//	body: { user_code, direction, reason, occurred_at?, force? }
func (h *Handlers) TimeclockAdminPunch(re *core.RequestEvent) error {
	if err := h.timeclockGate(re); err != nil {
		return err
	}
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	var body struct {
		UserCode   string `json:"user_code"`
		Direction  string `json:"direction"`
		Reason     string `json:"reason"`
		OccurredAt string `json:"occurred_at"`
		Force      bool   `json:"force"`
	}
	if err := re.BindBody(&body); err != nil {
		return re.BadRequestError("invalid request body", err)
	}
	in := timeclock.PunchInput{
		TargetUserCode: body.UserCode,
		Direction:      body.Direction,
		Source:         timeclock.SourceAdmin,
		ActorAdminID:   re.Auth.Id,
		Reason:         body.Reason,
		Force:          body.Force,
	}
	if body.OccurredAt != "" {
		t, err := time.Parse(time.RFC3339, body.OccurredAt)
		if err != nil {
			return re.BadRequestError("occurred_at must be RFC3339", err)
		}
		in.OccurredAt = t
	}
	res, err := timeclock.PerformPunch(h.App, h.PunchFleet, h.CheckoutFleet, h.punchRules(), kioskctx.Get(), in)
	if err != nil {
		return h.punchError(re, err, in.TargetUserCode)
	}
	PublishPunchEvent(res, in, "")
	return re.JSON(http.StatusOK, res)
}

// punchError maps the funnel's typed errors onto structured HTTP responses.
// The 409s carry a machine-readable `error` discriminator the SPA branches
// on (the open_checkouts shape additionally carries the blocking rows so
// the blocked-clock-out screen can render them without a second fetch).
func (h *Handlers) punchError(re *core.RequestEvent, err error, targetUserCode string) error {
	var oc *timeclock.OpenCheckoutsError
	switch {
	case errors.Is(err, timeclock.ErrUserNotFound):
		return re.NotFoundError("user not found", nil)
	case errors.Is(err, timeclock.ErrUserInactive):
		return re.BadRequestError("user is inactive", nil)
	case errors.Is(err, timeclock.ErrForemanGate):
		return re.ForbiddenError(timeclock.ErrForemanGate.Error(), nil)
	case errors.Is(err, timeclock.ErrAlreadyClockedIn):
		return re.JSON(http.StatusConflict, map[string]any{
			"error":   "already_clocked_in",
			"message": "Already clocked in.",
		})
	case errors.Is(err, timeclock.ErrNotClockedIn):
		return re.JSON(http.StatusConflict, map[string]any{
			"error":   "not_clocked_in",
			"message": "Not clocked in.",
		})
	case errors.As(err, &oc):
		// Hydrate the blocking rows for the SPA — local + fleet, the same merge
		// the funnel blocked on. The funnel only counted; the re-resolve here is
		// best-effort display data.
		var details []scan.OpenCheckoutDetail
		if u, uerr := h.App.FindFirstRecordByFilter("users", "code = {:c}",
			dbx.Params{"c": targetUserCode}); uerr == nil {
			details = h.mergedOpenCheckoutsForUser(u.Id, targetUserCode)
		}
		return re.JSON(http.StatusConflict, map[string]any{
			"error":          "open_checkouts",
			"message":        oc.Error(),
			"count":          oc.Count,
			"open_checkouts": details,
		})
	}
	return re.InternalServerError("punch failed", err)
}

// PublishPunchEvent emits the timeclock.punch event after a successful
// PerformPunch. Exported so the NATS command dispatcher publishes the same
// shape (PublishInventoryAdjustEvent precedent). Replayed (idempotent)
// results are skipped — the original attempt already published.
func PublishPunchEvent(res *timeclock.PunchResult, in timeclock.PunchInput, recordedByUserCode string) {
	if res == nil || res.Replayed {
		return
	}
	id := kioskctx.Get()
	events.Publish(events.TimeclockPunchSubject(id.KioskCode), events.BuildTimeclockPunchPayload(events.TimeclockPunchInput{
		PunchID:            res.PunchID,
		KioskCode:          id.KioskCode,
		LocationCode:       id.LocationCode,
		UserID:             res.UserID,
		UserCode:           res.UserCode,
		UserName:           res.UserName,
		Direction:          res.Direction,
		OccurredAt:         res.OccurredAt,
		Source:             res.Source,
		RecordedByUserCode: recordedByUserCode,
		AdminID:            in.ActorAdminID,
		ControllerAdminID:  in.ControllerAdminID,
		Reason:             in.Reason,
		Force:              in.Force,
		CommandID:          in.CommandID,
		RecordedAt:         res.RecordedAt,
	}))
}
