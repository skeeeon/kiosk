package handlers

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/commit"
	"github.com/skeeeon/kiosk/internal/kioskctx"

	_ "github.com/skeeeon/kiosk/migrations"
)

// setupAppInternal is the in-package twin of setupApp in stock_adjust_test.go
// (which lives in `handlers_test`). Duplicated rather than exported because
// the test is the only caller and exporting a test-only helper would muddy
// the public surface.
func setupAppInternal(t *testing.T) *pocketbase.PocketBase {
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

// TestExpectedOpenCheckouts_FallbackDivergence pins a known divergence
// between expectedOpenCheckouts (the integrity replay) and the commit hook's
// closeCheckoutsForLine fallback behavior.
//
// Scenario: Alice and Bob each check out 1 hammer. Alice's cart then carries
// a return line for hammer with qty=2 and no OriginalCheckoutUserID (e.g.,
// she bumped the qty via the line's +/- control after a single self-scan).
// At commit, closeCheckoutsForLine prefers Alice's open row, finds only 1,
// and falls back to deleting Bob's row to satisfy qty=2. The return is NOT
// flagged uncorrelated (deleted == qty), and OriginalCheckoutUserID is empty
// so the cross-user foreman gate never engages.
//
// Actual table state after commit: empty (both rows closed).
// Integrity replay state: charges -2 entirely against Alice → Alice clamps
// to 0, Bob still shows +1. A subsequent /integrity diff would report Bob
// as missing 1 hammer he doesn't actually have out — a false-positive drift
// signal that would prompt an unnecessary rebuild.
//
// This test pins today's behavior. The fix (if real ops pain surfaces) is
// in expectedOpenCheckouts: when a return's target user can't cover qty,
// charge the deficit against an arbitrary other-user bucket the same way
// closeCheckoutsForLine does, so the replay model mirrors the commit policy.
func TestExpectedOpenCheckouts_FallbackDivergence(t *testing.T) {
	app := setupAppInternal(t)

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	electricalID := ensureGroupForTest(t, app, "electrical")
	alice := core.NewRecord(users)
	alice.Set("email", "alice@test.local")
	alice.Set("name", "Alice")
	alice.Set("code", "EMP-A")
	alice.Set("role", "worker")
	alice.Set("group", electricalID)
	alice.Set("active", true)
	alice.SetPassword("password-aaaaaaaaaaaa")
	if err := app.Save(alice); err != nil {
		t.Fatalf("save alice: %v", err)
	}
	bob := core.NewRecord(users)
	bob.Set("email", "bob@test.local")
	bob.Set("name", "Bob")
	bob.Set("code", "EMP-B")
	bob.Set("role", "worker")
	bob.Set("group", electricalID)
	bob.Set("active", true)
	bob.SetPassword("password-bbbbbbbbbbbb")
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

	identity := kioskctx.Identity{KioskCode: "TEST", LocationCode: "T"}
	noopPublish := func(string, any) {}

	bobCheckout := &cart.Cart{
		ID: "c1", UserID: bob.Id, UserCode: "EMP-B", UserName: "Bob",
		Lines: []*cart.Line{{
			ItemID: hammer.Id, ItemCode: "HAMMER", ItemName: "Hammer",
			ItemType: "tool", TrackingMode: "quantity",
			Action: "checkout", Qty: 1,
		}},
	}
	if _, err := commit.Commit(app, bobCheckout, identity, commit.DefaultPolicy(), noopPublish); err != nil {
		t.Fatalf("bob checkout: %v", err)
	}
	aliceCheckout := &cart.Cart{
		ID: "c2", UserID: alice.Id, UserCode: "EMP-A", UserName: "Alice",
		Lines: []*cart.Line{{
			ItemID: hammer.Id, ItemCode: "HAMMER", ItemName: "Hammer",
			ItemType: "tool", TrackingMode: "quantity",
			Action: "checkout", Qty: 1,
		}},
	}
	if _, err := commit.Commit(app, aliceCheckout, identity, commit.DefaultPolicy(), noopPublish); err != nil {
		t.Fatalf("alice checkout: %v", err)
	}

	// Alice returns qty=2 — fallback closes Bob's row to satisfy.
	aliceReturn := &cart.Cart{
		ID: "c3", UserID: alice.Id, UserCode: "EMP-A", UserName: "Alice",
		Lines: []*cart.Line{{
			ItemID: hammer.Id, ItemCode: "HAMMER", ItemName: "Hammer",
			ItemType: "tool", TrackingMode: "quantity",
			Action: "return", Qty: 2,
		}},
	}
	if _, err := commit.Commit(app, aliceReturn, identity, commit.DefaultPolicy(), noopPublish); err != nil {
		t.Fatalf("alice return: %v", err)
	}

	// Reality: both open_checkouts rows are gone — commit's fallback worked.
	actual, err := app.FindRecordsByFilter("open_checkouts", "", "", 0, 0)
	if err != nil {
		t.Fatalf("load open_checkouts: %v", err)
	}
	if len(actual) != 0 {
		t.Fatalf("actual open_checkouts after fallback return: want 0, got %d", len(actual))
	}

	// Integrity replay: charges -2 against Alice (target user), leaves Bob's
	// +1 intact — the divergence.
	expected, _, err := expectedOpenCheckouts(app)
	if err != nil {
		t.Fatalf("expectedOpenCheckouts: %v", err)
	}

	bobKey := openKey{item: hammer.Id, instance: "", user: bob.Id}
	if got := expected[bobKey]; got != 1 {
		t.Errorf("expected[Bob] under divergent replay: want 1, got %d", got)
	}
	aliceKey := openKey{item: hammer.Id, instance: "", user: alice.Id}
	if got := expected[aliceKey]; got != -1 {
		t.Errorf("expected[Alice] under divergent replay: want -1, got %d", got)
	}

	// Post-clamp behavior the Integrity handler applies: negative buckets
	// are zeroed. Alice = 0 = actual (no diff). Bob = 1 ≠ actual 0 → would
	// be reported as missing_in_table. That is the false-positive drift.
	if expected[bobKey] <= 0 {
		t.Fatalf("post-clamp Bob bucket must be > 0 to produce drift; got %d",
			expected[bobKey])
	}
}

// TestReplayOpenRows_PreservesTimestamps confirms the rebuild's ledger
// replay stamps each rebuilt row with the source checkout's completed_at
// (not time.Now() as the prior implementation did) and FK-links back to
// the original transaction line.
func TestReplayOpenRows_PreservesTimestamps(t *testing.T) {
	app := setupAppInternal(t)

	users, _ := app.FindCollectionByNameOrId("users")
	alice := core.NewRecord(users)
	alice.Set("email", "alice@test.local")
	alice.Set("name", "Alice")
	alice.Set("code", "EMP-A")
	alice.Set("role", "worker")
	alice.Set("active", true)
	alice.SetPassword("password-aaaaaaaaaaaa")
	if err := app.Save(alice); err != nil {
		t.Fatalf("save alice: %v", err)
	}

	items, _ := app.FindCollectionByNameOrId("items")
	hammer := core.NewRecord(items)
	hammer.Set("code", "HAMMER")
	hammer.Set("name", "Hammer")
	hammer.Set("type", "tool")
	hammer.Set("tracking_mode", "quantity")
	hammer.Set("active", true)
	if err := app.Save(hammer); err != nil {
		t.Fatalf("save hammer: %v", err)
	}

	identity := kioskctx.Identity{KioskCode: "TEST", LocationCode: "T"}
	noopPublish := func(string, any) {}

	// SQLite stores timestamps at millisecond precision, so the row's value
	// can sit a few hundred microseconds before our wall-clock snapshot. The
	// window is loose-on-both-sides to absorb that without being meaningless.
	before := time.Now().UTC().Add(-time.Second)
	aliceCart := &cart.Cart{
		ID: "c1", UserID: alice.Id, UserCode: "EMP-A", UserName: "Alice",
		Lines: []*cart.Line{{
			ItemID: hammer.Id, ItemCode: "HAMMER", ItemName: "Hammer",
			ItemType: "tool", TrackingMode: "quantity",
			Action: "checkout", Qty: 2,
		}},
	}
	result, err := commit.Commit(app, aliceCart, identity, commit.DefaultPolicy(), noopPublish)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	rows, err := replayOpenRows(app)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: want 2, got %d", len(rows))
	}
	// Both rows should carry the commit's completed_at, the source line's
	// FK, and the correct user/item.
	lines, _ := app.FindRecordsByFilter("transaction_lines",
		"transaction = {:tx}", "", 0, 0,
		map[string]any{"tx": result.TransactionID})
	if len(lines) != 1 {
		t.Fatalf("source lines: want 1, got %d", len(lines))
	}
	srcLineID := lines[0].Id

	for i, r := range rows {
		if r.User != alice.Id {
			t.Errorf("row %d user: want alice, got %s", i, r.User)
		}
		if r.Item != hammer.Id {
			t.Errorf("row %d item: want hammer, got %s", i, r.Item)
		}
		if r.TransactionLine != srcLineID {
			t.Errorf("row %d transaction_line: want %s, got %s",
				i, srcLineID, r.TransactionLine)
		}
		if r.CheckedOutAt.Before(before) || r.CheckedOutAt.After(after) {
			t.Errorf("row %d checked_out_at %v not within [%v, %v]",
				i, r.CheckedOutAt, before, after)
		}
	}
}

// TestReplayOpenRows_SerializedInstanceReturn confirms a serialized return
// closes by instance regardless of user — mirrors closeCheckoutsForLine.
func TestReplayOpenRows_SerializedInstanceReturn(t *testing.T) {
	app := setupAppInternal(t)

	users, _ := app.FindCollectionByNameOrId("users")
	worker := core.NewRecord(users)
	worker.Set("email", "w@test.local")
	worker.Set("name", "Worker")
	worker.Set("code", "EMP-W")
	worker.Set("role", "worker")
	worker.Set("active", true)
	worker.SetPassword("password-wwwwwwwwwwww")
	if err := app.Save(worker); err != nil {
		t.Fatalf("save worker: %v", err)
	}

	items, _ := app.FindCollectionByNameOrId("items")
	driver := core.NewRecord(items)
	driver.Set("code", "DR-042")
	driver.Set("name", "Impact Driver")
	driver.Set("type", "tool")
	driver.Set("tracking_mode", "serialized")
	driver.Set("active", true)
	if err := app.Save(driver); err != nil {
		t.Fatalf("save driver: %v", err)
	}
	instances, _ := app.FindCollectionByNameOrId("item_instances")
	inst := core.NewRecord(instances)
	inst.Set("item", driver.Id)
	inst.Set("code", "DR-042-A")
	inst.Set("serial", "SN-A")
	inst.Set("active", true)
	if err := app.Save(inst); err != nil {
		t.Fatalf("save instance: %v", err)
	}

	identity := kioskctx.Identity{KioskCode: "TEST", LocationCode: "T"}
	noopPublish := func(string, any) {}

	// Checkout + return the same instance — replay should produce zero
	// open rows.
	checkout := &cart.Cart{
		ID: "c1", UserID: worker.Id, UserCode: "EMP-W", UserName: "Worker",
		Lines: []*cart.Line{{
			ItemID: driver.Id, ItemCode: "DR-042-A", ItemName: "Impact Driver",
			ItemType: "tool", TrackingMode: "serialized", Serial: "SN-A",
			ItemInstanceID: inst.Id,
			Action:         "checkout", Qty: 1,
		}},
	}
	if _, err := commit.Commit(app, checkout, identity, commit.DefaultPolicy(), noopPublish); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	returnCart := &cart.Cart{
		ID: "c2", UserID: worker.Id, UserCode: "EMP-W", UserName: "Worker",
		Lines: []*cart.Line{{
			ItemID: driver.Id, ItemCode: "DR-042-A", ItemName: "Impact Driver",
			ItemType: "tool", TrackingMode: "serialized", Serial: "SN-A",
			ItemInstanceID: inst.Id,
			Action:         "return", Qty: 1,
		}},
	}
	if _, err := commit.Commit(app, returnCart, identity, commit.DefaultPolicy(), noopPublish); err != nil {
		t.Fatalf("return: %v", err)
	}

	rows, err := replayOpenRows(app)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows after serialized return: want 0, got %d", len(rows))
	}
}

// ensureGroupForTest returns the id of a groups row with the given code,
// creating it on first use. Allows tests in this package to set users.group
// using a human-readable label.
func ensureGroupForTest(t *testing.T, app core.App, code string) string {
	t.Helper()
	if existing, err := app.FindFirstRecordByFilter("groups",
		"code = {:c}", map[string]any{"c": code}); err == nil {
		return existing.Id
	}
	col, err := app.FindCollectionByNameOrId("groups")
	if err != nil {
		t.Fatalf("find groups collection: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("code", code)
	rec.Set("name", code)
	rec.Set("active", true)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save group %q: %v", code, err)
	}
	return rec.Id
}
