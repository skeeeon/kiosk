package exports

import (
	"sort"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func seedItemLowStock(t *testing.T, app core.App, code, typ string, onHand, threshold int) {
	t.Helper()
	col, _ := app.FindCollectionByNameOrId("items")
	it := core.NewRecord(col)
	it.Set("code", code)
	it.Set("name", code)
	it.Set("type", typ)
	it.Set("tracking_mode", "quantity")
	it.Set("active", true)
	it.Set("quantity_on_hand", onHand)
	it.Set("reorder_threshold", threshold)
	if err := app.Save(it); err != nil {
		t.Fatalf("save item %s: %v", code, err)
	}
}

// TestComputeLowStockRows pins the shared predicate the CSV report and the
// metrics snapshot both depend on: threshold > 0 required, tools subtract
// nothing here (no open checkouts seeded), consumables use raw on-hand, and the
// kioskCode is stamped onto every row.
func TestComputeLowStockRows(t *testing.T) {
	app := setupApp(t)

	seedItemLowStock(t, app, "LOW", "tool", 2, 5)         // 2 ≤ 5 → low
	seedItemLowStock(t, app, "OK", "tool", 10, 3)         // 10 > 3 → not low
	seedItemLowStock(t, app, "NOTHRESH", "tool", 1, 0)    // threshold 0 → skipped
	seedItemLowStock(t, app, "GLOVES", "consumable", 1, 5) // 1 ≤ 5 → low

	rows, err := ComputeLowStockRows(app, "KIOSK-A")
	if err != nil {
		t.Fatalf("ComputeLowStockRows: %v", err)
	}

	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ItemCode)
		if r.KioskCode != "KIOSK-A" {
			t.Errorf("row %s kiosk_code = %q, want KIOSK-A", r.ItemCode, r.KioskCode)
		}
	}
	sort.Strings(got)

	want := []string{"GLOVES", "LOW"}
	if len(got) != len(want) {
		t.Fatalf("low-stock items = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("low-stock items = %v, want %v", got, want)
		}
	}
}
