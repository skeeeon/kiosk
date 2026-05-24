package scheduler

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/notifications"

	// Register kiosk migrations via init() so the runner can apply them.
	_ "github.com/skeeeon/kiosk/migrations"
)

// setupApp boots a fresh PB app with migrations applied. Mirrors the
// pattern in internal/commit/commit_test.go.
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

// scheduleRow builds an in-memory scheduled_reports record carrying the
// given cadence. Not persisted — runDailyActivityDigest only reads
// row.GetString("cadence").
func scheduleRow(t *testing.T, app core.App, cadence string) *core.Record {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("scheduled_reports")
	if err != nil {
		t.Fatalf("find scheduled_reports: %v", err)
	}
	r := core.NewRecord(col)
	r.Set("report_key", "daily_activity")
	r.Set("cadence", cadence)
	r.Set("hour", 6)
	return r
}

func TestDailyActivityWindowFor(t *testing.T) {
	cases := []struct {
		cadence string
		want    time.Duration
		ok      bool
	}{
		{"daily", 24 * time.Hour, true},
		{"weekly", 7 * 24 * time.Hour, true},
		{"monthly", 30 * 24 * time.Hour, true},
		{"hourly", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, err := dailyActivityWindowFor(c.cadence)
		if c.ok && err != nil {
			t.Errorf("%q: unexpected error: %v", c.cadence, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%q: expected error, got nil", c.cadence)
		}
		if got != c.want {
			t.Errorf("%q: window = %v; want %v", c.cadence, got, c.want)
		}
	}
}

func TestRunDailyActivityDigest_EmptyWindow(t *testing.T) {
	app := setupApp(t)
	kioskctx.Set(kioskctx.Identity{KioskCode: "BAY-01", LocationCode: "WH-A"})
	t.Cleanup(func() { kioskctx.Set(kioskctx.Identity{}) })

	row := scheduleRow(t, app, "daily")
	eventType, data, err := runDailyActivityDigest(app, row)
	if err != nil {
		t.Fatalf("runDailyActivityDigest: %v", err)
	}
	if eventType != notifications.EventTypeDailyActivity {
		t.Errorf("eventType = %q; want %q", eventType, notifications.EventTypeDailyActivity)
	}
	ctx, ok := data.(notifications.DailyActivityContext)
	if !ok {
		t.Fatalf("data type = %T; want DailyActivityContext", data)
	}
	if ctx.Kiosk.Code != "BAY-01" {
		t.Errorf("kiosk code = %q; want BAY-01", ctx.Kiosk.Code)
	}
	if ctx.Cadence != "daily" {
		t.Errorf("cadence = %q; want daily", ctx.Cadence)
	}
	if ctx.TransactionCount != 0 || ctx.LinesCount != 0 {
		t.Errorf("empty window should report zero counts; got %+v", ctx)
	}
	if got := ctx.WindowEnd.Sub(ctx.WindowStart); got != 24*time.Hour {
		t.Errorf("window span = %v; want 24h", got)
	}
}

func TestRunDailyActivityDigest_AggregatesAndRanks(t *testing.T) {
	app := setupApp(t)
	kioskctx.Set(kioskctx.Identity{KioskCode: "BAY-01", LocationCode: "WH-A"})
	t.Cleanup(func() { kioskctx.Set(kioskctx.Identity{}) })

	fix := seedActivityFixtures(t, app)
	row := scheduleRow(t, app, "daily")

	_, data, err := runDailyActivityDigest(app, row)
	if err != nil {
		t.Fatalf("runDailyActivityDigest: %v", err)
	}
	ctx := data.(notifications.DailyActivityContext)

	if ctx.TransactionCount != 3 {
		t.Errorf("transactionCount = %d; want 3", ctx.TransactionCount)
	}
	// 2 + 1 + 1 + 5 = 9 qty-worth of lines across the in-window txs:
	// txA hammer×2, txB driver×1, txC hammer×1 + screws×5.
	if ctx.LinesCount != 9 {
		t.Errorf("linesCount = %d; want 9", ctx.LinesCount)
	}
	if ctx.UniqueUsers != 2 {
		t.Errorf("uniqueUsers = %d; want 2", ctx.UniqueUsers)
	}
	if ctx.CheckedOut != 4 {
		t.Errorf("checkedOut = %d; want 4", ctx.CheckedOut)
	}
	if ctx.Consumed != 5 {
		t.Errorf("consumed = %d; want 5", ctx.Consumed)
	}
	if ctx.Returned != 0 {
		t.Errorf("returned = %d; want 0", ctx.Returned)
	}

	// Top items: hammer appears in 2 lines, screws in 1, driver in 1. Hammer
	// wins; the two-way tie between screws and driver breaks alphabetically
	// by item ID — we don't assert that order here, just that hammer leads.
	if len(ctx.TopItems) == 0 || ctx.TopItems[0].ItemCode != fix.hammerCode {
		t.Errorf("top item = %+v; want hammer first", ctx.TopItems)
	}
	if len(ctx.TopItems) != 3 {
		t.Errorf("len(topItems) = %d; want 3", len(ctx.TopItems))
	}

	// Top workers: alice has 3 lines (txA + txC consume), bob has 1 (txB).
	if len(ctx.TopWorkers) == 0 || ctx.TopWorkers[0].UserCode != fix.aliceCode {
		t.Errorf("top worker = %+v; want alice first", ctx.TopWorkers)
	}

	// Out-of-window transaction (3 days old) must not bleed in.
	if ctx.TransactionCount > 3 {
		t.Errorf("out-of-window transaction leaked into aggregate")
	}
}

type activityFixtures struct {
	hammerCode, screwCode, driverCode string
	aliceCode, bobCode                string
}

// seedActivityFixtures builds three in-window transactions and one
// out-of-window transaction so the runner has something to aggregate and
// something to ignore.
func seedActivityFixtures(t *testing.T, app core.App) activityFixtures {
	t.Helper()

	groups, err := app.FindCollectionByNameOrId("groups")
	if err != nil {
		t.Fatalf("find groups: %v", err)
	}
	g := core.NewRecord(groups)
	g.Set("code", "ELEC")
	g.Set("name", "Electrical")
	if err := app.Save(g); err != nil {
		t.Fatalf("save group: %v", err)
	}

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	alice := core.NewRecord(users)
	alice.Set("name", "Alice")
	alice.Set("code", "EMP-1")
	alice.Set("email", "alice@example.com")
	alice.Set("role", "worker")
	alice.Set("group", g.Id)
	alice.Set("active", true)
	alice.SetPassword("alice-password-123")
	if err := app.Save(alice); err != nil {
		t.Fatalf("save alice: %v", err)
	}
	bob := core.NewRecord(users)
	bob.Set("name", "Bob")
	bob.Set("code", "EMP-2")
	bob.Set("email", "bob@example.com")
	bob.Set("role", "worker")
	bob.Set("group", g.Id)
	bob.Set("active", true)
	bob.SetPassword("bob-password-123")
	if err := app.Save(bob); err != nil {
		t.Fatalf("save bob: %v", err)
	}

	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	hammer := core.NewRecord(items)
	hammer.Set("code", "HAMMER")
	hammer.Set("name", "Hammer")
	hammer.Set("type", "tool")
	hammer.Set("tracking_mode", "quantity")
	hammer.Set("active", true)
	if err := app.Save(hammer); err != nil {
		t.Fatalf("save hammer: %v", err)
	}
	driver := core.NewRecord(items)
	driver.Set("code", "DRIVER")
	driver.Set("name", "Driver")
	driver.Set("type", "tool")
	driver.Set("tracking_mode", "quantity")
	driver.Set("active", true)
	if err := app.Save(driver); err != nil {
		t.Fatalf("save driver: %v", err)
	}
	screw := core.NewRecord(items)
	screw.Set("code", "SCREW")
	screw.Set("name", "Screws")
	screw.Set("type", "consumable")
	screw.Set("tracking_mode", "quantity")
	screw.Set("active", true)
	if err := app.Save(screw); err != nil {
		t.Fatalf("save screw: %v", err)
	}

	now := time.Now().UTC()
	inWindow := now.Add(-2 * time.Hour)
	outOfWindow := now.Add(-3 * 24 * time.Hour)

	// txA: alice checks out 2 hammers — in window
	makeTx(t, app, alice.Id, inWindow, []lineSpec{{itemID: hammer.Id, action: "checkout", qty: 2}})
	// txB: bob checks out 1 driver — in window
	makeTx(t, app, bob.Id, inWindow, []lineSpec{{itemID: driver.Id, action: "checkout", qty: 1}})
	// txC: alice consumes 5 screws + checks 1 hammer (2 lines, same tx) — in window
	makeTx(t, app, alice.Id, inWindow, []lineSpec{
		{itemID: screw.Id, action: "consume", qty: 5},
		{itemID: hammer.Id, action: "checkout", qty: 1},
	})
	// out-of-window: alice consumes 100 screws three days ago
	makeTx(t, app, alice.Id, outOfWindow, []lineSpec{{itemID: screw.Id, action: "consume", qty: 100}})

	return activityFixtures{
		hammerCode: "HAMMER",
		screwCode:  "SCREW",
		driverCode: "DRIVER",
		aliceCode:  "EMP-1",
		bobCode:    "EMP-2",
	}
}

type lineSpec struct {
	itemID string
	action string
	qty    int
}

func makeTx(t *testing.T, app core.App, userID string, completedAt time.Time, lines []lineSpec) {
	t.Helper()
	txs, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		t.Fatalf("find transactions: %v", err)
	}
	lc, err := app.FindCollectionByNameOrId("transaction_lines")
	if err != nil {
		t.Fatalf("find transaction_lines: %v", err)
	}
	tx := core.NewRecord(txs)
	tx.Set("kiosk_code", "BAY-01")
	tx.Set("location_code", "WH-A")
	tx.Set("user", userID)
	tx.Set("started_at", completedAt.Add(-1*time.Minute))
	tx.Set("completed_at", completedAt)
	tx.Set("status", "completed")
	if err := app.Save(tx); err != nil {
		t.Fatalf("save tx: %v", err)
	}
	for _, l := range lines {
		row := core.NewRecord(lc)
		row.Set("transaction", tx.Id)
		row.Set("item", l.itemID)
		row.Set("action", l.action)
		row.Set("qty", l.qty)
		if err := app.Save(row); err != nil {
			t.Fatalf("save line: %v", err)
		}
	}
}
