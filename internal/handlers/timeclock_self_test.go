package handlers_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/handlers"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/timeclock"

	// Apply the virtual-terminal-only worker-auth migration in this package's
	// throwaway DBs so TestWorkerAuthMigration can assert it ran. Benign for
	// the other handler tests (they set re.Auth directly and never mint a
	// token, so the users AuthRule is irrelevant to them).
	_ "github.com/skeeeon/kiosk/migrations/timeclock"
)

// newSelfTCHandlers builds a Handlers with timeclock enabled — the minimum the
// /api/self/timeclock/* endpoints need. The cart store and notifier are unused
// here but required by the constructor.
func newSelfTCHandlers(app core.App) *handlers.Handlers {
	cfg := &config.Config{}
	cfg.Timeclock.Enabled = true
	return handlers.New(app, cfg, cart.NewStore(time.Minute), notifications.New(app))
}

func seedWorker(t *testing.T, app core.App, code string, active bool) *core.Record {
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
	w.Set("email", code+"@example.com")
	w.SetPassword("temp-password-123")
	if err := app.Save(w); err != nil {
		t.Fatalf("save worker %s: %v", code, err)
	}
	return w
}

func selfReq(app core.App, method, url, body string, auth *core.Record) (*core.RequestEvent, *httptest.ResponseRecorder) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, url, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e := new(core.RequestEvent)
	e.App = app
	e.Request = req
	e.Response = rec
	if auth != nil {
		e.Auth = auth
	}
	return e, rec
}

func wantAPIStatus(t *testing.T, err error, status int) {
	t.Helper()
	var apiErr *router.ApiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *router.ApiError, got %v", err)
	}
	if apiErr.Status != status {
		t.Fatalf("status: got %d, want %d", apiErr.Status, status)
	}
}

// seedPunch writes a raw time_punches row at a known occurred_at, bypassing
// the funnel (Go Save ignores the API-readonly rule) so a test can compose a
// closed interval with a deterministic duration.
func seedPunch(t *testing.T, app core.App, w *core.Record, direction string, at time.Time) {
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
	p.Set("kiosk_code", "TC-TEST")
	if err := app.Save(p); err != nil {
		t.Fatalf("save punch: %v", err)
	}
}

func latestPunch(t *testing.T, app core.App) *core.Record {
	t.Helper()
	rows, err := app.FindRecordsByFilter(timeclock.Collection, "", "-created", 1, 0, dbx.Params{})
	if err != nil {
		t.Fatalf("find punches: %v", err)
	}
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

func TestSelfTimeclockPunch_RequiresWorkerAuth(t *testing.T) {
	app := setupApp(t)
	kioskctx.Set(kioskctx.Identity{KioskCode: "TC-TEST", LocationCode: "WEB"})
	h := newSelfTCHandlers(app)

	e, _ := selfReq(app, http.MethodPost, "/api/self/timeclock/punch", `{"direction":"in"}`, nil)
	wantAPIStatus(t, h.SelfTimeclockPunch(e), http.StatusUnauthorized)
}

func TestSelfTimeclockPunch_RejectsNonWorkerToken(t *testing.T) {
	app := setupApp(t)
	kioskctx.Set(kioskctx.Identity{KioskCode: "TC-TEST", LocationCode: "WEB"})
	h := newSelfTCHandlers(app)

	// The bootstrap admin is an `admins` record — wrong collection.
	admin, err := app.FindFirstRecordByFilter("admins", "email = {:e}", dbx.Params{"e": "admin@kiosk.local"})
	if err != nil {
		t.Fatalf("find bootstrap admin: %v", err)
	}
	e, _ := selfReq(app, http.MethodPost, "/api/self/timeclock/punch", `{"direction":"in"}`, admin)
	wantAPIStatus(t, h.SelfTimeclockPunch(e), http.StatusForbidden)
}

func TestSelfTimeclockPunch_RejectsInactiveWorker(t *testing.T) {
	app := setupApp(t)
	kioskctx.Set(kioskctx.Identity{KioskCode: "TC-TEST", LocationCode: "WEB"})
	h := newSelfTCHandlers(app)

	w := seedWorker(t, app, "W-INACTIVE", false)
	e, _ := selfReq(app, http.MethodPost, "/api/self/timeclock/punch", `{"direction":"in"}`, w)
	wantAPIStatus(t, h.SelfTimeclockPunch(e), http.StatusForbidden)
}

// TestSelfTimeclockPunch_IdentityFromSession is the trust invariant: the punch
// lands on the AUTHENTICATED worker, never on a user_code smuggled in the body.
func TestSelfTimeclockPunch_IdentityFromSession(t *testing.T) {
	app := setupApp(t)
	kioskctx.Set(kioskctx.Identity{KioskCode: "TC-TEST", LocationCode: "WEB"})
	h := newSelfTCHandlers(app)

	alice := seedWorker(t, app, "W-ALICE", true)
	_ = seedWorker(t, app, "W-BOB", true)

	// Authenticate as Alice but try to smuggle Bob's code in the body. The
	// handler's body type has no user_code field, so the spoof is ignored by
	// construction — the punch must target Alice.
	e, rec := selfReq(app, http.MethodPost, "/api/self/timeclock/punch",
		`{"direction":"in","user_code":"W-BOB","target_user_code":"W-BOB"}`, alice)
	if err := h.SelfTimeclockPunch(e); err != nil {
		t.Fatalf("punch: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	p := latestPunch(t, app)
	if p == nil {
		t.Fatal("no punch row written")
	}
	if got := p.GetString("user_code"); got != "W-ALICE" {
		t.Errorf("punch targeted %q, want the authenticated worker W-ALICE", got)
	}
	if got := p.GetString("user"); got != alice.Id {
		t.Errorf("punch user FK %q, want Alice %q", got, alice.Id)
	}
	if got := p.GetString("source"); got != timeclock.SourceSelf {
		t.Errorf("source %q, want %q", got, timeclock.SourceSelf)
	}
	if got := p.GetString("kiosk_code"); got != "TC-TEST" {
		t.Errorf("kiosk_code %q, want the virtual terminal's identity", got)
	}
}

func TestSelfTimeclockStatus_ReturnsOwnState(t *testing.T) {
	app := setupApp(t)
	kioskctx.Set(kioskctx.Identity{KioskCode: "TC-TEST", LocationCode: "WEB"})
	h := newSelfTCHandlers(app)

	alice := seedWorker(t, app, "W-ALICE", true)

	// Anonymous status is rejected.
	e0, _ := selfReq(app, http.MethodGet, "/api/self/timeclock/status", "", nil)
	wantAPIStatus(t, h.SelfTimeclockStatus(e0), http.StatusUnauthorized)

	// Authed: not clocked in yet.
	e1, rec1 := selfReq(app, http.MethodGet, "/api/self/timeclock/status", "", alice)
	if err := h.SelfTimeclockStatus(e1); err != nil {
		t.Fatalf("status: %v", err)
	}
	if rec1.Code != http.StatusOK {
		t.Fatalf("status code: got %d, want 200", rec1.Code)
	}
	if !strings.Contains(rec1.Body.String(), `"user_code":"W-ALICE"`) {
		t.Errorf("status body does not report the authed worker: %s", rec1.Body.String())
	}
	if !strings.Contains(rec1.Body.String(), `"clocked_in":false`) {
		t.Errorf("expected not clocked in, got: %s", rec1.Body.String())
	}
}

// TestTimeclockStatus_TodaySeconds verifies the kiosk status endpoint reports
// the worker's CLOSED interval time for the local day. The seeded interval is
// anchored to local midnight, so the assertion holds regardless of when the
// test runs or the host timezone — covering the UTC-window-vs-local-day
// widening the handler relies on.
func TestTimeclockStatus_TodaySeconds(t *testing.T) {
	app := setupApp(t)
	kioskctx.Set(kioskctx.Identity{KioskCode: "TC-TEST", LocationCode: "WEB"})
	h := newSelfTCHandlers(app)

	w := seedWorker(t, app, "W-CLOCK", true)

	// A closed 2h interval (09:00→11:00 local) today, then clocked out.
	now := time.Now().In(time.Local)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	seedPunch(t, app, w, timeclock.DirectionIn, midnight.Add(9*time.Hour))
	seedPunch(t, app, w, timeclock.DirectionOut, midnight.Add(11*time.Hour))

	e, rec := selfReq(app, http.MethodGet, "/api/kiosk/timeclock/status?user_code=W-CLOCK", "", nil)
	if err := h.TimeclockStatus(e); err != nil {
		t.Fatalf("status: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status code: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"today_seconds":7200`) {
		t.Errorf("expected today_seconds 7200 (2h closed), got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"clocked_in":false`) {
		t.Errorf("expected clocked out after the paired interval, got: %s", rec.Body.String())
	}
}

// TestWorkerAuthMigration verifies the cmd/timeclock-only migration turns the
// users collection into a real worker-auth surface.
func TestWorkerAuthMigration(t *testing.T) {
	app := setupApp(t)
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	if users.AuthRule == nil || *users.AuthRule != "active = true" {
		got := "<nil>"
		if users.AuthRule != nil {
			got = *users.AuthRule
		}
		t.Errorf("users.AuthRule: got %q, want %q", got, "active = true")
	}
	if !users.OAuth2.Enabled {
		t.Error("users.OAuth2.Enabled: got false, want true")
	}
}
