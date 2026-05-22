package controller

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

// seedKiosk inserts a kiosks row directly via the DAO. Tests can't use the
// API path because PB rules don't apply to test-context DAO writes.
func seedKiosk(t *testing.T, app core.App, code, location string) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("kiosks")
	if err != nil {
		t.Fatalf("find kiosks: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("kiosk_code", code)
	rec.Set("location_code", location)
	rec.Set("status", "unknown")
	if err := app.Save(rec); err != nil {
		t.Fatalf("save kiosk: %v", err)
	}
	return rec.Id
}

// seedItem is defined in consumer_test.go in this package.

func seedKioskItem(t *testing.T, app core.App, kioskID, itemID string) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("kiosk_items")
	if err != nil {
		t.Fatalf("find kiosk_items: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("kiosk", kioskID)
	rec.Set("item", itemID)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save kiosk_items: %v", err)
	}
	return rec.Id
}

func TestKiosksForItem_ReturnsAllMembers(t *testing.T) {
	app := setupApp(t)
	k1 := seedKiosk(t, app, "K01", "shop-a")
	k2 := seedKiosk(t, app, "K02", "shop-b")
	itemA := seedItem(t, app, "WRENCH-10", "10mm Wrench")
	itemB := seedItem(t, app, "HAMMER-1", "Hammer")

	seedKioskItem(t, app, k1, itemA)
	seedKioskItem(t, app, k2, itemA)
	seedKioskItem(t, app, k1, itemB)

	kiosks, err := KiosksForItem(app, itemA)
	if err != nil {
		t.Fatalf("KiosksForItem: %v", err)
	}
	if len(kiosks) != 2 {
		t.Fatalf("expected 2 kiosks for itemA, got %d", len(kiosks))
	}

	codes := map[string]bool{}
	for _, k := range kiosks {
		codes[k.GetString("kiosk_code")] = true
	}
	if !codes["K01"] || !codes["K02"] {
		t.Errorf("expected K01 and K02 in members, got %v", codes)
	}
}

func TestKiosksForItem_NoMembersIsEmpty(t *testing.T) {
	app := setupApp(t)
	itemA := seedItem(t, app, "ORPHAN", "Nobody stocks this")

	kiosks, err := KiosksForItem(app, itemA)
	if err != nil {
		t.Fatalf("KiosksForItem: %v", err)
	}
	if len(kiosks) != 0 {
		t.Errorf("expected zero kiosks, got %d", len(kiosks))
	}
}

func TestItemsForKiosk_ReturnsAllItems(t *testing.T) {
	app := setupApp(t)
	k1 := seedKiosk(t, app, "K01", "shop-a")
	itemA := seedItem(t, app, "WRENCH-10", "10mm Wrench")
	itemB := seedItem(t, app, "HAMMER-1", "Hammer")
	_ = seedItem(t, app, "SCREWDRIVER", "Screwdriver") // unassigned; should not appear

	seedKioskItem(t, app, k1, itemA)
	seedKioskItem(t, app, k1, itemB)

	items, err := ItemsForKiosk(app, k1)
	if err != nil {
		t.Fatalf("ItemsForKiosk: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	codes := map[string]bool{}
	for _, it := range items {
		codes[it.GetString("code")] = true
	}
	if !codes["WRENCH-10"] || !codes["HAMMER-1"] || codes["SCREWDRIVER"] {
		t.Errorf("unexpected items: %v", codes)
	}
}

func TestCascadeDelete_RemovesMembershipRows(t *testing.T) {
	app := setupApp(t)
	k1 := seedKiosk(t, app, "K01", "shop-a")
	itemA := seedItem(t, app, "WRENCH-10", "10mm Wrench")
	rowID := seedKioskItem(t, app, k1, itemA)

	// Sanity check the row exists.
	if _, err := app.FindRecordById("kiosk_items", rowID); err != nil {
		t.Fatalf("kiosk_items row missing immediately after seed: %v", err)
	}

	// Delete the item — cascade should drop the kiosk_items row.
	item, err := app.FindRecordById("items", itemA)
	if err != nil {
		t.Fatalf("find item: %v", err)
	}
	if err := app.Delete(item); err != nil {
		t.Fatalf("delete item: %v", err)
	}

	if _, err := app.FindRecordById("kiosk_items", rowID); err == nil {
		t.Errorf("expected kiosk_items row to be cascade-deleted with the item")
	}
}
