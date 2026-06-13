package handlers

import (
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/exports"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/timeclock"
)

// Self-service timeclock endpoints for the public virtual terminal
// (cmd/timeclock). Unlike the anonymous /api/kiosk/timeclock/* family — where
// the kiosk box is the trust boundary and user_code arrives in the body —
// these are AUTHENTICATED: the worker logs in (OAuth2 SSO or password) and the
// punched identity is read from re.Auth, NEVER from the request body. This is
// the same server-resolved-identity discipline commit enforces for
// OriginalCheckoutUserID: a worker can only ever punch their own clock.
//
// Only the timeclock binary registers these routes; the regular kiosk binary
// never wires them, so the authed surface doesn't exist where it isn't wanted.

// requireWorker enforces that the request carries a valid token for the
// `users` auth collection and that the worker is active, returning the auth
// record. Mirrors requireAdmin; the active check means a deactivated worker's
// still-valid token can't punch (the AuthRule blocks new logins, this blocks
// lingering sessions).
func (h *Handlers) requireWorker(re *core.RequestEvent) (*core.Record, error) {
	if re.Auth == nil {
		return nil, re.UnauthorizedError("authentication required", nil)
	}
	if re.Auth.Collection() == nil || re.Auth.Collection().Name != "users" {
		return nil, re.ForbiddenError("worker access required", nil)
	}
	if !re.Auth.GetBool("active") {
		return nil, re.ForbiddenError("worker is inactive", nil)
	}
	return re.Auth, nil
}

// SelfTimeclockStatus returns the authenticated worker's own merged
// clocked-in state — the same shape as TimeclockStatus, but identity comes
// from the session, so there is no user_code query param to spoof.
//
//	GET /api/self/timeclock/status   (worker auth required)
func (h *Handlers) SelfTimeclockStatus(re *core.RequestEvent) error {
	if err := h.timeclockGate(re); err != nil {
		return err
	}
	worker, err := h.requireWorker(re)
	if err != nil {
		return err
	}
	userCode := worker.GetString("code")
	state, err := timeclock.CurrentState(h.App, h.PunchFleet, worker.Id, userCode)
	if err != nil {
		return re.InternalServerError("read punch state", err)
	}
	return re.JSON(http.StatusOK, map[string]any{
		"user_id":         worker.Id,
		"user_code":       userCode,
		"user_name":       worker.GetString("name"),
		"user_role":       worker.GetString("role"),
		"clocked_in":      state.ClockedIn,
		"since":           state.OccurredAt,
		"origin":          state.Origin,
		"open_checkouts":  h.openCheckoutsForUser(worker.Id),
		"block_clock_out": h.Cfg.Timeclock.BlockClockOutWithOpenCheckouts,
	})
}

// SelfTimeclockPunch records the authenticated worker's own live punch. The
// body carries ONLY a direction; the target is always the session's worker.
// Backdating, force, and foreman/admin powers are deliberately absent — this
// is a SourceSelf punch and nothing else.
//
//	POST /api/self/timeclock/punch   (worker auth required)
//	body: { direction: "in"|"out" }
func (h *Handlers) SelfTimeclockPunch(re *core.RequestEvent) error {
	if err := h.timeclockGate(re); err != nil {
		return err
	}
	worker, err := h.requireWorker(re)
	if err != nil {
		return err
	}
	var body struct {
		Direction string `json:"direction"`
	}
	if err := re.BindBody(&body); err != nil {
		return re.BadRequestError("invalid request body", err)
	}

	in := timeclock.PunchInput{
		// Identity from the session, never the body — the trust invariant.
		TargetUserCode: worker.GetString("code"),
		Direction:      body.Direction,
		Source:         timeclock.SourceSelf,
	}
	res, err := timeclock.PerformPunch(h.App, h.PunchFleet, h.punchRules(), kioskctx.Get(), in)
	if err != nil {
		return h.punchError(re, err, in.TargetUserCode)
	}
	PublishPunchEvent(res, in, "")
	return re.JSON(http.StatusOK, res)
}

// SelfTimeclockHistory returns the authenticated worker's own punches plus the
// paired display view for a date range — read-only visibility into their
// timesheet. UserCode is forced to the session's worker, so the range filter
// can never widen to another person.
//
//	GET /api/self/timeclock/history?from=&to=   (worker auth required)
func (h *Handlers) SelfTimeclockHistory(re *core.RequestEvent) error {
	if err := h.timeclockGate(re); err != nil {
		return err
	}
	worker, err := h.requireWorker(re)
	if err != nil {
		return err
	}
	from, to, err := parseYMDRange(re)
	if err != nil {
		return err
	}
	punches, pairRows, err := exports.LoadTimeclockPunches(h.App, exports.TimeclockQueryOptions{
		From:     from,
		To:       to,
		UserCode: worker.GetString("code"),
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
