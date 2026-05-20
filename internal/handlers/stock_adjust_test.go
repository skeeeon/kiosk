package handlers_test

import (
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/handlers"

	// Register migrations so the runner picks them up.
	_ "github.com/skeeeon/kiosk/migrations"
)

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

type stockSeed struct {
	ItemID  string
	AdminID string
}

func seedItemAndAdmin(t *testing.T, app core.App, startQty int) stockSeed {
	t.Helper()

	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	item := core.NewRecord(items)
	item.Set("code", "NITRILE-M")
	item.Set("name", "Nitrile Gloves M")
	item.Set("type", "consumable")
	item.Set("tracking_mode", "quantity")
	item.Set("active", true)
	item.Set("quantity_on_hand", startQty)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}

	admins, err := app.FindCollectionByNameOrId("admins")
	if err != nil {
		t.Fatalf("find admins: %v", err)
	}
	// The init migration already seeded admin@kiosk.local — just reuse it
	// instead of creating a second admin.
	bootstrap, err := app.FindFirstRecordByFilter("admins", "email = {:e}",
		dbx.Params{"e": "admin@kiosk.local"})
	if err != nil {
		t.Fatalf("find bootstrap admin: %v", err)
	}
	_ = admins // not used directly; just confirming the collection exists

	return stockSeed{ItemID: item.Id, AdminID: bootstrap.Id}
}

func latestAdjustmentFor(t *testing.T, app core.App, itemID string) *core.Record {
	t.Helper()
	rows, err := app.FindRecordsByFilter("stock_adjustments",
		"item = {:i}", "-created", 1, 0,
		dbx.Params{"i": itemID})
	if err != nil {
		t.Fatalf("find adjustments: %v", err)
	}
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

func TestPerformStockAdjustment_Delta(t *testing.T) {
	app := setupApp(t)
	s := seedItemAndAdmin(t, app, 10)

	r, err := handlers.PerformStockAdjustment(app, s.ItemID, s.AdminID, "delta", 5, "restock from PO-42")
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if r.Delta != 5 || r.NewQuantity != 15 || r.PrevQuantity != 10 {
		t.Errorf("result: got %+v, want delta=5 new=15 prev=10", r)
	}

	item, _ := app.FindRecordById("items", s.ItemID)
	if got := item.GetInt("quantity_on_hand"); got != 15 {
		t.Errorf("item qty after delta+5: want 15, got %d", got)
	}

	adj := latestAdjustmentFor(t, app, s.ItemID)
	if adj == nil {
		t.Fatal("no adjustment row written")
	}
	if adj.GetInt("delta") != 5 || adj.GetInt("new_quantity") != 15 {
		t.Errorf("audit row: delta=%d new=%d, want 5/15", adj.GetInt("delta"), adj.GetInt("new_quantity"))
	}
	if adj.GetString("reason") != "restock from PO-42" {
		t.Errorf("reason not preserved: %q", adj.GetString("reason"))
	}
	if adj.GetString("admin") != s.AdminID {
		t.Errorf("admin FK: want %s, got %s", s.AdminID, adj.GetString("admin"))
	}
}

func TestPerformStockAdjustment_Absolute(t *testing.T) {
	app := setupApp(t)
	s := seedItemAndAdmin(t, app, 12)

	r, err := handlers.PerformStockAdjustment(app, s.ItemID, s.AdminID, "absolute", 50, "physical count")
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if r.Delta != 38 || r.NewQuantity != 50 || r.PrevQuantity != 12 {
		t.Errorf("absolute result: got %+v, want delta=38 new=50 prev=12", r)
	}

	item, _ := app.FindRecordById("items", s.ItemID)
	if got := item.GetInt("quantity_on_hand"); got != 50 {
		t.Errorf("item qty after set-to-50: want 50, got %d", got)
	}
	adj := latestAdjustmentFor(t, app, s.ItemID)
	if adj.GetInt("delta") != 38 {
		t.Errorf("audit delta after absolute: want 38, got %d", adj.GetInt("delta"))
	}
}

func TestPerformStockAdjustment_DownToNegative_Allowed(t *testing.T) {
	app := setupApp(t)
	s := seedItemAndAdmin(t, app, 2)

	_, err := handlers.PerformStockAdjustment(app, s.ItemID, s.AdminID, "delta", -5, "found broken box")
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	item, _ := app.FindRecordById("items", s.ItemID)
	if got := item.GetInt("quantity_on_hand"); got != -3 {
		t.Errorf("qty after delta -5 from 2: want -3, got %d", got)
	}
}

func TestPerformStockAdjustment_EmptyReason_Rejected(t *testing.T) {
	app := setupApp(t)
	s := seedItemAndAdmin(t, app, 10)

	_, err := handlers.PerformStockAdjustment(app, s.ItemID, s.AdminID, "delta", 1, "")
	if err == nil {
		t.Fatal("expected error for empty reason")
	}
}

func TestPerformStockAdjustment_ItemNotFound(t *testing.T) {
	app := setupApp(t)
	s := seedItemAndAdmin(t, app, 10)

	_, err := handlers.PerformStockAdjustment(app, "no-such-item", s.AdminID, "delta", 1, "x")
	if err == nil {
		t.Fatal("expected error for missing item")
	}
}
