package metrics

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	_ "github.com/skeeeon/kiosk/migrations"
)

// setupApp mirrors the pattern in internal/commit/commit_test.go: boot a PB in
// a temp dir and apply migrations explicitly (Automigrate hooks OnServe, which
// these no-server tests never reach).
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

func seedUser(t *testing.T, app core.App, code, name string) string {
	t.Helper()
	col, _ := app.FindCollectionByNameOrId("users")
	u := core.NewRecord(col)
	u.Set("code", code)
	u.Set("name", name)
	u.Set("email", code+"@example.com")
	u.Set("role", "worker")
	u.Set("active", true)
	u.SetPassword("not-used")
	if err := app.Save(u); err != nil {
		t.Fatalf("save user %s: %v", code, err)
	}
	return u.Id
}

func seedItem(t *testing.T, app core.App, code string, onHand, threshold int) string {
	t.Helper()
	col, _ := app.FindCollectionByNameOrId("items")
	it := core.NewRecord(col)
	it.Set("code", code)
	it.Set("name", code)
	it.Set("type", "tool")
	it.Set("tracking_mode", "quantity")
	it.Set("active", true)
	it.Set("quantity_on_hand", onHand)
	it.Set("reorder_threshold", threshold)
	if err := app.Save(it); err != nil {
		t.Fatalf("save item %s: %v", code, err)
	}
	return it.Id
}

func seedTransaction(t *testing.T, app core.App, userID string, completedAt time.Time) string {
	t.Helper()
	col, _ := app.FindCollectionByNameOrId("transactions")
	tx := core.NewRecord(col)
	tx.Set("kiosk_code", "KIOSK-A")
	tx.Set("location_code", "WEST")
	tx.Set("user", userID)
	tx.Set("completed_at", completedAt)
	tx.Set("status", "completed")
	tx.Set("lines_count", 1)
	if err := app.Save(tx); err != nil {
		t.Fatalf("save tx: %v", err)
	}
	return tx.Id
}

// seedOpenCheckout builds the minimal valid chain (transaction →
// transaction_line → open_checkouts) so the open_checkouts row satisfies its
// required relations.
func seedOpenCheckout(t *testing.T, app core.App, userID, itemID string) {
	t.Helper()
	txID := seedTransaction(t, app, userID, time.Now().UTC())

	lineCol, _ := app.FindCollectionByNameOrId("transaction_lines")
	line := core.NewRecord(lineCol)
	line.Set("transaction", txID)
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

func TestCompute(t *testing.T) {
	app := setupApp(t)

	alice := seedUser(t, app, "ALICE", "Alice")
	bob := seedUser(t, app, "BOB", "Bob")

	// A tool that's checked out (not low) and a separate low-stock SKU.
	drill := seedItem(t, app, "DRILL", 10, 0) // threshold 0 → never low
	seedItem(t, app, "BIT", 1, 5)             // on_hand 1 ≤ threshold 5 → low

	// 2 open for Alice, 1 for Bob → items_out=3, distinct holders=2.
	seedOpenCheckout(t, app, alice, drill)
	seedOpenCheckout(t, app, alice, drill)
	seedOpenCheckout(t, app, bob, drill)

	// Transactions: the 3 above land "now" (today + this week). seedOpenCheckout
	// creates one each, so far 3. Add one 10 days ago — outside both windows.
	seedTransaction(t, app, alice, time.Now().UTC().AddDate(0, 0, -10))

	op := Operational{UptimeSeconds: 42, NATSConnected: true, ActiveCarts: 2}
	snap, err := Compute(app, op, "KIOSK-A")
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	if snap.KioskCode != "KIOSK-A" {
		t.Errorf("kiosk_code = %q, want KIOSK-A", snap.KioskCode)
	}
	if snap.GeneratedAt == "" {
		t.Error("generated_at is empty")
	}
	// Operational is passed through verbatim.
	if snap.Operational != op {
		t.Errorf("operational = %+v, want %+v", snap.Operational, op)
	}

	if snap.Ledger.ItemsOut != 3 {
		t.Errorf("items_out = %d, want 3", snap.Ledger.ItemsOut)
	}
	if snap.Ledger.UsersWithItemsOut != 2 {
		t.Errorf("users_with_items_out = %d, want 2", snap.Ledger.UsersWithItemsOut)
	}
	if snap.Ledger.LowStockSKUs != 1 {
		t.Errorf("low_stock_skus = %d, want 1", snap.Ledger.LowStockSKUs)
	}
	// 3 "now" transactions from the open checkouts; the 10-day-old is excluded.
	if snap.Ledger.TransactionsToday != 3 {
		t.Errorf("transactions_today = %d, want 3", snap.Ledger.TransactionsToday)
	}
	if snap.Ledger.TransactionsWeek != 3 {
		t.Errorf("transactions_week = %d, want 3", snap.Ledger.TransactionsWeek)
	}
}
