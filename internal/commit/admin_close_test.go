package commit_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/commit"
	"github.com/skeeeon/kiosk/internal/events"
)

// ensureAdmin creates an admins record once for AdminClose tests and returns
// its id. Kept local to this file so the broader fixture setup stays focused
// on the worker-driven commit path.
func ensureAdmin(t *testing.T, app core.App, email string) string {
	t.Helper()
	admins, err := app.FindCollectionByNameOrId("admins")
	if err != nil {
		t.Fatalf("find admins: %v", err)
	}
	rec := core.NewRecord(admins)
	rec.Set("email", email)
	rec.Set("name", "Test Admin")
	rec.Set("active", true)
	rec.SetPassword("test-admin-password-123")
	if err := app.Save(rec); err != nil {
		t.Fatalf("save admin: %v", err)
	}
	return rec.Id
}

// seedOpenCheckout puts one row in open_checkouts via the regular commit
// path so AdminClose has something to close. Returns the open_checkout id.
func seedOpenCheckout(t *testing.T, app core.App, s seed) string {
	t.Helper()
	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolQtyID, ItemCode: "HAMMER", ItemName: "Hammer",
		ItemType: "tool", TrackingMode: "quantity",
		Action: "checkout", Qty: 1,
	})
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed checkout: %v", err)
	}
	rows, err := app.FindRecordsByFilter("open_checkouts",
		"user = {:u}", "", 0, 0, dbx.Params{"u": s.UserID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("seed open_checkouts: rows=%d err=%v", len(rows), err)
	}
	return rows[0].Id
}

// seedSerializedOpenCheckout puts a serialized open_checkouts row in via
// commit, returning (open_checkout_id, instance_id).
func seedSerializedOpenCheckout(t *testing.T, app core.App, s seed) (string, string) {
	t.Helper()
	c := newCart(s.UserID, &cart.Line{
		ItemID: s.ToolSerialID, ItemCode: "DR-042-A", ItemName: "Impact Driver",
		ItemType: "tool", TrackingMode: "serialized", Serial: "SN-A",
		ItemInstanceID: s.ToolSerialInstanceID,
		Action:         "checkout", Qty: 1,
	})
	if _, err := commit.Commit(app, c, testIdentity, commit.DefaultPolicy(), (&captured{}).publish); err != nil {
		t.Fatalf("seed serialized checkout: %v", err)
	}
	rows, err := app.FindRecordsByFilter("open_checkouts",
		"item_instance = {:i}", "", 0, 0,
		dbx.Params{"i": s.ToolSerialInstanceID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("seed open_checkouts (serialized): rows=%d err=%v", len(rows), err)
	}
	return rows[0].Id, s.ToolSerialInstanceID
}

func TestAdminClose_NonSerialized_DeletesOpenRowAndWritesLedger(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	adminID := ensureAdmin(t, app, "admin1@test.local")
	openID := seedOpenCheckout(t, app, s)

	pub := &captured{}
	// returned_offline is the "no inventory side-effect" reason — keeps
	// this test focused on the core close semantics. The lost/damaged
	// side-effects are exercised in TestAdminClose_LostOrDamaged_*.
	result, err := commit.AdminClose(app, commit.AdminCloseInput{
		OpenCheckoutID: openID,
		ActorID:        adminID,
		Source:         events.SourceLocal,
		Reason:         "returned_offline",
		Notes:          "worker brought it back",
		Identity:       testIdentity,
	}, pub.publish)
	if err != nil {
		t.Fatalf("AdminClose: %v", err)
	}
	if result.TransactionID == "" || result.LineID == "" {
		t.Errorf("result missing ids: %+v", result)
	}
	if result.ClosureReason != "returned_offline" {
		t.Errorf("closure_reason: want returned_offline, got %q", result.ClosureReason)
	}

	if n := countOpenCheckouts(t, app, "", nil); n != 0 {
		t.Errorf("open_checkouts after close: want 0, got %d", n)
	}

	tx, err := app.FindRecordById("transactions", result.TransactionID)
	if err != nil {
		t.Fatalf("find transaction: %v", err)
	}
	if tx.GetString("closed_by_admin") != adminID {
		t.Errorf("transactions.closed_by_admin: want %q, got %q", adminID, tx.GetString("closed_by_admin"))
	}
	if tx.GetString("user") != s.UserID {
		t.Errorf("transactions.user: want affected worker %q, got %q", s.UserID, tx.GetString("user"))
	}
	if tx.GetInt("lines_count") != 1 {
		t.Errorf("transactions.lines_count: want 1, got %d", tx.GetInt("lines_count"))
	}

	line, err := app.FindRecordById("transaction_lines", result.LineID)
	if err != nil {
		t.Fatalf("find line: %v", err)
	}
	if line.GetString("action") != "admin_close" {
		t.Errorf("line.action: want admin_close, got %q", line.GetString("action"))
	}
	if line.GetString("closed_by_admin") != adminID {
		t.Errorf("line.closed_by_admin: want %q, got %q", adminID, line.GetString("closed_by_admin"))
	}
	if line.GetString("closure_reason") != "returned_offline" {
		t.Errorf("line.closure_reason: want returned_offline, got %q", line.GetString("closure_reason"))
	}
	if line.GetString("original_checkout_user") != s.UserID {
		t.Errorf("line.original_checkout_user: want %q, got %q", s.UserID, line.GetString("original_checkout_user"))
	}
	if line.GetString("notes") != "worker brought it back" {
		t.Errorf("line.notes: want %q, got %q", "worker brought it back", line.GetString("notes"))
	}

	// Exactly one admin_close event published, on the right subject.
	if len(pub.events) != 1 {
		t.Fatalf("event count: want 1, got %d (subjects=%v)", len(pub.events), pub.subjects())
	}
	if !strings.HasSuffix(pub.events[0].Subject, ".checkout.admin_close") {
		t.Errorf("event subject: want suffix .checkout.admin_close, got %q", pub.events[0].Subject)
	}
}

func TestAdminClose_Serialized_PreservesInstanceFK(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	adminID := ensureAdmin(t, app, "admin2@test.local")
	openID, instanceID := seedSerializedOpenCheckout(t, app, s)

	result, err := commit.AdminClose(app, commit.AdminCloseInput{
		OpenCheckoutID: openID,
		ActorID:        adminID,
		Source:         events.SourceLocal,
		Reason:         "damaged",
		Identity:       testIdentity,
	}, (&captured{}).publish)
	if err != nil {
		t.Fatalf("AdminClose: %v", err)
	}

	line, err := app.FindRecordById("transaction_lines", result.LineID)
	if err != nil {
		t.Fatalf("find line: %v", err)
	}
	if got := line.GetString("item_instance"); got != instanceID {
		t.Errorf("line.item_instance: want %q, got %q", instanceID, got)
	}
	if got := line.GetString("serial"); got != "SN-A" {
		t.Errorf("line.serial: want SN-A, got %q", got)
	}
	if n := countOpenCheckouts(t, app, "item_instance = {:i}", dbx.Params{"i": instanceID}); n != 0 {
		t.Errorf("open_checkouts for instance after close: want 0, got %d", n)
	}
}

func TestAdminClose_IdempotentReplay_ReturnsPriorResult(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	adminID := ensureAdmin(t, app, "admin3@test.local")
	openID := seedOpenCheckout(t, app, s)

	in := commit.AdminCloseInput{
		OpenCheckoutID: openID,
		ActorID:        adminID,
		Source:         events.SourceController,
		CommandID:      "cmd-fixed-uuid",
		Reason:         "returned_offline",
		Identity:       testIdentity,
	}

	first, err := commit.AdminClose(app, in, (&captured{}).publish)
	if err != nil {
		t.Fatalf("first AdminClose: %v", err)
	}

	second, err := commit.AdminClose(app, in, (&captured{}).publish)
	if err != nil {
		t.Fatalf("replay AdminClose: %v", err)
	}

	if first.TransactionID != second.TransactionID {
		t.Errorf("replay must return same transaction; first=%q second=%q",
			first.TransactionID, second.TransactionID)
	}
	// Only one transaction row exists for this command_id.
	rows, err := app.FindRecordsByFilter("transactions",
		"command_id = {:c}", "", 0, 0, dbx.Params{"c": "cmd-fixed-uuid"})
	if err != nil {
		t.Fatalf("list transactions: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("transactions for command_id: want 1, got %d", len(rows))
	}
}

func TestAdminClose_ConcurrentReplay_OneTransactionRow(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	adminID := ensureAdmin(t, app, "admin4@test.local")
	openID := seedOpenCheckout(t, app, s)

	in := commit.AdminCloseInput{
		OpenCheckoutID: openID,
		ActorID:        adminID,
		Source:         events.SourceController,
		CommandID:      "cmd-race",
		Reason:         "other",
		Identity:       testIdentity,
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := commit.AdminClose(app, in, (&captured{}).publish)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	// Both calls must succeed (one as a real write, one as replay).
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	rows, err := app.FindRecordsByFilter("transactions",
		"command_id = {:c}", "", 0, 0, dbx.Params{"c": "cmd-race"})
	if err != nil {
		t.Fatalf("list transactions: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("transactions for command_id under race: want 1, got %d", len(rows))
	}
}

func TestAdminClose_InvalidClosureReason_Rejected(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	adminID := ensureAdmin(t, app, "admin5@test.local")
	openID := seedOpenCheckout(t, app, s)

	_, err := commit.AdminClose(app, commit.AdminCloseInput{
		OpenCheckoutID: openID,
		ActorID:        adminID,
		Source:         events.SourceLocal,
		Reason:         "not-a-valid-reason",
		Identity:       testIdentity,
	}, (&captured{}).publish)
	if err == nil {
		t.Fatal("expected validation error for unknown closure_reason")
	}
	if n := countOpenCheckouts(t, app, "", nil); n != 1 {
		t.Errorf("open_checkouts on validation failure: want 1 (unchanged), got %d", n)
	}
}

func TestAdminClose_MissingOpenCheckout_Errors(t *testing.T) {
	app := setupApp(t)
	_ = seedFixtures(t, app)
	adminID := ensureAdmin(t, app, "admin6@test.local")

	_, err := commit.AdminClose(app, commit.AdminCloseInput{
		OpenCheckoutID: "nonexistent-id",
		ActorID:        adminID,
		Source:         events.SourceLocal,
		Reason:         "lost",
		Identity:       testIdentity,
	}, (&captured{}).publish)
	if err == nil {
		t.Fatal("expected error for missing open_checkout")
	}
}

func TestAdminClose_ControllerSource_NoLocalAdminFK(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	openID := seedOpenCheckout(t, app, s)

	// Controller-source closes pass the controller admin's PB id, which is
	// NOT a record in the kiosk's admins collection. The local admin FK
	// must stay null; the actor lives in the event payload's
	// controller_admin_id field instead. Using returned_offline keeps this
	// test focused on actor wiring — lost/damaged paths add inventory
	// effects that are covered separately.
	pub := &captured{}
	result, err := commit.AdminClose(app, commit.AdminCloseInput{
		OpenCheckoutID: openID,
		ActorID:        "controller-admin-id-not-in-kiosk-db",
		Source:         events.SourceController,
		CommandID:      "cmd-ctrl-1",
		Reason:         "returned_offline",
		Identity:       testIdentity,
	}, pub.publish)
	if err != nil {
		t.Fatalf("AdminClose: %v", err)
	}
	tx, err := app.FindRecordById("transactions", result.TransactionID)
	if err != nil {
		t.Fatalf("find transaction: %v", err)
	}
	if tx.GetString("closed_by_admin") != "" {
		t.Errorf("controller source must leave local admin FK blank, got %q",
			tx.GetString("closed_by_admin"))
	}
	if tx.GetString("command_id") != "cmd-ctrl-1" {
		t.Errorf("transactions.command_id: want cmd-ctrl-1, got %q", tx.GetString("command_id"))
	}

	if len(pub.events) != 1 {
		t.Fatalf("expected one event")
	}
	payload, ok := pub.events[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type: %T", pub.events[0].Payload)
	}
	if payload["controller_admin_id"] != "controller-admin-id-not-in-kiosk-db" {
		t.Errorf("payload.controller_admin_id: %v", payload["controller_admin_id"])
	}
	if payload["admin_id"] != "" {
		t.Errorf("payload.admin_id must be empty for controller source, got %v", payload["admin_id"])
	}
}

// setItemQty sets quantity_on_hand on a fixture item so the decrement
// tests can verify the before/after values. The seed leaves it at the
// PB default (0).
func setItemQty(t *testing.T, app core.App, itemID string, qty int) {
	t.Helper()
	rec, err := app.FindRecordById("items", itemID)
	if err != nil {
		t.Fatalf("find item: %v", err)
	}
	rec.Set("quantity_on_hand", qty)
	if err := app.Save(rec); err != nil {
		t.Fatalf("set quantity_on_hand: %v", err)
	}
}

// findStockAdjustmentsForItem returns every stock_adjustments row for the
// supplied item id, oldest first. Used by the lost/damaged side-effect
// tests to confirm an audit row landed.
func findStockAdjustmentsForItem(t *testing.T, app core.App, itemID string) []*core.Record {
	t.Helper()
	rows, err := app.FindRecordsByFilter("stock_adjustments",
		"item = {:i}", "created", 0, 0, dbx.Params{"i": itemID})
	if err != nil {
		t.Fatalf("list stock_adjustments: %v", err)
	}
	return rows
}

func TestAdminClose_Lost_NonSerialized_DecrementsQtyAndAudits(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	setItemQty(t, app, s.ToolQtyID, 10)
	adminID := ensureAdmin(t, app, "loss-1@test.local")
	openID := seedOpenCheckout(t, app, s)

	pub := &captured{}
	_, err := commit.AdminClose(app, commit.AdminCloseInput{
		OpenCheckoutID: openID,
		ActorID:        adminID,
		Source:         events.SourceLocal,
		Reason:         "lost",
		Notes:          "left on jobsite",
		Identity:       testIdentity,
	}, pub.publish)
	if err != nil {
		t.Fatalf("AdminClose: %v", err)
	}

	// quantity_on_hand drops by 1.
	itemRec, _ := app.FindRecordById("items", s.ToolQtyID)
	if got := itemRec.GetInt("quantity_on_hand"); got != 9 {
		t.Errorf("quantity_on_hand: want 9, got %d", got)
	}

	// stock_adjustments row exists with the close-derived reason text.
	adjs := findStockAdjustmentsForItem(t, app, s.ToolQtyID)
	if len(adjs) != 1 {
		t.Fatalf("stock_adjustments rows: want 1, got %d", len(adjs))
	}
	adj := adjs[0]
	if adj.GetInt("delta") != -1 {
		t.Errorf("stock_adjustments.delta: want -1, got %d", adj.GetInt("delta"))
	}
	if adj.GetInt("new_quantity") != 9 {
		t.Errorf("stock_adjustments.new_quantity: want 9, got %d", adj.GetInt("new_quantity"))
	}
	if got := adj.GetString("reason"); got != "admin_close: lost" {
		t.Errorf("stock_adjustments.reason: want %q, got %q", "admin_close: lost", got)
	}
	if adj.GetString("admin") != adminID {
		t.Errorf("stock_adjustments.admin: want %q, got %q", adminID, adj.GetString("admin"))
	}
	if adj.GetString("source") != "local" {
		t.Errorf("stock_adjustments.source: want local, got %q", adj.GetString("source"))
	}

	// Two events fired: the close + the inventory.adjust. Order is
	// close-first so downstream consumers can correlate via the same
	// item/kiosk.
	if len(pub.events) != 2 {
		t.Fatalf("events: want 2, got %d (subjects=%v)", len(pub.events), pub.subjects())
	}
	if !strings.HasSuffix(pub.events[0].Subject, ".checkout.admin_close") {
		t.Errorf("first subject: want .checkout.admin_close, got %q", pub.events[0].Subject)
	}
	if !strings.HasSuffix(pub.events[1].Subject, ".inventory.adjust") {
		t.Errorf("second subject: want .inventory.adjust, got %q", pub.events[1].Subject)
	}
	invPayload, _ := pub.events[1].Payload.(map[string]any)
	if invPayload["delta"] != -1 {
		t.Errorf("inventory.adjust payload.delta: %v", invPayload["delta"])
	}
	if invPayload["prev_quantity"] != 10 || invPayload["new_quantity"] != 9 {
		t.Errorf("inventory.adjust payload.prev=%v new=%v (want 10, 9)",
			invPayload["prev_quantity"], invPayload["new_quantity"])
	}
}

func TestAdminClose_Damaged_BehavesLikeLost(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	setItemQty(t, app, s.ToolQtyID, 5)
	adminID := ensureAdmin(t, app, "damaged-1@test.local")
	openID := seedOpenCheckout(t, app, s)

	pub := &captured{}
	_, err := commit.AdminClose(app, commit.AdminCloseInput{
		OpenCheckoutID: openID,
		ActorID:        adminID,
		Source:         events.SourceLocal,
		Reason:         "damaged",
		Identity:       testIdentity,
	}, pub.publish)
	if err != nil {
		t.Fatalf("AdminClose: %v", err)
	}

	itemRec, _ := app.FindRecordById("items", s.ToolQtyID)
	if got := itemRec.GetInt("quantity_on_hand"); got != 4 {
		t.Errorf("quantity_on_hand: want 4, got %d", got)
	}
	adjs := findStockAdjustmentsForItem(t, app, s.ToolQtyID)
	if len(adjs) != 1 {
		t.Fatalf("stock_adjustments: want 1, got %d", len(adjs))
	}
	if got := adjs[0].GetString("reason"); got != "admin_close: damaged" {
		t.Errorf("reason: want admin_close: damaged, got %q", got)
	}
}

func TestAdminClose_ReturnedOffline_LeavesInventoryAlone(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	setItemQty(t, app, s.ToolQtyID, 7)
	adminID := ensureAdmin(t, app, "ret-off-1@test.local")
	openID := seedOpenCheckout(t, app, s)

	pub := &captured{}
	_, err := commit.AdminClose(app, commit.AdminCloseInput{
		OpenCheckoutID: openID,
		ActorID:        adminID,
		Source:         events.SourceLocal,
		Reason:         "returned_offline",
		Identity:       testIdentity,
	}, pub.publish)
	if err != nil {
		t.Fatalf("AdminClose: %v", err)
	}

	itemRec, _ := app.FindRecordById("items", s.ToolQtyID)
	if got := itemRec.GetInt("quantity_on_hand"); got != 7 {
		t.Errorf("quantity_on_hand: want 7 (unchanged), got %d", got)
	}
	if rows := findStockAdjustmentsForItem(t, app, s.ToolQtyID); len(rows) != 0 {
		t.Errorf("stock_adjustments: want 0 (none for returned_offline), got %d", len(rows))
	}
	if len(pub.events) != 1 {
		t.Errorf("events: want 1 (close only), got %d", len(pub.events))
	}
}

func TestAdminClose_Other_LeavesInventoryAlone(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	setItemQty(t, app, s.ToolQtyID, 3)
	adminID := ensureAdmin(t, app, "other-1@test.local")
	openID := seedOpenCheckout(t, app, s)

	_, err := commit.AdminClose(app, commit.AdminCloseInput{
		OpenCheckoutID: openID,
		ActorID:        adminID,
		Source:         events.SourceLocal,
		Reason:         "other",
		Identity:       testIdentity,
	}, (&captured{}).publish)
	if err != nil {
		t.Fatalf("AdminClose: %v", err)
	}

	itemRec, _ := app.FindRecordById("items", s.ToolQtyID)
	if got := itemRec.GetInt("quantity_on_hand"); got != 3 {
		t.Errorf("quantity_on_hand: want 3 (unchanged for 'other'), got %d", got)
	}
	if rows := findStockAdjustmentsForItem(t, app, s.ToolQtyID); len(rows) != 0 {
		t.Errorf("stock_adjustments: want 0 for 'other', got %d", len(rows))
	}
}

func TestAdminClose_Lost_Serialized_DecommissionsInstance(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	setItemQty(t, app, s.ToolSerialID, 2)
	adminID := ensureAdmin(t, app, "loss-ser-1@test.local")
	openID, instanceID := seedSerializedOpenCheckout(t, app, s)

	pub := &captured{}
	_, err := commit.AdminClose(app, commit.AdminCloseInput{
		OpenCheckoutID: openID,
		ActorID:        adminID,
		Source:         events.SourceLocal,
		Reason:         "lost",
		Identity:       testIdentity,
	}, pub.publish)
	if err != nil {
		t.Fatalf("AdminClose: %v", err)
	}

	// Instance is flipped to active=false.
	inst, err := app.FindRecordById("item_instances", instanceID)
	if err != nil {
		t.Fatalf("find instance: %v", err)
	}
	if inst.GetBool("active") {
		t.Errorf("instance.active: want false, got true (lost must decommission)")
	}

	// instance_audit row with action=decommission exists.
	auditRows, err := app.FindRecordsByFilter("instance_audit",
		"item_instance = {:i} && action = 'decommission'",
		"", 0, 0, dbx.Params{"i": instanceID})
	if err != nil || len(auditRows) != 1 {
		t.Fatalf("instance_audit rows: len=%d err=%v", len(auditRows), err)
	}
	if got := auditRows[0].GetString("reason"); got != "admin_close: lost" {
		t.Errorf("instance_audit.reason: want %q, got %q", "admin_close: lost", got)
	}
	if got := auditRows[0].GetBool("prev_active"); got != true {
		t.Errorf("instance_audit.prev_active: want true, got %v", got)
	}

	// Three events: close + inventory.adjust + instance.lifecycle.
	if len(pub.events) != 3 {
		t.Fatalf("events: want 3, got %d (subjects=%v)", len(pub.events), pub.subjects())
	}
	wantSuffixes := []string{".checkout.admin_close", ".inventory.adjust", ".instance.lifecycle"}
	for i, want := range wantSuffixes {
		if !strings.HasSuffix(pub.events[i].Subject, want) {
			t.Errorf("events[%d]: want suffix %q, got %q", i, want, pub.events[i].Subject)
		}
	}
}

func TestAdminClose_IdempotentReplay_DoesNotDoubleDecrement(t *testing.T) {
	app := setupApp(t)
	s := seedFixtures(t, app)
	setItemQty(t, app, s.ToolQtyID, 10)
	adminID := ensureAdmin(t, app, "idem-lost-1@test.local")
	openID := seedOpenCheckout(t, app, s)

	in := commit.AdminCloseInput{
		OpenCheckoutID: openID,
		ActorID:        adminID,
		Source:         events.SourceController,
		CommandID:      "cmd-lost-idempotent",
		Reason:         "lost",
		Identity:       testIdentity,
	}
	if _, err := commit.AdminClose(app, in, (&captured{}).publish); err != nil {
		t.Fatalf("first AdminClose: %v", err)
	}
	if _, err := commit.AdminClose(app, in, (&captured{}).publish); err != nil {
		t.Fatalf("replay AdminClose: %v", err)
	}

	itemRec, _ := app.FindRecordById("items", s.ToolQtyID)
	if got := itemRec.GetInt("quantity_on_hand"); got != 9 {
		t.Errorf("quantity_on_hand after replay: want 9 (decrement once), got %d", got)
	}
	adjs := findStockAdjustmentsForItem(t, app, s.ToolQtyID)
	if len(adjs) != 1 {
		t.Errorf("stock_adjustments: want 1 (no double-write on replay), got %d", len(adjs))
	}
}
