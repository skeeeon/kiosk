package timeclock

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/dberr"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// Typed errors the HTTP/command layers map to structured responses. The
// open-checkouts block carries a count, so it's a struct with an Is hook
// rather than a bare sentinel — errors.Is(err, ErrOpenCheckouts) works and
// errors.As recovers the detail.
var (
	ErrNotClockedIn     = errors.New("not clocked in")
	ErrAlreadyClockedIn = errors.New("already clocked in")
	ErrUserNotFound     = errors.New("user not found")
	ErrUserInactive     = errors.New("user is inactive")
	ErrForemanGate      = errors.New("foreman punches require an active foreman with a group, targeting a worker in the same group")
	ErrOpenCheckouts    = errors.New("open checkouts block clock-out")
)

// OpenCheckoutsError is the concrete clock-out-blocked error; Count is how
// many open_checkouts rows the worker holds at this kiosk.
type OpenCheckoutsError struct {
	Count int
}

func (e *OpenCheckoutsError) Error() string {
	return fmt.Sprintf("%d open checkout(s) must be returned before clocking out", e.Count)
}

func (e *OpenCheckoutsError) Is(target error) bool { return target == ErrOpenCheckouts }

// errIdempotentReplay — same sentinel dance as PerformStockAdjustment: the
// txn callback signals "this command_id already applied," the (empty) txn
// rolls back, and the outer code re-reads the prior row.
var errIdempotentReplay = errors.New("idempotent replay")

// Rules are the funnel-enforced policy knobs, decoupled from the config
// package so this stays a leaf. Handlers map config.TimeclockConfig onto it.
type Rules struct {
	BlockClockOutWithOpenCheckouts bool
}

// PunchInput describes one punch attempt. TargetUserCode is resolved against
// the users collection INSIDE the transaction (fresh read — same trust
// stance as commit's foreman gate); callers never pass record ids for the
// target. Exactly one actor field is required per non-self source.
type PunchInput struct {
	TargetUserCode string
	Direction      string // DirectionIn | DirectionOut
	Source         string // SourceSelf | SourceForeman | SourceAdmin | SourceControllerAdmin

	ActorUserID       string // foreman's users.id (Source == foreman)
	ActorAdminID      string // local admins.id (Source == admin)
	ControllerAdminID string // controller admin's PB id, opaque text (Source == controller_admin)

	// OccurredAt zero means "now" (server-stamped). Non-zero (backdating)
	// is allowed only for admin/controller_admin sources.
	OccurredAt time.Time

	// Reason is required for admin/controller_admin punches (they are always
	// manual interventions); ignored-if-empty for live punches.
	Reason string

	// Force lets an admin/controller_admin clock-out bypass the
	// open-checkouts block — the "worker drove home with a tool" escape
	// hatch. Rejected for self/foreman.
	Force bool

	// CommandID is the idempotency key for command-bus punches (unique when
	// non-empty). Empty for local HTTP punches.
	CommandID string
}

// PunchResult is what the funnel returns and what the HTTP/command layers
// serialize. ClockedIn is the user's MERGED state after the punch (a
// backdated correction may not change it).
type PunchResult struct {
	PunchID    string    `json:"punch_id"`
	UserID     string    `json:"user_id"`
	UserCode   string    `json:"user_code"`
	UserName   string    `json:"user_name"`
	Direction  string    `json:"direction"`
	OccurredAt time.Time `json:"occurred_at"`
	RecordedAt time.Time `json:"recorded_at"`
	Source     string    `json:"source"`
	ClockedIn  bool      `json:"clocked_in"`
	Replayed   bool      `json:"replayed,omitempty"`
}

// futureSkew is how far ahead of the server clock an occurred_at may sit
// before being rejected — generous enough for honest clock drift between an
// admin's browser and the kiosk, tight enough that nobody pre-records
// tomorrow's punches.
const futureSkew = time.Minute

// PerformPunch is the ONLY writer of time_punches. Every path — kiosk
// touchscreen, foreman crew punch, local admin correction, controller
// command — funnels through here so the validation rules hold by
// construction. It does NOT publish the timeclock.punch event; callers do
// that post-commit (Perform/Publish split, stock_adjust precedent) and skip
// it on Replayed results.
func PerformPunch(app core.App, fleet *Fleet, rules Rules, id kioskctx.Identity, in PunchInput) (*PunchResult, error) {
	in.TargetUserCode = strings.TrimSpace(in.TargetUserCode)
	in.Reason = strings.TrimSpace(in.Reason)
	if in.TargetUserCode == "" {
		return nil, fmt.Errorf("target user code is required")
	}
	if in.Direction != DirectionIn && in.Direction != DirectionOut {
		return nil, fmt.Errorf("direction must be %q or %q", DirectionIn, DirectionOut)
	}
	if id.KioskCode == "" {
		return nil, fmt.Errorf("kiosk identity is not set")
	}

	adminSource := in.Source == SourceAdmin || in.Source == SourceControllerAdmin
	switch in.Source {
	case SourceSelf:
		// no actor fields
	case SourceForeman:
		if in.ActorUserID == "" {
			return nil, fmt.Errorf("foreman punch requires the acting foreman's user id")
		}
	case SourceAdmin:
		if in.ActorAdminID == "" {
			return nil, fmt.Errorf("admin punch requires the acting admin's id")
		}
	case SourceControllerAdmin:
		if in.ControllerAdminID == "" {
			return nil, fmt.Errorf("controller punch requires the controller admin's id")
		}
	default:
		return nil, fmt.Errorf("invalid source %q", in.Source)
	}
	if adminSource && in.Reason == "" {
		return nil, fmt.Errorf("reason is required for %s punches", in.Source)
	}
	if !adminSource {
		if !in.OccurredAt.IsZero() {
			return nil, fmt.Errorf("only admin punches may set occurred_at; %s punches are stamped now", in.Source)
		}
		if in.Force {
			return nil, fmt.Errorf("only admin punches may force past the open-checkouts block")
		}
	}
	now := time.Now().UTC()
	occurredAt := in.OccurredAt.UTC()
	if in.OccurredAt.IsZero() {
		occurredAt = now
	} else if occurredAt.After(now.Add(futureSkew)) {
		return nil, fmt.Errorf("occurred_at %s is in the future", occurredAt.Format(time.RFC3339))
	}

	var out PunchResult
	err := app.RunInTransaction(func(tx core.App) error {
		if in.CommandID != "" {
			existing, lerr := tx.FindFirstRecordByFilter(Collection,
				"command_id = {:c}", dbx.Params{"c": in.CommandID})
			if lerr == nil && existing != nil {
				return errIdempotentReplay
			}
			if lerr != nil && !dberr.IsNotFound(lerr) {
				return fmt.Errorf("idempotency lookup: %w", lerr)
			}
		}

		target, err := tx.FindFirstRecordByFilter("users",
			"code = {:c}", dbx.Params{"c": in.TargetUserCode})
		if err != nil {
			if dberr.IsNotFound(err) {
				return ErrUserNotFound
			}
			return fmt.Errorf("find user %s: %w", in.TargetUserCode, err)
		}
		// Live punches require an active worker; admins may correct the
		// record of someone who has since been deactivated.
		if !adminSource && !target.GetBool("active") {
			return ErrUserInactive
		}

		// Foreman gate — re-read role + group from the DB inside the txn,
		// exactly like commit's cross-user gate. The HTTP pre-flight is UX;
		// this is the trust boundary.
		if in.Source == SourceForeman {
			actor, err := tx.FindRecordById("users", in.ActorUserID)
			if err != nil {
				return fmt.Errorf("find foreman %s: %w", in.ActorUserID, err)
			}
			if actor.GetString("role") != "foreman" || !actor.GetBool("active") {
				return ErrForemanGate
			}
			group := actor.GetString("group")
			if group == "" || target.GetString("group") != group {
				return ErrForemanGate
			}
		}

		// Alternation — live punches only. Admin punches bypass (a
		// correction routinely writes "out" into a sequence that currently
		// ends "out"); their required reason is the audit trail. The check
		// uses the MERGED state so a clock-in at kiosk A allows the
		// clock-out at kiosk B.
		if !adminSource {
			state, err := CurrentState(tx, fleet, target.Id, in.TargetUserCode)
			if err != nil {
				return fmt.Errorf("read punch state: %w", err)
			}
			if in.Direction == DirectionIn && state.ClockedIn {
				return ErrAlreadyClockedIn
			}
			if in.Direction == DirectionOut && !state.ClockedIn {
				return ErrNotClockedIn
			}
		}

		// Open-checkouts block — local-scoped by design (v1): tools out at
		// another kiosk don't block a clock-out here. Applies to admin
		// punches too unless Force.
		if in.Direction == DirectionOut && rules.BlockClockOutWithOpenCheckouts && !(adminSource && in.Force) {
			rows, err := tx.FindRecordsByFilter("open_checkouts",
				"user = {:u}", "", 0, 0, dbx.Params{"u": target.Id})
			if err != nil {
				return fmt.Errorf("count open checkouts: %w", err)
			}
			if len(rows) > 0 {
				return &OpenCheckoutsError{Count: len(rows)}
			}
		}

		col, err := tx.FindCollectionByNameOrId(Collection)
		if err != nil {
			return fmt.Errorf("find %s collection: %w", Collection, err)
		}
		rec := core.NewRecord(col)
		rec.Set("user", target.Id)
		rec.Set("user_code", in.TargetUserCode)
		rec.Set("direction", in.Direction)
		rec.Set("occurred_at", occurredAt)
		rec.Set("source", in.Source)
		switch in.Source {
		case SourceForeman:
			rec.Set("recorded_by_user", in.ActorUserID)
		case SourceAdmin:
			rec.Set("recorded_by_admin", in.ActorAdminID)
		case SourceControllerAdmin:
			rec.Set("controller_admin_id", in.ControllerAdminID)
		}
		if in.Reason != "" {
			rec.Set("reason", in.Reason)
		}
		if in.Force {
			rec.Set("force", true)
		}
		rec.Set("kiosk_code", id.KioskCode)
		rec.Set("location_code", id.LocationCode)
		if in.CommandID != "" {
			rec.Set("command_id", in.CommandID)
		}
		if err := tx.Save(rec); err != nil {
			if in.CommandID != "" && dberr.IsUniqueViolation(err) {
				return errIdempotentReplay
			}
			return fmt.Errorf("save punch: %w", err)
		}

		// Merged state after the punch — a backdated correction may leave
		// the user's current state untouched, and the UI shows this value.
		state, err := CurrentState(tx, fleet, target.Id, in.TargetUserCode)
		if err != nil {
			return fmt.Errorf("re-read punch state: %w", err)
		}

		out = PunchResult{
			PunchID:    rec.Id,
			UserID:     target.Id,
			UserCode:   in.TargetUserCode,
			UserName:   target.GetString("name"),
			Direction:  in.Direction,
			OccurredAt: occurredAt,
			RecordedAt: rec.GetDateTime("created").Time(),
			Source:     in.Source,
			ClockedIn:  state.ClockedIn,
		}
		return nil
	})
	if errors.Is(err, errIdempotentReplay) {
		return fetchPunchByCommandID(app, fleet, in.CommandID)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// fetchPunchByCommandID re-reads the prior row after either idempotency
// detection path so a replayed command returns the same result shape.
func fetchPunchByCommandID(app core.App, fleet *Fleet, commandID string) (*PunchResult, error) {
	rec, err := app.FindFirstRecordByFilter(Collection,
		"command_id = {:c}", dbx.Params{"c": commandID})
	if err != nil {
		return nil, fmt.Errorf("idempotent replay re-fetch: %w", err)
	}
	userID := rec.GetString("user")
	userCode := rec.GetString("user_code")
	state, err := CurrentState(app, fleet, userID, userCode)
	if err != nil {
		return nil, err
	}
	res := &PunchResult{
		PunchID:    rec.Id,
		UserID:     userID,
		UserCode:   userCode,
		Direction:  rec.GetString("direction"),
		OccurredAt: rec.GetDateTime("occurred_at").Time(),
		RecordedAt: rec.GetDateTime("created").Time(),
		Source:     rec.GetString("source"),
		ClockedIn:  state.ClockedIn,
		Replayed:   true,
	}
	if u, uerr := app.FindRecordById("users", userID); uerr == nil {
		res.UserName = u.GetString("name")
	}
	return res, nil
}
