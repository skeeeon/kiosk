package timeclock

import (
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
)

// RepublishResult summarizes a punch republish pass.
type RepublishResult struct {
	Published int    `json:"published"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
}

// RepublishPunches re-emits timeclock.punch events for every time_punches
// row in the optional [from, to] occurred_at window (RFC3339; empty = open
// ended). Sibling of PerformLedgerRepublish, kept separate because a flat
// punches walk shares nothing with the transactions walk. Safe to replay:
// the controller's projection is idempotent on source_punch_id, and its KV
// punch-state write is monotonic on occurred_at, so old punches can't drag
// fleet state backwards.
func RepublishPunches(app core.App, from, to string, publish func(subject string, payload any)) (*RepublishResult, error) {
	filter := ""
	params := dbx.Params{}
	add := func(expr string) {
		if filter != "" {
			filter += " && "
		}
		filter += expr
	}
	if from != "" {
		t, err := time.Parse(time.RFC3339, from)
		if err != nil {
			return nil, fmt.Errorf("from must be RFC3339: %w", err)
		}
		add("occurred_at >= {:from}")
		params["from"] = t.UTC()
	}
	if to != "" {
		t, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return nil, fmt.Errorf("to must be RFC3339: %w", err)
		}
		add("occurred_at <= {:to}")
		params["to"] = t.UTC()
	}

	recs, err := app.FindRecordsByFilter(Collection, filter, "occurred_at,created", 0, 0, params)
	if err != nil {
		return nil, fmt.Errorf("load punches: %w", err)
	}

	// Bulk-resolve foreman recorders to user codes so the republished shape
	// matches the live one.
	recorderIDs := map[string]struct{}{}
	for _, r := range recs {
		if id := r.GetString("recorded_by_user"); id != "" {
			recorderIDs[id] = struct{}{}
		}
	}
	recorderCode := map[string]string{}
	for id := range recorderIDs {
		if u, ferr := app.FindRecordById("users", id); ferr == nil {
			recorderCode[id] = u.GetString("code")
		}
	}

	for _, r := range recs {
		userName := ""
		if u, ferr := app.FindRecordById("users", r.GetString("user")); ferr == nil {
			userName = u.GetString("name")
		}
		kioskCode := r.GetString("kiosk_code")
		publish(events.TimeclockPunchSubject(kioskCode), events.BuildTimeclockPunchPayload(events.TimeclockPunchInput{
			PunchID:            r.Id,
			KioskCode:          kioskCode,
			LocationCode:       r.GetString("location_code"),
			UserID:             r.GetString("user"),
			UserCode:           r.GetString("user_code"),
			UserName:           userName,
			Direction:          r.GetString("direction"),
			OccurredAt:         r.GetDateTime("occurred_at").Time(),
			Source:             r.GetString("source"),
			RecordedByUserCode: recorderCode[r.GetString("recorded_by_user")],
			AdminID:            r.GetString("recorded_by_admin"),
			ControllerAdminID:  r.GetString("controller_admin_id"),
			Reason:             r.GetString("reason"),
			Force:              r.GetBool("force"),
			CommandID:          r.GetString("command_id"),
			JobCode:            r.GetString("job_code"),
			Note:               r.GetString("note"),
			RecordedAt:         r.GetDateTime("created").Time(),
		}))
	}

	return &RepublishResult{Published: len(recs), From: from, To: to}, nil
}
