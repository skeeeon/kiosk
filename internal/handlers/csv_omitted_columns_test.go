package handlers

import (
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// TestValidateImportRow_OmittedQuantityColumns confirms the validator only
// emits `quantity_on_hand` / `reorder_threshold` keys when the header is
// present in the CSV. Without the gate, parseCSVInt's empty-string-zero
// fallback would silently set every imported row's quantity to 0 on every
// upsert — wiping the kiosk-local stock state the catalog watcher is
// careful not to touch.
func TestValidateImportRow_OmittedQuantityColumns(t *testing.T) {
	// Header has the core columns but not quantity_on_hand /
	// reorder_threshold. The row index for "active" maps to a "true" cell
	// so the imported record reads as active.
	headers := normalizeHeaders([]string{"code", "name", "type", "tracking_mode", "active"})
	row := []string{"HAMMER", "Hammer", "tool", "quantity", "true"}

	data, errs := validateImportRow(headers, row)
	if len(errs) > 0 {
		t.Fatalf("unexpected validation errors: %+v", errs)
	}
	if _, ok := data["quantity_on_hand"]; ok {
		t.Errorf("data should NOT contain quantity_on_hand when header is omitted; got %v",
			data["quantity_on_hand"])
	}
	if _, ok := data["reorder_threshold"]; ok {
		t.Errorf("data should NOT contain reorder_threshold when header is omitted")
	}
}

// TestValidateImportRow_PresentQuantityColumns_ParsesValue covers the
// other side: when the header IS present, the value parses through to the
// data map (empty/garbage cells fall back to zero per parseCSVInt — but
// the key is still set, which is what makes the omitted-header case above
// matter).
func TestValidateImportRow_PresentQuantityColumns_ParsesValue(t *testing.T) {
	headers := normalizeHeaders([]string{
		"code", "name", "type", "tracking_mode", "active",
		"quantity_on_hand", "reorder_threshold",
	})
	row := []string{"HAMMER", "Hammer", "tool", "quantity", "true", "42", "10"}

	data, errs := validateImportRow(headers, row)
	if len(errs) > 0 {
		t.Fatalf("unexpected validation errors: %+v", errs)
	}
	if got, want := data["quantity_on_hand"], 42; got != want {
		t.Errorf("quantity_on_hand: got %v, want %d", got, want)
	}
	if got, want := data["reorder_threshold"], 10; got != want {
		t.Errorf("reorder_threshold: got %v, want %d", got, want)
	}
}

// TestImportUpsert_PreservesExistingQuantitiesWhenColumnsOmitted exercises
// the end-to-end behavior: an existing item has qty=42 and threshold=10
// recorded via stock adjustments. A re-import of that item with a CSV
// that omits the quantity columns must leave the existing values intact.
// This pins the contract README documents: "if omitted, existing rows
// keep their current values."
func TestImportUpsert_PreservesExistingQuantitiesWhenColumnsOmitted(t *testing.T) {
	app := setupAppInternal(t)

	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	existing := core.NewRecord(items)
	existing.Set("code", "HAMMER")
	existing.Set("name", "Hammer")
	existing.Set("type", "tool")
	existing.Set("tracking_mode", "quantity")
	existing.Set("active", true)
	existing.Set("quantity_on_hand", 42)
	existing.Set("reorder_threshold", 10)
	if err := app.Save(existing); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	// Headers omit the qty columns — same shape an admin would upload to
	// just refresh name/category without touching stock.
	headers := normalizeHeaders([]string{
		"code", "name", "type", "tracking_mode", "active", "category",
	})
	row := []string{"HAMMER", "Hammer (Reframed)", "tool", "quantity", "true", "Hand Tools"}

	data, errs := validateImportRow(headers, row)
	if len(errs) > 0 {
		t.Fatalf("validation: %+v", errs)
	}

	// Mirror the upsert loop in CSVImport.
	rec, err := app.FindFirstRecordByFilter("items",
		"code = {:c}", dbx.Params{"c": data["code"]})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	for k, v := range data {
		rec.Set(k, v)
	}
	if err := app.Save(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Reload from the DB to verify the qty fields survived.
	refreshed, err := app.FindFirstRecordByFilter("items",
		"code = {:c}", dbx.Params{"c": "HAMMER"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := refreshed.GetInt("quantity_on_hand"); got != 42 {
		t.Errorf("quantity_on_hand: want 42 (preserved), got %d", got)
	}
	if got := refreshed.GetInt("reorder_threshold"); got != 10 {
		t.Errorf("reorder_threshold: want 10 (preserved), got %d", got)
	}
	// Sanity: the non-qty fields DID update.
	if got := refreshed.GetString("name"); got != "Hammer (Reframed)" {
		t.Errorf("name: want updated, got %q", got)
	}
	if got := refreshed.GetString("category"); got != "Hand Tools" {
		t.Errorf("category: want updated, got %q", got)
	}
}
