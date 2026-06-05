package handlers

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/commit"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/ledger"

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

// TestExpectedOpenCheckouts_MatchesActualAfterUncorrelatedReturn confirms
// that the integrity replay (expectedOpenCheckouts) agrees with the
// commit hook's actual open_checkouts writes after an uncorrelated
// return — i.e. the case that used to diverge when the resolver had a
// cross-user fallback.
//
// Scenario: Alice (foreman) and Bob each have 1 hammer out. Alice
// commits a self-return of qty=2. The resolver closes only Alice's
// row, flags the line uncorrelated, and leaves Bob's row intact.
// Replay charges -2 against Alice → Alice clamps to 0, Bob = +1.
// Both match reality.
func TestExpectedOpenCheckouts_MatchesActualAfterUncorrelatedReturn(t *testing.T) {
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
	// Foreman so policy.AllowUncorrelated + role gate both pass.
	alice.Set("role", "foreman")
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

	// Alice returns qty=2 — closes her own row, leaves Bob's alone,
	// flags the line uncorrelated.
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

	// Reality: only Bob's row remains.
	actualRows, err := app.FindRecordsByFilter("open_checkouts", "", "", 0, 0)
	if err != nil {
		t.Fatalf("load open_checkouts: %v", err)
	}
	if len(actualRows) != 1 {
		t.Fatalf("actual open_checkouts: want 1, got %d", len(actualRows))
	}
	if actualRows[0].GetString("user") != bob.Id {
		t.Errorf("surviving row user: want Bob, got %s", actualRows[0].GetString("user"))
	}

	// Replay matches: Alice clamps to 0, Bob = +1.
	expected, _, err := expectedOpenCheckouts(app)
	if err != nil {
		t.Fatalf("expectedOpenCheckouts: %v", err)
	}
	bobKey := openKey{item: hammer.Id, instance: "", user: bob.Id}
	if got := expected[bobKey]; got != 1 {
		t.Errorf("expected[Bob]: want 1, got %d", got)
	}
	aliceKey := openKey{item: hammer.Id, instance: "", user: alice.Id}
	// Alice's raw replay value is -1 (one +1 checkout, one -2 return).
	// The Integrity handler clamps negatives to zero downstream; we
	// assert the raw value here so a future change to the clamp logic
	// doesn't quietly mask drift.
	if got := expected[aliceKey]; got != -1 {
		t.Errorf("expected[Alice] raw: want -1, got %d", got)
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

	rows, err := ledger.ReplayOpenRows(app, "")
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
	inst.Set("status", "in_service")
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

	rows, err := ledger.ReplayOpenRows(app, "")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows after serialized return: want 0, got %d", len(rows))
	}
}

// TestReplayOpenRows_NoCrossUserBorrowOnOverReturn is the rebuild-side twin
// of TestExpectedOpenCheckouts_MatchesActualAfterUncorrelatedReturn. It
// confirms ledger.ReplayOpenRows (which backs the integrity REBUILD and the
// reports view) agrees with commit after an over-quantity return: only the
// returning user's row is removed and other users' rows survive. The old
// cross-user fallback deleted Bob's row here — the bug that let "rebuild"
// corrupt state the integrity "check" reported healthy.
func TestReplayOpenRows_NoCrossUserBorrowOnOverReturn(t *testing.T) {
	app := setupAppInternal(t)

	users, _ := app.FindCollectionByNameOrId("users")
	electricalID := ensureGroupForTest(t, app, "electrical")
	alice := core.NewRecord(users)
	alice.Set("email", "alice@test.local")
	alice.Set("name", "Alice")
	alice.Set("code", "EMP-A")
	alice.Set("role", "foreman")
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

	for _, u := range []struct {
		id, code, name, cartID string
	}{{bob.Id, "EMP-B", "Bob", "c1"}, {alice.Id, "EMP-A", "Alice", "c2"}} {
		c := &cart.Cart{
			ID: u.cartID, UserID: u.id, UserCode: u.code, UserName: u.name,
			Lines: []*cart.Line{{
				ItemID: hammer.Id, ItemCode: "HAMMER", ItemName: "Hammer",
				ItemType: "tool", TrackingMode: "quantity",
				Action: "checkout", Qty: 1,
			}},
		}
		if _, err := commit.Commit(app, c, identity, commit.DefaultPolicy(), noopPublish); err != nil {
			t.Fatalf("%s checkout: %v", u.name, err)
		}
	}

	// Alice returns qty=2 — over-returns; commit closes only her row.
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

	rows, err := ledger.ReplayOpenRows(app, "")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("replay after over-return: want 1 row (Bob's), got %d", len(rows))
	}
	if rows[0].User != bob.Id {
		t.Errorf("surviving replay row: want Bob, got %s", rows[0].User)
	}
}

// TestReplayOpenRows_AdminCloseRemovesRow confirms admin_close lines are
// replayed as a subtraction, so neither the integrity check reports false
// drift nor the rebuild resurrects an administratively-closed checkout.
func TestReplayOpenRows_AdminCloseRemovesRow(t *testing.T) {
	app := setupAppInternal(t)

	admins, err := app.FindCollectionByNameOrId("admins")
	if err != nil {
		t.Fatalf("find admins: %v", err)
	}
	admin := core.NewRecord(admins)
	admin.Set("email", "admin@test.local")
	admin.Set("name", "Admin")
	admin.SetPassword("password-adminadmin")
	if err := app.Save(admin); err != nil {
		t.Fatalf("save admin: %v", err)
	}

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

	checkout := &cart.Cart{
		ID: "c1", UserID: worker.Id, UserCode: "EMP-W", UserName: "Worker",
		Lines: []*cart.Line{{
			ItemID: hammer.Id, ItemCode: "HAMMER", ItemName: "Hammer",
			ItemType: "tool", TrackingMode: "quantity",
			Action: "checkout", Qty: 1,
		}},
	}
	if _, err := commit.Commit(app, checkout, identity, commit.DefaultPolicy(), noopPublish); err != nil {
		t.Fatalf("checkout: %v", err)
	}

	openRows, err := app.FindRecordsByFilter("open_checkouts", "", "", 1, 0)
	if err != nil || len(openRows) != 1 {
		t.Fatalf("expected 1 open row, got %d (err %v)", len(openRows), err)
	}

	// returned_offline does not touch inventory — keeps the test focused on
	// the ledger subtraction rather than the qty side-effect.
	if _, err := commit.AdminClose(app, commit.AdminCloseInput{
		OpenCheckoutID: openRows[0].Id,
		ActorID:        admin.Id,
		Source:         "local",
		Reason:         "returned_offline",
		Identity:       identity,
	}, noopPublish); err != nil {
		t.Fatalf("admin close: %v", err)
	}

	// Rebuild replay must not resurrect the closed row.
	rows, err := ledger.ReplayOpenRows(app, "")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("replay after admin_close: want 0 rows, got %d", len(rows))
	}

	// Integrity check must net to zero for the holder (raw +1 checkout, -1
	// admin_close), i.e. no drift.
	expected, _, err := expectedOpenCheckouts(app)
	if err != nil {
		t.Fatalf("expectedOpenCheckouts: %v", err)
	}
	if got := expected[openKey{item: hammer.Id, instance: "", user: worker.Id}]; got != 0 {
		t.Errorf("expected[worker] after admin_close: want 0, got %d", got)
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
