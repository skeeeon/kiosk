package timeclock_test

import (
	"errors"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/timeclock"

	// Register kiosk migrations via init() so the runner can apply them below.
	_ "github.com/skeeeon/kiosk/migrations"
)

var testIdentity = kioskctx.Identity{KioskCode: "TEST", LocationCode: "T"}

// setupApp boots a fresh PB app in a temp dir with migrations applied —
// same pattern as internal/commit/commit_test.go.
func setupApp(t *testing.T) *pocketbase.PocketBase {
	t.Helper()
	t.Setenv("KIOSK_QUIET_BOOTSTRAP", "1")

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  t.TempDir(),
		HideStartBanner: true,
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	runner := core.NewMigrationsRunner(app, core.AppMigrations)
	if _, err := runner.Up(); err != nil {
		t.Fatalf("migrations up: %v", err)
	}
	t.Cleanup(func() { _ = app.ResetBootstrapState() })
	return app
}

type seed struct {
	ForemanID    string // Alice — foreman in "electrical"
	WorkerID     string // Bob — worker in "electrical"
	OutsiderID   string // Carol — worker in "plumbing"
	InactiveID   string // Dave — inactive worker in "electrical"
	ItemID       string
	AdminID      string // bootstrap admin record id
	ElectricalID string
}

func seedFixtures(t *testing.T, app core.App) seed {
	t.Helper()
	groups, err := app.FindCollectionByNameOrId("groups")
	if err != nil {
		t.Fatalf("find groups: %v", err)
	}
	mkGroup := func(code string) string {
		g := core.NewRecord(groups)
		g.Set("code", code)
		g.Set("name", code)
		g.Set("active", true)
		if err := app.Save(g); err != nil {
			t.Fatalf("save group %s: %v", code, err)
		}
		return g.Id
	}
	electrical := mkGroup("electrical")
	plumbing := mkGroup("plumbing")

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	mkUser := func(code, name, role, group string, active bool) string {
		u := core.NewRecord(users)
		u.Set("email", code+"@test.local")
		u.Set("name", name)
		u.Set("code", code)
		u.Set("role", role)
		u.Set("group", group)
		u.Set("active", active)
		u.SetPassword(code + "-password-123")
		if err := app.Save(u); err != nil {
			t.Fatalf("save user %s: %v", code, err)
		}
		return u.Id
	}
	alice := mkUser("EMP-1", "Alice", "foreman", electrical, true)
	bob := mkUser("EMP-2", "Bob", "worker", electrical, true)
	carol := mkUser("EMP-3", "Carol", "worker", plumbing, true)
	dave := mkUser("EMP-4", "Dave", "worker", electrical, false)

	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	item := core.NewRecord(items)
	item.Set("code", "HAMMER")
	item.Set("name", "Hammer")
	item.Set("type", "tool")
	item.Set("tracking_mode", "quantity")
	item.Set("active", true)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}

	admin, err := app.FindFirstRecordByFilter("admins", "")
	if err != nil {
		t.Fatalf("find bootstrap admin: %v", err)
	}

	return seed{
		ForemanID:    alice,
		WorkerID:     bob,
		OutsiderID:   carol,
		InactiveID:   dave,
		ItemID:       item.Id,
		AdminID:      admin.Id,
		ElectricalID: electrical,
	}
}

// seedOpenCheckout writes the minimal transaction → line → open_checkouts
// chain so the clock-out block has a row to count.
func seedOpenCheckout(t *testing.T, app core.App, userID, itemID string) {
	t.Helper()
	txCol, _ := app.FindCollectionByNameOrId("transactions")
	tx := core.NewRecord(txCol)
	tx.Set("kiosk_code", "TEST")
	tx.Set("location_code", "T")
	tx.Set("user", userID)
	tx.Set("status", "completed")
	tx.Set("completed_at", time.Now().UTC())
	if err := app.Save(tx); err != nil {
		t.Fatalf("save tx: %v", err)
	}
	linesCol, _ := app.FindCollectionByNameOrId("transaction_lines")
	line := core.NewRecord(linesCol)
	line.Set("transaction", tx.Id)
	line.Set("item", itemID)
	line.Set("action", "checkout")
	line.Set("qty", 1)
	if err := app.Save(line); err != nil {
		t.Fatalf("save line: %v", err)
	}
	ocCol, _ := app.FindCollectionByNameOrId("open_checkouts")
	oc := core.NewRecord(ocCol)
	oc.Set("item", itemID)
	oc.Set("user", userID)
	oc.Set("checked_out_at", time.Now().UTC())
	oc.Set("transaction_line", line.Id)
	if err := app.Save(oc); err != nil {
		t.Fatalf("save open_checkout: %v", err)
	}
}

func selfPunch(code, direction string) timeclock.PunchInput {
	return timeclock.PunchInput{
		TargetUserCode: code,
		Direction:      direction,
		Source:         timeclock.SourceSelf,
	}
}

func countPunches(t *testing.T, app core.App, userID string) int {
	t.Helper()
	rows, err := app.FindRecordsByFilter(timeclock.Collection,
		"user = {:u}", "", 0, 0, dbx.Params{"u": userID})
	if err != nil {
		t.Fatalf("count punches: %v", err)
	}
	return len(rows)
}

func TestPerformPunch_AlternationLive(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	res, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, selfPunch("EMP-2", "in"))
	if err != nil {
		t.Fatalf("clock in: %v", err)
	}
	if !res.ClockedIn || res.Direction != "in" {
		t.Fatalf("clock in result: %+v", res)
	}

	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, selfPunch("EMP-2", "in")); !errors.Is(err, timeclock.ErrAlreadyClockedIn) {
		t.Fatalf("double clock-in: got %v, want ErrAlreadyClockedIn", err)
	}

	res, err = timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, selfPunch("EMP-2", "out"))
	if err != nil {
		t.Fatalf("clock out: %v", err)
	}
	if res.ClockedIn {
		t.Fatalf("expected clocked out, got %+v", res)
	}

	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, selfPunch("EMP-2", "out")); !errors.Is(err, timeclock.ErrNotClockedIn) {
		t.Fatalf("double clock-out: got %v, want ErrNotClockedIn", err)
	}
	if n := countPunches(t, app, s.WorkerID); n != 2 {
		t.Fatalf("expected 2 punch rows, got %d", n)
	}
}

func TestPerformPunch_LiveRules(t *testing.T) {
	app := setupApp(t)
	seedFixtures(t, app)

	// Unknown user.
	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, selfPunch("NOPE", "in")); !errors.Is(err, timeclock.ErrUserNotFound) {
		t.Fatalf("unknown user: got %v, want ErrUserNotFound", err)
	}
	// Inactive worker can't live-punch.
	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, selfPunch("EMP-4", "in")); !errors.Is(err, timeclock.ErrUserInactive) {
		t.Fatalf("inactive: got %v, want ErrUserInactive", err)
	}
	// Live punches can't backdate or force.
	in := selfPunch("EMP-2", "in")
	in.OccurredAt = time.Now().Add(-time.Hour)
	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, in); err == nil {
		t.Fatal("backdated self punch should fail")
	}
	in = selfPunch("EMP-2", "out")
	in.Force = true
	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, in); err == nil {
		t.Fatal("forced self punch should fail")
	}
}

func TestPerformPunch_AdminBackdateAndRules(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	admin := func(code, direction, reason string, occurredAt time.Time) timeclock.PunchInput {
		return timeclock.PunchInput{
			TargetUserCode: code,
			Direction:      direction,
			Source:         timeclock.SourceAdmin,
			ActorAdminID:   s.AdminID,
			Reason:         reason,
			OccurredAt:     occurredAt,
		}
	}

	// Reason required.
	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, admin("EMP-2", "in", "", time.Time{})); err == nil {
		t.Fatal("admin punch without reason should fail")
	}
	// Future occurred_at rejected.
	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, admin("EMP-2", "in", "fix", time.Now().Add(time.Hour))); err == nil {
		t.Fatal("future punch should fail")
	}
	// Backdated corrective punch bypasses alternation (out with no prior in).
	res, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, admin("EMP-2", "out", "forgot to clock out", time.Now().Add(-2*time.Hour)))
	if err != nil {
		t.Fatalf("backdated admin out: %v", err)
	}
	if res.ClockedIn {
		t.Fatalf("merged state after backdated out should be clocked out: %+v", res)
	}
	// Admin may punch an inactive worker (correcting the record).
	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, admin("EMP-4", "in", "missed punch", time.Now().Add(-3*time.Hour))); err != nil {
		t.Fatalf("admin punch for inactive worker: %v", err)
	}

	// A backdated correction OLDER than the latest punch must not flip the
	// merged current state.
	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, selfPunch("EMP-2", "in")); err != nil {
		t.Fatalf("live clock in: %v", err)
	}
	res, err = timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, admin("EMP-2", "out", "yesterday's missed out", time.Now().Add(-90*time.Minute)))
	if err != nil {
		t.Fatalf("older corrective punch: %v", err)
	}
	if !res.ClockedIn {
		t.Fatalf("current state should still be clocked in (live punch is newest): %+v", res)
	}
}

func TestPerformPunch_ForemanGate(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	foreman := func(actorID, target string) timeclock.PunchInput {
		return timeclock.PunchInput{
			TargetUserCode: target,
			Direction:      "in",
			Source:         timeclock.SourceForeman,
			ActorUserID:    actorID,
		}
	}

	// Worker acting as foreman → rejected.
	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, foreman(s.WorkerID, "EMP-1")); !errors.Is(err, timeclock.ErrForemanGate) {
		t.Fatalf("worker-as-foreman: got %v, want ErrForemanGate", err)
	}
	// Foreman punching outside their group → rejected.
	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, foreman(s.ForemanID, "EMP-3")); !errors.Is(err, timeclock.ErrForemanGate) {
		t.Fatalf("cross-group: got %v, want ErrForemanGate", err)
	}
	// Foreman punching a crew member → ok, recorder stamped.
	res, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, foreman(s.ForemanID, "EMP-2"))
	if err != nil {
		t.Fatalf("foreman punch: %v", err)
	}
	rec, err := app.FindRecordById(timeclock.Collection, res.PunchID)
	if err != nil {
		t.Fatalf("load punch: %v", err)
	}
	if got := rec.GetString("recorded_by_user"); got != s.ForemanID {
		t.Fatalf("recorded_by_user: got %q, want %q", got, s.ForemanID)
	}
	if got := rec.GetString("source"); got != timeclock.SourceForeman {
		t.Fatalf("source: got %q, want foreman", got)
	}
	// Foremen can't backdate.
	in := foreman(s.ForemanID, "EMP-2")
	in.Direction = "out"
	in.OccurredAt = time.Now().Add(-time.Hour)
	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, in); err == nil {
		t.Fatal("backdated foreman punch should fail")
	}
}

func TestPerformPunch_OpenCheckoutsBlockAndForce(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	rules := timeclock.Rules{BlockClockOutWithOpenCheckouts: true}

	if _, err := timeclock.PerformPunch(app, nil, nil, rules, testIdentity, selfPunch("EMP-2", "in")); err != nil {
		t.Fatalf("clock in: %v", err)
	}
	seedOpenCheckout(t, app, s.WorkerID, s.ItemID)

	// Self clock-out blocked, count carried.
	_, err := timeclock.PerformPunch(app, nil, nil, rules, testIdentity, selfPunch("EMP-2", "out"))
	if !errors.Is(err, timeclock.ErrOpenCheckouts) {
		t.Fatalf("blocked clock-out: got %v, want ErrOpenCheckouts", err)
	}
	var oc *timeclock.OpenCheckoutsError
	if !errors.As(err, &oc) || oc.Count != 1 {
		t.Fatalf("expected count 1, got %+v", oc)
	}

	// Admin without force is blocked too.
	adminOut := timeclock.PunchInput{
		TargetUserCode: "EMP-2",
		Direction:      "out",
		Source:         timeclock.SourceAdmin,
		ActorAdminID:   s.AdminID,
		Reason:         "left site",
	}
	if _, err := timeclock.PerformPunch(app, nil, nil, rules, testIdentity, adminOut); !errors.Is(err, timeclock.ErrOpenCheckouts) {
		t.Fatalf("admin non-force: got %v, want ErrOpenCheckouts", err)
	}

	// Admin force is the escape hatch.
	adminOut.Force = true
	res, err := timeclock.PerformPunch(app, nil, nil, rules, testIdentity, adminOut)
	if err != nil {
		t.Fatalf("forced clock-out: %v", err)
	}
	if res.ClockedIn {
		t.Fatalf("expected clocked out after force: %+v", res)
	}
	rec, _ := app.FindRecordById(timeclock.Collection, res.PunchID)
	if !rec.GetBool("force") {
		t.Fatal("force flag not persisted")
	}
}

// A worker with NO local open checkouts but tools out at ANOTHER kiosk (per the
// fleet replica) is blocked here, and a self-acknowledgment (Force) clears it.
func TestPerformPunch_FleetOpenCheckoutsBlockAndAcknowledge(t *testing.T) {
	app := setupApp(t)
	seedFixtures(t, app)
	rules := timeclock.Rules{BlockClockOutWithOpenCheckouts: true}

	if _, err := timeclock.PerformPunch(app, nil, nil, rules, testIdentity, selfPunch("EMP-2", "in")); err != nil {
		t.Fatalf("clock in: %v", err)
	}

	fleet := timeclock.NewCheckoutFleet()
	fleet.Upsert(timeclock.OpenCheckoutsStatePayload{
		UserCode: "EMP-2",
		Rows:     []timeclock.OpenCheckoutRow{{ItemCode: "DRILL", ItemName: "Drill", KioskCode: "KIOSK-B"}},
	})

	_, err := timeclock.PerformPunch(app, nil, fleet, rules, testIdentity, selfPunch("EMP-2", "out"))
	var oc *timeclock.OpenCheckoutsError
	if !errors.As(err, &oc) || oc.Count != 1 {
		t.Fatalf("fleet row at another kiosk must block: got %v", err)
	}

	// Self "clock out anyway" — Force on a self punch acknowledges the open
	// tools, clears the block, and is recorded as source=self force=true.
	ack := selfPunch("EMP-2", "out")
	ack.Force = true
	res, err := timeclock.PerformPunch(app, nil, fleet, rules, testIdentity, ack)
	if err != nil {
		t.Fatalf("acknowledged clock-out: %v", err)
	}
	if res.ClockedIn {
		t.Fatalf("expected clocked out after acknowledge: %+v", res)
	}
	rec, _ := app.FindRecordById(timeclock.Collection, res.PunchID)
	if !rec.GetBool("force") || rec.GetString("source") != timeclock.SourceSelf {
		t.Fatalf("acknowledgment should persist as source=self force=true, got source=%q force=%v",
			rec.GetString("source"), rec.GetBool("force"))
	}
}

// Replica rows tagged with THIS kiosk's own code must not block: the local
// open_checkouts table is authoritative for this kiosk, so a self-tagged
// replica row (an echo of a local checkout) would otherwise double-count.
func TestPerformPunch_FleetSelfKioskRowsDoNotDoubleBlock(t *testing.T) {
	app := setupApp(t)
	seedFixtures(t, app)
	rules := timeclock.Rules{BlockClockOutWithOpenCheckouts: true}

	if _, err := timeclock.PerformPunch(app, nil, nil, rules, testIdentity, selfPunch("EMP-2", "in")); err != nil {
		t.Fatalf("clock in: %v", err)
	}

	fleet := timeclock.NewCheckoutFleet()
	fleet.Upsert(timeclock.OpenCheckoutsStatePayload{
		UserCode: "EMP-2",
		Rows:     []timeclock.OpenCheckoutRow{{ItemCode: "DRILL", KioskCode: testIdentity.KioskCode}},
	})

	if _, err := timeclock.PerformPunch(app, nil, fleet, rules, testIdentity, selfPunch("EMP-2", "out")); err != nil {
		t.Fatalf("self-tagged replica row must not block: %v", err)
	}
}

func TestPerformPunch_CommandIDIdempotency(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	in := timeclock.PunchInput{
		TargetUserCode:    "EMP-2",
		Direction:         "in",
		Source:            timeclock.SourceControllerAdmin,
		ControllerAdminID: "ctrl-admin-1",
		Reason:            "remote punch",
		CommandID:         "cmd-123",
	}
	first, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, in)
	if err != nil {
		t.Fatalf("first punch: %v", err)
	}
	if first.Replayed {
		t.Fatal("first punch must not be a replay")
	}
	second, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, in)
	if err != nil {
		t.Fatalf("replayed punch: %v", err)
	}
	if !second.Replayed || second.PunchID != first.PunchID {
		t.Fatalf("expected replay of %s, got %+v", first.PunchID, second)
	}
	if n := countPunches(t, app, s.WorkerID); n != 1 {
		t.Fatalf("expected 1 punch row, got %d", n)
	}
}

func TestCurrentState_FleetMergeRule(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Local: clocked out an hour ago.
	adminOut := timeclock.PunchInput{
		TargetUserCode: "EMP-2",
		Direction:      "out",
		Source:         timeclock.SourceAdmin,
		ActorAdminID:   s.AdminID,
		Reason:         "seed",
		OccurredAt:     time.Now().Add(-time.Hour),
	}
	if _, err := timeclock.PerformPunch(app, nil, nil, timeclock.Rules{}, testIdentity, adminOut); err != nil {
		t.Fatalf("seed punch: %v", err)
	}

	// Fleet replica: clocked in at another kiosk five minutes ago — fresher.
	fleet := timeclock.NewFleet()
	fleet.Upsert(timeclock.PunchStatePayload{
		UserCode:   "EMP-2",
		ClockedIn:  true,
		OccurredAt: time.Now().Add(-5 * time.Minute),
	})

	state, err := timeclock.CurrentState(app, fleet, s.WorkerID, "EMP-2")
	if err != nil {
		t.Fatalf("current state: %v", err)
	}
	if !state.ClockedIn || state.Origin != "fleet" {
		t.Fatalf("expected fleet-clocked-in, got %+v", state)
	}

	// With the fleet saying "in", a live clock-out HERE passes alternation.
	res, err := timeclock.PerformPunch(app, fleet, nil, timeclock.Rules{}, testIdentity, selfPunch("EMP-2", "out"))
	if err != nil {
		t.Fatalf("cross-kiosk clock-out: %v", err)
	}
	if res.ClockedIn {
		t.Fatalf("expected clocked out: %+v", res)
	}

	// Monotonic Upsert: an older fleet entry can't regress state.
	if applied := fleet.Upsert(timeclock.PunchStatePayload{
		UserCode:   "EMP-2",
		ClockedIn:  true,
		OccurredAt: time.Now().Add(-30 * time.Minute),
	}); applied {
		t.Fatal("older fleet entry must not apply")
	}
}

// A punch made at THIS kiosk echoes back through controller → punch_state →
// fleet replica. Even if the echo's timestamp reads fractionally fresher than
// the local row (pre-truncation deployments published ns while the DB stored
// ms), the same punch must never be reported as "fleet" — the SPA renders
// that as "clocked in at another kiosk".
func TestCurrentState_OwnPunchEchoStaysLocal(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	fleet := timeclock.NewFleet()

	res, err := timeclock.PerformPunch(app, fleet, nil, timeclock.Rules{}, testIdentity, selfPunch("EMP-2", "in"))
	if err != nil {
		t.Fatalf("clock in: %v", err)
	}

	fleet.Upsert(timeclock.PunchStatePayload{
		UserCode:      "EMP-2",
		ClockedIn:     true,
		OccurredAt:    res.OccurredAt.Add(time.Microsecond),
		SourcePunchID: res.PunchID,
	})

	state, err := timeclock.CurrentState(app, fleet, s.WorkerID, "EMP-2")
	if err != nil {
		t.Fatalf("current state: %v", err)
	}
	if state.Origin != "local" {
		t.Fatalf("own punch echo must stay local, got %+v", state)
	}
	if !state.ClockedIn {
		t.Fatalf("expected clocked in, got %+v", state)
	}

	// A genuinely different punch (another kiosk's id) still wins when fresher.
	fleet.Upsert(timeclock.PunchStatePayload{
		UserCode:      "EMP-2",
		ClockedIn:     false,
		OccurredAt:    res.OccurredAt.Add(time.Minute),
		SourcePunchID: "punch-from-kiosk-b",
	})
	state, err = timeclock.CurrentState(app, fleet, s.WorkerID, "EMP-2")
	if err != nil {
		t.Fatalf("current state: %v", err)
	}
	if state.Origin != "fleet" || state.ClockedIn {
		t.Fatalf("fresher foreign punch must win as fleet, got %+v", state)
	}
}
