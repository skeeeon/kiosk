package controller

import (
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/timeclock"
)

func seedPunchUser(t *testing.T, app core.App, code, name string) string {
	t.Helper()
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	u := core.NewRecord(users)
	u.Set("email", code+"@test.local")
	u.Set("name", name)
	u.Set("code", code)
	u.Set("role", "worker")
	u.Set("active", true)
	u.SetPassword(code + "-password-123")
	if err := app.Save(u); err != nil {
		t.Fatalf("save user: %v", err)
	}
	return u.Id
}

func punchPayload(punchID string, occurredAt time.Time) EventPayload {
	return EventPayload{
		PunchID:      punchID,
		KioskCode:    "KIOSK-A",
		LocationCode: "WEST",
		UserID:       "kiosk-local-user-id",
		UserCode:     "EMP-9",
		UserName:     "Pat",
		Direction:    "in",
		OccurredAt:   occurredAt,
		Source:       timeclock.SourceSelf,
	}
}

func TestProjectTimePunch_ProjectsAndDedupes(t *testing.T) {
	app := setupApp(t)
	agg := NewAggregator(app, nil, "")
	userID := seedPunchUser(t, app, "EMP-9", "Pat")

	p := punchPayload("punch-1", time.Date(2026, 6, 11, 7, 0, 0, 0, time.UTC))
	if out := agg.ProjectTimePunch(p); out != projectAck {
		t.Fatalf("ProjectTimePunch: got %v, want projectAck", out)
	}

	rec, err := app.FindFirstRecordByFilter("time_punches",
		"source_punch_id = {:id}", dbx.Params{"id": "punch-1"})
	if err != nil || rec == nil {
		t.Fatalf("expected a projected punch row: err=%v rec=%v", err, rec)
	}
	if got := rec.GetString("user"); got != userID {
		t.Errorf("user FK: got %q, want %q (resolved by code)", got, userID)
	}
	if got := rec.GetString("kiosk_code"); got != "KIOSK-A" {
		t.Errorf("kiosk_code: got %q", got)
	}
	if got := rec.GetString("direction"); got != "in" {
		t.Errorf("direction: got %q", got)
	}

	// Redelivery is a no-op.
	if out := agg.ProjectTimePunch(p); out != projectAck {
		t.Fatalf("redelivery: got %v, want projectAck", out)
	}
	rows, _ := app.FindRecordsByFilter("time_punches",
		"source_punch_id = {:id}", "", 10, 0, dbx.Params{"id": "punch-1"})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after redelivery, got %d", len(rows))
	}
}

func TestProjectTimePunch_UnknownUserAcks(t *testing.T) {
	app := setupApp(t)
	agg := NewAggregator(app, nil, "")

	p := punchPayload("punch-2", time.Now().UTC())
	p.UserCode = "GHOST"
	if out := agg.ProjectTimePunch(p); out != projectAck {
		t.Fatalf("unknown user must ack (retry won't help): got %v", out)
	}
	rows, _ := app.FindRecordsByFilter("time_punches", "", "", 10, 0)
	if len(rows) != 0 {
		t.Fatalf("no row should be written for an unknown user, got %d", len(rows))
	}
}

func TestProjectTimePunch_ActorColumns(t *testing.T) {
	app := setupApp(t)
	agg := NewAggregator(app, nil, "")
	seedPunchUser(t, app, "EMP-9", "Pat")
	foremanID := seedPunchUser(t, app, "EMP-10", "Frank")

	// Foreman recorder resolves to the org-wide user FK.
	p := punchPayload("punch-3", time.Now().UTC())
	p.Source = timeclock.SourceForeman
	p.RecordedByUserCode = "EMP-10"
	if out := agg.ProjectTimePunch(p); out != projectAck {
		t.Fatalf("foreman punch: %v", out)
	}
	rec, _ := app.FindFirstRecordByFilter("time_punches",
		"source_punch_id = {:id}", dbx.Params{"id": "punch-3"})
	if got := rec.GetString("recorded_by_user"); got != foremanID {
		t.Errorf("recorded_by_user: got %q, want %q", got, foremanID)
	}

	// Kiosk-local admin id lands in source_actor (its FK can't resolve here).
	p = punchPayload("punch-4", time.Now().UTC())
	p.Source = timeclock.SourceAdmin
	p.AdminID = "kiosk-admin-id"
	p.Reason = "correction"
	if out := agg.ProjectTimePunch(p); out != projectAck {
		t.Fatalf("admin punch: %v", out)
	}
	rec, _ = app.FindFirstRecordByFilter("time_punches",
		"source_punch_id = {:id}", dbx.Params{"id": "punch-4"})
	if got := rec.GetString("source_actor"); got != "kiosk-admin-id" {
		t.Errorf("source_actor: got %q", got)
	}
	if got := rec.GetString("reason"); got != "correction" {
		t.Errorf("reason: got %q", got)
	}
}

func TestShouldReplacePunchState_Monotonic(t *testing.T) {
	older := timeclock.PunchStatePayload{UserCode: "EMP-9", ClockedIn: true,
		OccurredAt: time.Date(2026, 6, 11, 7, 0, 0, 0, time.UTC)}
	newer := timeclock.PunchStatePayload{UserCode: "EMP-9", ClockedIn: false,
		OccurredAt: time.Date(2026, 6, 11, 15, 0, 0, 0, time.UTC)}

	if !shouldReplacePunchState(older, newer) {
		t.Fatal("newer punch must replace older state")
	}
	if shouldReplacePunchState(newer, older) {
		t.Fatal("older punch must not regress newer state")
	}
	if shouldReplacePunchState(newer, newer) {
		t.Fatal("equal timestamps must not replace (redelivery)")
	}
}
