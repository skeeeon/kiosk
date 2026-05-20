package commit_test

import (
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/commit"
	"github.com/skeeeon/kiosk/internal/kioskctx"

	// Register kiosk migrations via init() so the runner can apply them below.
	_ "github.com/skeeeon/kiosk/migrations"
)

var testIdentity = kioskctx.Identity{KioskCode: "TEST", LocationCode: "T"}

// setupApp boots a fresh PB app in a temp dir with our migrations applied.
// Each test gets its own DB — slower but isolation matters more than speed.
//
// migratecmd's Automigrate hooks OnServe (not OnBootstrap), so in tests we
// apply migrations by iterating the registered list directly.
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
	UserID, OtherUserID string
	ToolQtyID           string
	ToolSerialID        string
	ConsumableID        string
}

func seedFixtures(t *testing.T, app core.App) seed {
	t.Helper()

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	alice := core.NewRecord(users)
	alice.Set("email", "alice@test.local")
	alice.Set("name", "Alice")
	alice.Set("code", "EMP-1")
	alice.Set("role", "worker")
	alice.Set("active", true)
	alice.SetPassword("alice-password-123")
	if err := app.Save(alice); err != nil {
		t.Fatalf("save alice: %v", err)
	}

	bob := core.NewRecord(users)
	bob.Set("email", "bob@test.local")
	bob.Set("name", "Bob")
	bob.Set("code", "EMP-2")
	bob.Set("role", "worker")
	bob.Set("active", true)
	bob.SetPassword("bob-password-123")
	if err := app.Save(bob); err != nil {
		t.Fatalf("save bob: %v", err)
	}

	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	toolQty := core.NewRecord(items)
	toolQty.Set("code", "HAMMER")
	toolQty.Set("name", "Hammer")
	toolQty.Set("type", "tool")
	toolQty.Set("tracking_mode", "quantity")
	toolQty.Set("active", true)
	if err := app.Save(toolQty); err != nil {
		t.Fatalf("save hammer: %v", err)
	}

	toolSerial := core.NewRecord(items)
	toolSerial.Set("code", "DR-042")
	toolSerial.Set("name", "Impact Driver SN-042")
	toolSerial.Set("type", "tool")
	toolSerial.Set("tracking_mode", "serialized")
	toolSerial.Set("serial", "SN-042")
	toolSerial.Set("active", true)
	if err := app.Save(toolSerial); err != nil {
		t.Fatalf("save impact driver: %v", err)
	}

	consumable := core.NewRecord(items)
	consumable.Set("code", "SCREW-3IN")
	consumable.Set("name", "Deck Screws 3in")
	consumable.Set("type", "consumable")
	consumable.Set("tracking_mode", "quantity")
	consumable.Set("active", true)
	if err := app.Save(consumable); err != nil {
		t.Fatalf("save screws: %v", err)
	}

	return seed{
		UserID:       alice.Id,
		OtherUserID:  bob.Id,
		ToolQtyID:    toolQty.Id,
		ToolSerialID: toolSerial.Id,
		ConsumableID: consumable.Id,
	}
}

func newCart(userID string, lines ...*cart.Line) *cart.Cart {
	return &cart.Cart{
		ID:        "test-cart",
		UserID:    userID,
		UserCode:  "EMP-1",
		UserName:  "Alice",
		StartedAt: time.Now().Add(-1 * time.Minute),
		Lines:     lines,
	}
}

type captured struct {
	events []capturedEvent
}

type capturedEvent struct {
	Subject string
	Payload any
}

func (c *captured) publish(subject string, payload any) {
	c.events = append(c.events, capturedEvent{subject, payload})
}

func (c *captured) subjects() []string {
	out := make([]string, 0, len(c.events))
	for _, e := range c.events {
		out = append(out, e.Subject)
	}
	return out
}

func countOpenCheckouts(t *testing.T, app core.App, filter string, params dbx.Params) int {
	t.Helper()
	rows, err := app.FindRecordsByFilter("open_checkouts", filter, "", 0, 0, params)
	if err != nil {
		t.Fatalf("count open_checkouts: %v", err)
	}
	return len(rows)
}

// ----- tests -----

func TestCheckout_Tool_NotCurrentlyOut_InsertsOpenCheckout(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, ItemCode: "HAMMER", ItemName: "Hammer",
		ItemType: "tool", TrackingMode: "quantity",
		Action: "checkout", Qty: 1,
	})
	pub := &captured{}

	result, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), pub.publish)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if result.CheckedOut != 1 || result.LinesCount != 1 {
		t.Errorf("result counts: %+v", result)
	}
	if n := countOpenCheckouts(t, app, "user = {:u}", dbx.Params{"u": s.UserID}); n != 1 {
		t.Errorf("open_checkouts for user: want 1, got %d", n)
	}
}

func TestCheckout_NonSerialized_QtyN_InsertsNRows(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, ItemCode: "HAMMER", ItemName: "Hammer",
		ItemType: "tool", TrackingMode: "quantity",
		Action: "checkout", Qty: 3,
	})
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n := countOpenCheckouts(t, app, "user = {:u}", dbx.Params{"u": s.UserID}); n != 3 {
		t.Errorf("open rows: want 3, got %d", n)
	}
}

func TestCheckout_Serialized_QtyTwo_Rejected(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolSerialID, ItemCode: "DR-042", ItemName: "Impact Driver",
		ItemType: "tool", TrackingMode: "serialized", Serial: "SN-042",
		Action: "checkout", Qty: 2,
	})
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err == nil {
		t.Fatal("expected error for serialized qty>1, got nil")
	}
}

func TestReturn_Tool_CurrentlyOut_DeletesOpenCheckout(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// First, commit a checkout to put the tool in open_checkouts.
	checkout := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	if _, err := commit.Commit(app, checkout, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed checkout: %v", err)
	}

	// Now return it.
	returnCart := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "return", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	if _, err := commit.Commit(app, returnCart, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("commit return: %v", err)
	}

	if n := countOpenCheckouts(t, app, "", nil); n != 0 {
		t.Errorf("open_checkouts after return: want 0, got %d", n)
	}
}

func TestReturn_Tool_NotCurrentlyOut_SetsUncorrelated(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "return", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	result, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The transaction_line for that return should have uncorrelated=true.
	lines, _ := app.FindRecordsByFilter("transaction_lines", "transaction = {:tx}", "", 0, 0,
		dbx.Params{"tx": result.TransactionID})
	if len(lines) != 1 {
		t.Fatalf("lines: want 1, got %d", len(lines))
	}
	if !lines[0].GetBool("uncorrelated") {
		t.Error("expected uncorrelated=true on the return line")
	}
}

func TestReturn_CrossUser_DeletesOriginalUsersOpenRow(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Bob checks out the tool.
	bobCheckout := newCart(s.OtherUserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	bobCheckout.UserCode = "EMP-2"
	bobCheckout.UserName = "Bob"
	if _, err := commit.Commit(app, bobCheckout, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed bob checkout: %v", err)
	}

	// Alice returns Bob's tool — the cart line carries Bob's id as
	// original_checkout_user (set at add-time by the cart handler).
	aliceReturn := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "return", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
		OriginalCheckoutUserID: s.OtherUserID,
	})
	if _, err := commit.Commit(app, aliceReturn, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("commit alice return: %v", err)
	}

	if n := countOpenCheckouts(t, app, "user = {:u}", dbx.Params{"u": s.OtherUserID}); n != 0 {
		t.Errorf("bob's open rows: want 0, got %d", n)
	}
}

func TestConsume_Consumable_NoOpenCheckoutChange(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ConsumableID, Action: "consume", Qty: 99,
		ItemType: "consumable", TrackingMode: "quantity",
	})
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n := countOpenCheckouts(t, app, "", nil); n != 0 {
		t.Errorf("open_checkouts after consume: want 0, got %d", n)
	}
}

func TestCommit_MixedCart_ProducesAllExpectedSideEffects(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Seed Alice with one hammer already out so we can return it.
	pre := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	if _, err := commit.Commit(app, pre, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pub := &captured{}
	mixed := newCart(s.UserID,
		// return the hammer she has out
		&cart.Line{
			ItemID: s.ToolQtyID, Action: "return", Qty: 1,
			ItemType: "tool", TrackingMode: "quantity",
		},
		// checkout the impact driver (serialized)
		&cart.Line{
			ItemID: s.ToolSerialID, Action: "checkout", Qty: 1,
			ItemType: "tool", TrackingMode: "serialized", Serial: "SN-042",
		},
		// consume 50 screws
		&cart.Line{
			ItemID: s.ConsumableID, Action: "consume", Qty: 50,
			ItemType: "consumable", TrackingMode: "quantity",
		},
	)
	result, err := commit.Commit(app, mixed, testIdentity, commit.DefaultPolicy(), pub.publish)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if result.CheckedOut != 1 || result.Returned != 1 || result.Consumed != 1 {
		t.Errorf("counts: %+v", result)
	}
	if result.LinesCount != 3 {
		t.Errorf("lines_count: want 3, got %d", result.LinesCount)
	}

	// Open checkouts should now hold one row: the serialized impact driver.
	rows, _ := app.FindRecordsByFilter("open_checkouts", "", "", 0, 0)
	if len(rows) != 1 {
		t.Fatalf("open_checkouts: want 1, got %d", len(rows))
	}
	if rows[0].GetString("item") != s.ToolSerialID {
		t.Errorf("open row item: want %s, got %s", s.ToolSerialID, rows[0].GetString("item"))
	}

	// One transaction.complete + three item.* events.
	if len(pub.events) != 4 {
		t.Errorf("events: want 4, got %d (%v)", len(pub.events), pub.subjects())
	}
	wantSubjects := map[string]int{
		"kiosk.TEST.transaction.complete": 1,
		"kiosk.TEST.item.return":          1,
		"kiosk.TEST.item.checkout":        1,
		"kiosk.TEST.item.consume":         1,
	}
	gotSubjects := map[string]int{}
	for _, s := range pub.subjects() {
		gotSubjects[s]++
	}
	for k, v := range wantSubjects {
		if gotSubjects[k] != v {
			t.Errorf("subject %s: want %d, got %d", k, v, gotSubjects[k])
		}
	}
}

func TestCommit_EmptyCart_Rejected(t *testing.T) {
	app := setupApp(t)
	c := &cart.Cart{ID: "x", UserID: "fake", Lines: []*cart.Line{}}
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err == nil {
		t.Fatal("expected error for empty cart")
	}
}

func TestConsume_DecrementsQuantityOnHand(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Seed consumable with 10 on hand.
	item, _ := app.FindRecordById("items", s.ConsumableID)
	item.Set("quantity_on_hand", 10)
	if err := app.Save(item); err != nil {
		t.Fatalf("seed quantity_on_hand: %v", err)
	}

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ConsumableID, Action: "consume", Qty: 4,
		ItemType: "consumable", TrackingMode: "quantity",
	})
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, _ := app.FindRecordById("items", s.ConsumableID)
	if got := after.GetInt("quantity_on_hand"); got != 6 {
		t.Errorf("quantity_on_hand after consume 4 of 10: want 6, got %d", got)
	}
}

func TestConsume_AllowedToGoNegative(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Seed consumable with 2 on hand.
	item, _ := app.FindRecordById("items", s.ConsumableID)
	item.Set("quantity_on_hand", 2)
	if err := app.Save(item); err != nil {
		t.Fatalf("seed quantity_on_hand: %v", err)
	}

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ConsumableID, Action: "consume", Qty: 5,
		ItemType: "consumable", TrackingMode: "quantity",
	})
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, _ := app.FindRecordById("items", s.ConsumableID)
	if got := after.GetInt("quantity_on_hand"); got != -3 {
		t.Errorf("quantity_on_hand after consume 5 of 2: want -3, got %d", got)
	}
}

func TestCheckout_DoesNotChangeQuantityOnHand(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Tool fleet count = 5.
	item, _ := app.FindRecordById("items", s.ToolQtyID)
	item.Set("quantity_on_hand", 5)
	if err := app.Save(item); err != nil {
		t.Fatalf("seed tool fleet: %v", err)
	}

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 3,
		ItemType: "tool", TrackingMode: "quantity",
	})
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, _ := app.FindRecordById("items", s.ToolQtyID)
	if got := after.GetInt("quantity_on_hand"); got != 5 {
		t.Errorf("tool quantity_on_hand should not change on checkout: want 5, got %d", got)
	}
}

func TestCrossUserReturn_RejectedWhenPolicyDenies(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Bob checks out the tool.
	bobCheckout := newCart(s.OtherUserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	bobCheckout.UserCode = "EMP-2"
	bobCheckout.UserName = "Bob"
	if _, err := commit.Commit(app, bobCheckout, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed bob checkout: %v", err)
	}

	// Alice tries to return Bob's tool under a strict policy.
	aliceReturn := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "return", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
		OriginalCheckoutUserID: s.OtherUserID,
	})
	strict := commit.Policy{AllowCrossUser: false, AllowUncorrelated: true}
	if _, err := commit.Commit(app, aliceReturn, testIdentity, strict, (&captured{}).publish); err == nil {
		t.Fatal("expected error for cross-user return under strict policy")
	}

	// Bob's open checkout should still be there — the rejected commit must
	// roll back, not leak state.
	if n := countOpenCheckouts(t, app, "user = {:u}", dbx.Params{"u": s.OtherUserID}); n != 1 {
		t.Errorf("bob's open rows after rejected return: want 1, got %d", n)
	}
}

func TestUncorrelatedReturn_RejectedWhenPolicyDenies(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Nothing is out — this return is uncorrelated by definition.
	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "return", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	strict := commit.Policy{AllowCrossUser: true, AllowUncorrelated: false}
	if _, err := commit.Commit(app, c, testIdentity, strict, (&captured{}).publish); err == nil {
		t.Fatal("expected error for uncorrelated return under strict policy")
	}

	// No transactions or open rows written.
	txs, _ := app.FindRecordsByFilter("transactions", "", "", 0, 0)
	if len(txs) != 0 {
		t.Errorf("transactions after rejected return: want 0, got %d", len(txs))
	}
}

func TestCommit_TransactionRollsBackOnError(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Cart that will fail mid-commit: valid first line, invalid second.
	c := newCart(s.UserID,
		&cart.Line{
			ItemID: s.ToolQtyID, Action: "checkout", Qty: 1,
			ItemType: "tool", TrackingMode: "quantity",
		},
		&cart.Line{
			ItemID: "nonexistent-item-id", Action: "checkout", Qty: 1,
		},
	)
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err == nil {
		t.Fatal("expected error")
	}

	// Nothing should have been written.
	if n := countOpenCheckouts(t, app, "", nil); n != 0 {
		t.Errorf("open_checkouts after rollback: want 0, got %d", n)
	}
	txs, _ := app.FindRecordsByFilter("transactions", "", "", 0, 0)
	if len(txs) != 0 {
		t.Errorf("transactions after rollback: want 0, got %d", len(txs))
	}
}
