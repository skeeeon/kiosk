package scheduler

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/timeclock"
)

// capturedSend records one fan-out send so the test can assert per-worker
// scoping and recipient shape without standing up SMTP.
type capturedSend struct {
	eventType  string
	data       any
	recipients notifications.Recipients
}

func seedSelfWorker(t *testing.T, app core.App, code, email string, active bool) *core.Record {
	t.Helper()
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	w := core.NewRecord(users)
	w.Set("code", code)
	w.Set("name", "Worker "+code)
	w.Set("role", "worker")
	w.Set("active", active)
	w.Set("email", email)
	w.SetPassword("temp-password-123")
	if err := app.Save(w); err != nil {
		t.Fatalf("save worker %s: %v", code, err)
	}
	return w
}

func seedSelfPunch(t *testing.T, app core.App, w *core.Record, direction string, at time.Time) {
	t.Helper()
	coll, err := app.FindCollectionByNameOrId(timeclock.Collection)
	if err != nil {
		t.Fatalf("find punches collection: %v", err)
	}
	p := core.NewRecord(coll)
	p.Set("user", w.Id)
	p.Set("user_code", w.GetString("code"))
	p.Set("direction", direction)
	p.Set("occurred_at", at)
	p.Set("source", timeclock.SourceSelf)
	p.Set("kiosk_code", "BAY-01")
	if err := app.Save(p); err != nil {
		t.Fatalf("save punch: %v", err)
	}
}

func selfScheduleRow(t *testing.T, app core.App, cadence string) *core.Record {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("scheduled_reports")
	if err != nil {
		t.Fatalf("find scheduled_reports: %v", err)
	}
	r := core.NewRecord(col)
	r.Set("report_key", "timeclock_self")
	r.Set("cadence", cadence)
	r.Set("hour", 6)
	return r
}

// TestRunTimeclockSelfDigest_PerWorkerFanOut is the core of the fan-out: one
// private email per ACTIVE worker with punches in the window, scoped to that
// worker, delivered via worker_email — and nobody else.
func TestRunTimeclockSelfDigest_PerWorkerFanOut(t *testing.T) {
	app := setupApp(t)
	kioskctx.Set(kioskctx.Identity{KioskCode: "BAY-01", LocationCode: "WH-A"})
	t.Cleanup(func() { kioskctx.Set(kioskctx.Identity{}) })

	alice := seedSelfWorker(t, app, "EMP-1", "alice@example.com", true)
	carol := seedSelfWorker(t, app, "EMP-3", "carol@example.com", false) // inactive
	_ = seedSelfWorker(t, app, "EMP-2", "bob@example.com", true)         // active, no punches

	// Anchor intervals to local midnight so the totals are deterministic and
	// fall inside the daily window's date filter regardless of host timezone.
	now := time.Now().In(time.Local)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	// alice: a closed 2h interval today.
	seedSelfPunch(t, app, alice, timeclock.DirectionIn, midnight.Add(9*time.Hour))
	seedSelfPunch(t, app, alice, timeclock.DirectionOut, midnight.Add(11*time.Hour))
	// carol (inactive): also has a pair — she's paired into day-totals but must
	// be filtered out at send time because she's deactivated.
	seedSelfPunch(t, app, carol, timeclock.DirectionIn, midnight.Add(8*time.Hour))
	seedSelfPunch(t, app, carol, timeclock.DirectionOut, midnight.Add(10*time.Hour))

	var sends []capturedSend
	send := func(eventType string, data any, r notifications.Recipients) error {
		sends = append(sends, capturedSend{eventType, data, r})
		return nil
	}

	if err := runTimeclockSelfDigest(app, selfScheduleRow(t, app, "daily"), send); err != nil {
		t.Fatalf("runTimeclockSelfDigest: %v", err)
	}

	// Exactly one send: active alice. Inactive carol and punchless bob excluded.
	if len(sends) != 1 {
		t.Fatalf("sends = %d; want 1 (active worker with punches only)", len(sends))
	}
	s := sends[0]
	if s.eventType != notifications.EventTypeTimeclockSelfDigest {
		t.Errorf("eventType = %q; want %q", s.eventType, notifications.EventTypeTimeclockSelfDigest)
	}
	if !s.recipients.WorkerEmail || s.recipients.AllAdmins || len(s.recipients.Extras) != 0 {
		t.Errorf("recipients = %+v; want worker_email only", s.recipients)
	}
	ctx, ok := s.data.(notifications.TimeclockSelfDigestContext)
	if !ok {
		t.Fatalf("data type = %T; want TimeclockSelfDigestContext", s.data)
	}
	if ctx.Worker.Code != "EMP-1" || ctx.Worker.Email != "alice@example.com" {
		t.Errorf("worker = %+v; want EMP-1 / alice@example.com", ctx.Worker)
	}
	if ctx.Total != "2h00m" {
		t.Errorf("total = %q; want 2h00m", ctx.Total)
	}
	if ctx.RowsCount == 0 {
		t.Errorf("rows = %d; want >= 1", ctx.RowsCount)
	}
}

// TestRunTimeclockSelfDigest_NoPunches: an empty window sends nothing and is
// not an error (the schedule simply had no one to summarize).
func TestRunTimeclockSelfDigest_NoPunches(t *testing.T) {
	app := setupApp(t)
	kioskctx.Set(kioskctx.Identity{KioskCode: "BAY-01", LocationCode: "WH-A"})
	t.Cleanup(func() { kioskctx.Set(kioskctx.Identity{}) })

	_ = seedSelfWorker(t, app, "EMP-1", "alice@example.com", true)

	n := 0
	send := func(string, any, notifications.Recipients) error { n++; return nil }
	if err := runTimeclockSelfDigest(app, selfScheduleRow(t, app, "daily"), send); err != nil {
		t.Fatalf("runTimeclockSelfDigest: %v", err)
	}
	if n != 0 {
		t.Errorf("sends = %d; want 0", n)
	}
}
