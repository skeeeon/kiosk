package exports

import (
	"encoding/csv"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/timeclock"
)

// Timeclock report plumbing shared by the kiosk handlers (local punches) and
// the controller handlers (fleet-wide projected punches). Same split as the
// other reports: fetch/aggregate here, presentation in the CSV writer, HTTP
// shaping in the per-binary handler.

// TimeclockQueryOptions filter the punch-history load. From/To are
// YYYY-MM-DD (inclusive, validated by the handler) applied against
// occurred_at — the business timestamp, so a backdated correction shows up
// on the day it amends, not the day it was typed.
type TimeclockQueryOptions struct {
	From      string
	To        string
	UserCode  string
	KioskCode string // controller-side fleet filter; empty on a kiosk
}

// PunchExportRow is one flattened punch — the raw-punch contract the CSV
// exports for payroll and the JSON history endpoint returns alongside the
// paired intervals.
type PunchExportRow struct {
	PunchID      string    `json:"punch_id"`
	OccurredAt   time.Time `json:"occurred_at"`
	RecordedAt   time.Time `json:"recorded_at"`
	UserID       string    `json:"user_id"`
	UserCode     string    `json:"user_code"`
	UserName     string    `json:"user_name"`
	Direction    string    `json:"direction"`
	Source       string    `json:"source"`
	RecordedBy   string    `json:"recorded_by,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Force        bool      `json:"force,omitempty"`
	KioskCode    string    `json:"kiosk_code"`
	LocationCode string    `json:"location_code,omitempty"`
	CommandID    string    `json:"command_id,omitempty"`
}

// LoadTimeclockPunches reads time_punches under the given filters, returning
// both the flattened export rows (occurred_at ascending) and the
// timeclock.PunchRow projection ready for Pair(). user_name and the
// foreman/admin recorded_by are resolved best-effort via bulk lookups.
func LoadTimeclockPunches(app core.App, opts TimeclockQueryOptions) ([]PunchExportRow, []timeclock.PunchRow, error) {
	filter := ""
	params := dbx.Params{}
	add := func(expr string) {
		if filter != "" {
			filter += " && "
		}
		filter += expr
	}
	if opts.From != "" {
		add("occurred_at >= {:from}")
		params["from"] = opts.From + " 00:00:00.000Z"
	}
	if opts.To != "" {
		add("occurred_at <= {:to}")
		params["to"] = opts.To + " 23:59:59.999Z"
	}
	if opts.UserCode != "" {
		add("user_code = {:uc}")
		params["uc"] = opts.UserCode
	}
	if opts.KioskCode != "" {
		add("kiosk_code = {:kc}")
		params["kc"] = opts.KioskCode
	}

	recs, err := app.FindRecordsByFilter(timeclock.Collection, filter, "occurred_at,created", 0, 0, params)
	if err != nil {
		return nil, nil, err
	}

	userIDs := map[string]struct{}{}
	adminIDs := map[string]struct{}{}
	for _, r := range recs {
		userIDs[r.GetString("user")] = struct{}{}
		if id := r.GetString("recorded_by_user"); id != "" {
			userIDs[id] = struct{}{}
		}
		if id := r.GetString("recorded_by_admin"); id != "" {
			adminIDs[id] = struct{}{}
		}
	}
	users := map[string]*core.Record{}
	for id := range userIDs {
		if id == "" {
			continue
		}
		if rec, ferr := app.FindRecordById("users", id); ferr == nil {
			users[id] = rec
		}
	}
	admins := map[string]*core.Record{}
	for id := range adminIDs {
		if rec, ferr := app.FindRecordById("admins", id); ferr == nil {
			admins[id] = rec
		}
	}

	rows := make([]PunchExportRow, 0, len(recs))
	pairRows := make([]timeclock.PunchRow, 0, len(recs))
	for _, r := range recs {
		userID := r.GetString("user")
		userName := ""
		if u := users[userID]; u != nil {
			userName = u.GetString("name")
		}
		recordedBy := ""
		switch r.GetString("source") {
		case timeclock.SourceForeman:
			if u := users[r.GetString("recorded_by_user")]; u != nil {
				recordedBy = u.GetString("code")
			}
		case timeclock.SourceAdmin:
			if a := admins[r.GetString("recorded_by_admin")]; a != nil {
				recordedBy = a.GetString("name")
			}
		case timeclock.SourceControllerAdmin:
			recordedBy = r.GetString("controller_admin_id")
		}
		rows = append(rows, PunchExportRow{
			PunchID:      r.Id,
			OccurredAt:   r.GetDateTime("occurred_at").Time(),
			RecordedAt:   r.GetDateTime("created").Time(),
			UserID:       userID,
			UserCode:     r.GetString("user_code"),
			UserName:     userName,
			Direction:    r.GetString("direction"),
			Source:       r.GetString("source"),
			RecordedBy:   recordedBy,
			Reason:       r.GetString("reason"),
			Force:        r.GetBool("force"),
			KioskCode:    r.GetString("kiosk_code"),
			LocationCode: r.GetString("location_code"),
			CommandID:    r.GetString("command_id"),
		})
		pairRows = append(pairRows, timeclock.PunchRow{
			ID:         r.Id,
			UserID:     userID,
			UserCode:   r.GetString("user_code"),
			UserName:   userName,
			Direction:  r.GetString("direction"),
			OccurredAt: r.GetDateTime("occurred_at").Time(),
			Created:    r.GetDateTime("created").Time(),
		})
	}
	return rows, pairRows, nil
}

// WriteTimeclockCSV streams raw punches — THE payroll contract. Deliberately
// no pairing, no totals, no rounding: downstream systems interpret;
// timestamps are UTC RFC3339.
func WriteTimeclockCSV(w io.Writer, rows []PunchExportRow) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"occurred_at", "user_code", "user_name", "direction",
		"source", "recorded_by", "reason", "force",
		"kiosk_code", "location_code", "recorded_at", "punch_id",
	}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := writeRow(cw, []string{
			r.OccurredAt.UTC().Format(time.RFC3339),
			r.UserCode, r.UserName, r.Direction,
			r.Source, r.RecordedBy, r.Reason, strconv.FormatBool(r.Force),
			r.KioskCode, r.LocationCode,
			r.RecordedAt.UTC().Format(time.RFC3339),
			r.PunchID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ClockedInRow is one currently-clocked-in user for the "who's in now" view.
type ClockedInRow struct {
	UserID   string    `json:"user_id"`
	UserCode string    `json:"user_code"`
	UserName string    `json:"user_name"`
	Since    time.Time `json:"since"`
	Origin   string    `json:"origin"`
}

// ComputeClockedInNow walks every user and reports the clocked-in ones via
// the timeclock merge rule — so on a managed kiosk the list reflects fleet
// state, and on the controller (nil fleet) it reflects the projected ledger.
// One LatestPunch query per user; workforce sizes here are hundreds, not
// millions.
func ComputeClockedInNow(app core.App, fleet *timeclock.Fleet) ([]ClockedInRow, error) {
	users, err := app.FindRecordsByFilter("users", "", "name", 0, 0)
	if err != nil {
		return nil, err
	}
	out := []ClockedInRow{}
	for _, u := range users {
		state, serr := timeclock.CurrentState(app, fleet, u.Id, u.GetString("code"))
		if serr != nil {
			return nil, serr
		}
		if !state.ClockedIn {
			continue
		}
		out = append(out, ClockedInRow{
			UserID:   u.Id,
			UserCode: u.GetString("code"),
			UserName: u.GetString("name"),
			Since:    state.OccurredAt,
			Origin:   state.Origin,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out, nil
}
