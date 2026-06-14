package commit_test

import (
	"strings"
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
	UserID, OtherUserID   string
	ToolQtyID             string
	ToolSerialID          string
	ToolSerialInstanceID  string
	ToolSerialInstance2ID string // a second instance of the same serialized SKU
	ConsumableID          string
}

func seedFixtures(t *testing.T, app core.App) seed {
	t.Helper()

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	electricalID := ensureGroup(t, app, "electrical")
	// Alice is a foreman in the "electrical" group. The positive-path return
	// tests below rely on this: only a foreman in the same group as the
	// original checkout user can perform cross-user or uncorrelated returns.
	alice := core.NewRecord(users)
	alice.Set("email", "alice@test.local")
	alice.Set("name", "Alice")
	alice.Set("code", "EMP-1")
	alice.Set("role", "foreman")
	alice.Set("group", electricalID)
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
	bob.Set("group", electricalID)
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
	toolSerial.Set("name", "Impact Driver")
	toolSerial.Set("type", "tool")
	toolSerial.Set("tracking_mode", "serialized")
	toolSerial.Set("active", true)
	if err := app.Save(toolSerial); err != nil {
		t.Fatalf("save impact driver: %v", err)
	}

	// Two instances of the same serialized SKU. Tests for cross-instance
	// behavior (returning B doesn't touch A) need both.
	instances, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		t.Fatalf("find item_instances: %v", err)
	}
	instA := core.NewRecord(instances)
	instA.Set("item", toolSerial.Id)
	instA.Set("code", "DR-042-A")
	instA.Set("serial", "SN-A")
	instA.Set("status", "in_service")
	if err := app.Save(instA); err != nil {
		t.Fatalf("save instance A: %v", err)
	}
	instB := core.NewRecord(instances)
	instB.Set("item", toolSerial.Id)
	instB.Set("code", "DR-042-B")
	instB.Set("serial", "SN-B")
	instB.Set("status", "in_service")
	if err := app.Save(instB); err != nil {
		t.Fatalf("save instance B: %v", err)
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
		UserID:                alice.Id,
		OtherUserID:           bob.Id,
		ToolQtyID:             toolQty.Id,
		ToolSerialID:          toolSerial.Id,
		ToolSerialInstanceID:  instA.Id,
		ToolSerialInstance2ID: instB.Id,
		ConsumableID:          consumable.Id,
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

// txCompletePayload returns the transaction.complete event payload map, failing
// the test if none was captured. Matched by subject suffix since item.{action}
// payloads also carry a transaction_id.
func txCompletePayload(t *testing.T, pub *captured) map[string]any {
	t.Helper()
	for _, e := range pub.events {
		if strings.HasSuffix(e.Subject, ".transaction.complete") {
			m, ok := e.Payload.(map[string]any)
			if !ok {
				t.Fatalf("transaction.complete payload not a map: %T", e.Payload)
			}
			return m
		}
	}
	t.Fatalf("no transaction.complete event captured (subjects: %v)", pub.subjects())
	return nil
}

// ----- tests -----

// TestCommit_StampsDoorID: a cart carrying a DoorID (set by the enclosure_diff
// path, or injected by the manual-commit handler) lands on the ledger row and
// rides the transaction.complete event.
func TestCommit_StampsDoorID(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, ItemCode: "HAMMER", ItemName: "Hammer",
		ItemType: "tool", TrackingMode: "quantity",
		Action: "checkout", Qty: 1,
	})
	c.DoorID = "DOOR-A"
	pub := &captured{}

	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), pub.publish); err != nil {
		t.Fatalf("commit: %v", err)
	}

	txs, err := app.FindRecordsByFilter("transactions", "user = {:u}", "", 0, 0, dbx.Params{"u": s.UserID})
	if err != nil {
		t.Fatalf("find transactions: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("transactions: want 1, got %d", len(txs))
	}
	if got := txs[0].GetString("door_id"); got != "DOOR-A" {
		t.Errorf("transaction door_id: want DOOR-A, got %q", got)
	}

	if got := txCompletePayload(t, pub)["door_id"]; got != "DOOR-A" {
		t.Errorf("event door_id: want DOOR-A, got %v", got)
	}
}

// TestCommit_OmitsDoorIDWhenEmpty: the common single-kiosk path leaves the
// column empty and keeps door_id off the wire entirely (the conditional in
// BuildTransactionCompletePayload), so old consumers see an unchanged payload.
func TestCommit_OmitsDoorIDWhenEmpty(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, ItemCode: "HAMMER", ItemName: "Hammer",
		ItemType: "tool", TrackingMode: "quantity",
		Action: "checkout", Qty: 1,
	})
	pub := &captured{}

	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), pub.publish); err != nil {
		t.Fatalf("commit: %v", err)
	}

	txs, err := app.FindRecordsByFilter("transactions", "user = {:u}", "", 0, 0, dbx.Params{"u": s.UserID})
	if err != nil {
		t.Fatalf("find transactions: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("transactions: want 1, got %d", len(txs))
	}
	if got := txs[0].GetString("door_id"); got != "" {
		t.Errorf("transaction door_id: want empty, got %q", got)
	}

	if _, ok := txCompletePayload(t, pub)["door_id"]; ok {
		t.Errorf("transaction.complete should omit door_id when empty")
	}
}

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
		ItemID: s.ToolSerialID, ItemCode: "DR-042-A", ItemName: "Impact Driver",
		ItemType: "tool", TrackingMode: "serialized", Serial: "SN-A",
		ItemInstanceID: s.ToolSerialInstanceID,
		Action:         "checkout", Qty: 2,
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

// When a return's qty exceeds the target user's open rows, the resolver
// no longer borrows from other users' open rows — the shortfall is
// surfaced as uncorrelated=true. A regular worker can't record an
// uncorrelated return, so the commit rolls back entirely.
func TestReturn_QtyExceedsTargetUserRows_RegularWorkerRejected(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Bob (worker) checks out 2 hammers.
	bobCheckout := newCart(s.OtherUserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 2,
		ItemType: "tool", TrackingMode: "quantity",
	})
	bobCheckout.UserCode = "EMP-2"
	bobCheckout.UserName = "Bob"
	if _, err := commit.Commit(app, bobCheckout, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed bob checkout: %v", err)
	}

	// Alice (foreman) also checks out 1 hammer — used to prove the
	// resolver no longer reaches into another user's open rows.
	aliceCheckout := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	if _, err := commit.Commit(app, aliceCheckout, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed alice checkout: %v", err)
	}

	// Bob attempts to return 3 — he only has 2 out.
	bobReturn := newCart(s.OtherUserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "return", Qty: 3,
		ItemType: "tool", TrackingMode: "quantity",
	})
	bobReturn.UserCode = "EMP-2"
	bobReturn.UserName = "Bob"
	_, err := commit.Commit(app, bobReturn, testIdentity, commit.DefaultPolicy(), (&captured{}).publish)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "only a foreman can record an uncorrelated return") {
		t.Errorf("unexpected error: %v", err)
	}

	// Commit rolled back: Bob's 2 rows and Alice's 1 row both intact.
	if n := countOpenCheckouts(t, app, "user = {:u}", dbx.Params{"u": s.OtherUserID}); n != 2 {
		t.Errorf("bob's open rows after rollback: want 2, got %d", n)
	}
	if n := countOpenCheckouts(t, app, "user = {:u}", dbx.Params{"u": s.UserID}); n != 1 {
		t.Errorf("alice's open rows: want 1 (untouched), got %d", n)
	}
}

// Same shape but with a foreman in the cart: the partial close lands,
// the line is flagged uncorrelated, and another user's open rows for
// the same SKU are not touched.
func TestReturn_QtyExceedsTargetUserRows_ForemanPartialUncorrelated(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Alice (foreman) checks out 2 hammers.
	aliceCheckout := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 2,
		ItemType: "tool", TrackingMode: "quantity",
	})
	if _, err := commit.Commit(app, aliceCheckout, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed alice checkout: %v", err)
	}

	// Bob (worker) checks out 1 hammer — the "evidence" row that the
	// old fallback would have consumed.
	bobCheckout := newCart(s.OtherUserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	bobCheckout.UserCode = "EMP-2"
	bobCheckout.UserName = "Bob"
	if _, err := commit.Commit(app, bobCheckout, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed bob checkout: %v", err)
	}

	// Alice attempts to return 3 — she only has 2 out.
	aliceReturn := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "return", Qty: 3,
		ItemType: "tool", TrackingMode: "quantity",
	})
	result, err := commit.Commit(app, aliceReturn, testIdentity, commit.DefaultPolicy(), (&captured{}).publish)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Alice's open rows are gone; Bob's row is untouched.
	if n := countOpenCheckouts(t, app, "user = {:u}", dbx.Params{"u": s.UserID}); n != 0 {
		t.Errorf("alice's open rows after partial return: want 0, got %d", n)
	}
	if n := countOpenCheckouts(t, app, "user = {:u}", dbx.Params{"u": s.OtherUserID}); n != 1 {
		t.Errorf("bob's open rows: want 1 (untouched), got %d", n)
	}

	// The return line is flagged uncorrelated.
	lines, _ := app.FindRecordsByFilter("transaction_lines", "transaction = {:tx}", "", 0, 0,
		dbx.Params{"tx": result.TransactionID})
	if len(lines) != 1 {
		t.Fatalf("lines: want 1, got %d", len(lines))
	}
	if !lines[0].GetBool("uncorrelated") {
		t.Error("expected uncorrelated=true on the partial-match return line")
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
		// checkout the impact driver (serialized) — instance A
		&cart.Line{
			ItemID: s.ToolSerialID, Action: "checkout", Qty: 1,
			ItemType: "tool", TrackingMode: "serialized", Serial: "SN-A",
			ItemInstanceID: s.ToolSerialInstanceID,
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
		"kiosk.TEST.event.transaction.complete": 1,
		"kiosk.TEST.event.item.return":          1,
		"kiosk.TEST.event.item.checkout":        1,
		"kiosk.TEST.event.item.consume":         1,
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

func TestCheckout_SerializedInstance_WritesInstanceFK(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolSerialID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "serialized",
		ItemInstanceID: s.ToolSerialInstanceID,
	})
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rows, _ := app.FindRecordsByFilter("open_checkouts", "item_instance = {:inst}", "", 0, 0,
		dbx.Params{"inst": s.ToolSerialInstanceID})
	if len(rows) != 1 {
		t.Fatalf("open rows for instance A: want 1, got %d", len(rows))
	}
	if rows[0].GetString("item_instance") != s.ToolSerialInstanceID {
		t.Errorf("item_instance FK not set on open_checkouts row")
	}
}

func TestReturn_SerializedInstance_TargetsOnlyThatInstance(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Check out both instances.
	for _, instID := range []string{s.ToolSerialInstanceID, s.ToolSerialInstance2ID} {
		c := newCart(s.UserID, &cart.Line{
			ItemID: s.ToolSerialID, Action: "checkout", Qty: 1,
			ItemType: "tool", TrackingMode: "serialized",
			ItemInstanceID: instID,
		})
		if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
			t.Fatalf("seed checkout: %v", err)
		}
	}

	// Return only B.
	ret := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolSerialID, Action: "return", Qty: 1,
		ItemType: "tool", TrackingMode: "serialized",
		ItemInstanceID: s.ToolSerialInstance2ID,
	})
	if _, err := commit.Commit(app, ret, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("commit return: %v", err)
	}

	// A still out, B closed.
	if n := countOpenCheckouts(t, app, "item_instance = {:inst}", dbx.Params{"inst": s.ToolSerialInstanceID}); n != 1 {
		t.Errorf("instance A open rows: want 1, got %d", n)
	}
	if n := countOpenCheckouts(t, app, "item_instance = {:inst}", dbx.Params{"inst": s.ToolSerialInstance2ID}); n != 0 {
		t.Errorf("instance B open rows: want 0, got %d", n)
	}
}

// TestReturn_SerializedCrossUser_RejectedForNonForeman is the H3 regression:
// a worker scanning another worker's serialized tool and flipping the line to
// "return" must NOT silently close that checkout. The cart-write paths don't
// set OriginalCheckoutUserID for a plain serialized scan, so before the fix
// the cross-user gate was skipped and commit closed the holder's row by
// instance with no foreman check. commit now resolves the holder server-side.
func TestReturn_SerializedCrossUser_RejectedForNonForeman(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	// Cart user (Alice) is a plain worker — not allowed cross-user returns.
	setUserRoleAndGroup(t, app, s.UserID, "worker", "electrical")

	// Bob checks out a serialized instance.
	bobCheckout := newCart(s.OtherUserID, &cart.Line{
		ItemID: s.ToolSerialID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "serialized",
		ItemInstanceID: s.ToolSerialInstanceID,
	})
	bobCheckout.UserCode = "EMP-2"
	bobCheckout.UserName = "Bob"
	if _, err := commit.Commit(app, bobCheckout, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed bob checkout: %v", err)
	}

	// Alice (worker) returns Bob's instance WITHOUT supplying
	// OriginalCheckoutUserID — exactly the scan→flip-to-return path.
	aliceReturn := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolSerialID, Action: "return", Qty: 1,
		ItemType: "tool", TrackingMode: "serialized",
		ItemInstanceID: s.ToolSerialInstanceID,
	})
	if _, err := commit.Commit(app, aliceReturn, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err == nil {
		t.Fatal("expected error: non-foreman serialized cross-user return must be blocked")
	}

	// Bob's open checkout must survive (transaction rolled back).
	if n := countOpenCheckouts(t, app, "item_instance = {:inst}", dbx.Params{"inst": s.ToolSerialInstanceID}); n != 1 {
		t.Errorf("bob's serialized open row after rejected return: want 1, got %d", n)
	}
}

// TestReturn_SerializedCrossUser_AllowedForForemanSameGroup confirms the gate
// still permits the legitimate case — a foreman in the holder's group closing
// another worker's serialized checkout — and attributes the line to the
// resolved holder even though the client supplied no OriginalCheckoutUserID.
func TestReturn_SerializedCrossUser_AllowedForForemanSameGroup(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	setUserRoleAndGroup(t, app, s.UserID, "foreman", "electrical")
	setUserRoleAndGroup(t, app, s.OtherUserID, "worker", "electrical")

	bobCheckout := newCart(s.OtherUserID, &cart.Line{
		ItemID: s.ToolSerialID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "serialized",
		ItemInstanceID: s.ToolSerialInstanceID,
	})
	bobCheckout.UserCode = "EMP-2"
	bobCheckout.UserName = "Bob"
	if _, err := commit.Commit(app, bobCheckout, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed bob checkout: %v", err)
	}

	aliceReturn := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolSerialID, Action: "return", Qty: 1,
		ItemType: "tool", TrackingMode: "serialized",
		ItemInstanceID: s.ToolSerialInstanceID,
	})
	if _, err := commit.Commit(app, aliceReturn, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("foreman serialized cross-user return should succeed: %v", err)
	}

	if n := countOpenCheckouts(t, app, "item_instance = {:inst}", dbx.Params{"inst": s.ToolSerialInstanceID}); n != 0 {
		t.Errorf("instance open rows after foreman return: want 0, got %d", n)
	}
	// The line must be attributed to the server-resolved holder (Bob).
	line, err := app.FindFirstRecordByFilter("transaction_lines",
		"item_instance = {:inst} && action = 'return'", dbx.Params{"inst": s.ToolSerialInstanceID})
	if err != nil {
		t.Fatalf("find return line: %v", err)
	}
	if line.GetString("original_checkout_user") != s.OtherUserID {
		t.Errorf("return line original_checkout_user: want Bob (%s), got %q",
			s.OtherUserID, line.GetString("original_checkout_user"))
	}
}

func TestCheckout_SerializedItem_MissingInstance_Rejected(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolSerialID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "serialized",
		// no ItemInstanceID
	})
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err == nil {
		t.Fatal("expected error for serialized line without instance, got nil")
	}
}

func TestCheckout_SerializedInstance_WrongItem_Rejected(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)

	// Pretend the line points instance A at the wrong (quantity) item.
	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "serialized",
		ItemInstanceID: s.ToolSerialInstanceID,
	})
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err == nil {
		t.Fatal("expected error for instance/item mismatch, got nil")
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

// setUserRoleAndGroup is a test helper that flips a seeded user's role/group
// for the role+group rules below. Direct DAO update; bypasses collection rules.
// groupCode is the human-readable code; an empty string clears the FK (used
// to test the "ungrouped foreman" case).
func setUserRoleAndGroup(t *testing.T, app core.App, userID, role, groupCode string) {
	t.Helper()
	rec, err := app.FindRecordById("users", userID)
	if err != nil {
		t.Fatalf("find user %s: %v", userID, err)
	}
	rec.Set("role", role)
	if groupCode == "" {
		rec.Set("group", "")
	} else {
		rec.Set("group", ensureGroup(t, app, groupCode))
	}
	if err := app.Save(rec); err != nil {
		t.Fatalf("save user %s: %v", userID, err)
	}
}

// ensureGroup returns the id of a groups row with the given code, creating
// the row if needed. Lets tests use human-readable code strings while the
// underlying field is a relation FK.
func ensureGroup(t *testing.T, app core.App, code string) string {
	t.Helper()
	if existing, err := app.FindFirstRecordByFilter("groups", "code = {:code}", dbx.Params{"code": code}); err == nil {
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

func TestCrossUserReturn_RejectedWhenCartUserIsNotForeman(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	// Demote Alice to a plain worker; group stays "electrical".
	setUserRoleAndGroup(t, app, s.UserID, "worker", "electrical")

	// Bob (worker, electrical) checks out the tool.
	bobCheckout := newCart(s.OtherUserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	bobCheckout.UserCode = "EMP-2"
	bobCheckout.UserName = "Bob"
	if _, err := commit.Commit(app, bobCheckout, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed bob checkout: %v", err)
	}

	// Alice (worker) attempts cross-user return — should be blocked regardless
	// of the permissive policy because she isn't a foreman.
	aliceReturn := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "return", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
		OriginalCheckoutUserID: s.OtherUserID,
	})
	if _, err := commit.Commit(app, aliceReturn, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err == nil {
		t.Fatal("expected error: non-foreman cross-user return must be blocked")
	}

	// Bob's open checkout should still be there (transaction rolled back).
	if n := countOpenCheckouts(t, app, "user = {:u}", dbx.Params{"u": s.OtherUserID}); n != 1 {
		t.Errorf("bob's open rows after rejected return: want 1, got %d", n)
	}
}

func TestCrossUserReturn_RejectedWhenForemanHasNoGroup(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	// Strip Alice's group while leaving her as a foreman. Ungrouped foremen
	// can't act for anyone — strictest interpretation.
	setUserRoleAndGroup(t, app, s.UserID, "foreman", "")

	bobCheckout := newCart(s.OtherUserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	bobCheckout.UserCode = "EMP-2"
	bobCheckout.UserName = "Bob"
	if _, err := commit.Commit(app, bobCheckout, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed bob checkout: %v", err)
	}

	aliceReturn := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "return", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
		OriginalCheckoutUserID: s.OtherUserID,
	})
	if _, err := commit.Commit(app, aliceReturn, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err == nil {
		t.Fatal("expected error: ungrouped foreman cross-user return must be blocked")
	}
}

func TestCrossUserReturn_RejectedWhenGroupsDiffer(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	// Put Bob in a different group than Alice.
	setUserRoleAndGroup(t, app, s.OtherUserID, "worker", "hvac")

	bobCheckout := newCart(s.OtherUserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	bobCheckout.UserCode = "EMP-2"
	bobCheckout.UserName = "Bob"
	if _, err := commit.Commit(app, bobCheckout, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed bob checkout: %v", err)
	}

	// Alice (foreman/electrical) tries to return Bob's (worker/hvac) tool.
	aliceReturn := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "return", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
		OriginalCheckoutUserID: s.OtherUserID,
	})
	if _, err := commit.Commit(app, aliceReturn, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err == nil {
		t.Fatal("expected error: cross-group return must be blocked")
	}
}

func TestUncorrelatedReturn_RejectedWhenCartUserIsNotForeman(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	// Demote Alice; uncorrelated returns are a janitorial action reserved
	// for foremen regardless of group.
	setUserRoleAndGroup(t, app, s.UserID, "worker", "electrical")

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "return", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err == nil {
		t.Fatal("expected error: non-foreman uncorrelated return must be blocked")
	}
}

func TestCommit_StampsUserGroupOnTransaction(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	// Alice (foreman, electrical) does a plain self-checkout.

	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, Action: "checkout", Qty: 1,
		ItemType: "tool", TrackingMode: "quantity",
	})
	result, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	rec, err := app.FindRecordById("transactions", result.TransactionID)
	if err != nil {
		t.Fatalf("find transaction: %v", err)
	}
	if got := rec.GetString("user_group"); got != "electrical" {
		t.Errorf("user_group snapshot: want %q, got %q", "electrical", got)
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
