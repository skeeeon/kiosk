package commands

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/handlers"
)

// setThreshold is the call under test, wrapped so each case reads as one line.
func setThreshold(t *testing.T, d *Dispatcher, body map[string]any) Reply {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return d.handleInventorySetThreshold(context.Background(), payload)
}

func thresholdOf(t *testing.T, app core.App, code string) int {
	t.Helper()
	rec, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": code})
	if err != nil {
		t.Fatalf("find item %s: %v", code, err)
	}
	return rec.GetInt("reorder_threshold")
}

// TestInventorySetThreshold_HappyPath — this command is the ONLY way a
// controller-managed kiosk receives a reorder_threshold. The field is
// kiosk-local by design and never crosses the catalogue wire, so without
// it low-stock alerting cannot fire anywhere in a managed fleet.
func TestInventorySetThreshold_HappyPath(t *testing.T) {
	app := setupApp(t)
	seedItem(t, app, "WIDGET", 10)
	d := NewDispatcher(app, "KIOSK01")

	reply := setThreshold(t, d, map[string]any{
		"controller_admin_id": "ctrl-admin",
		"item_code":           "WIDGET",
		"value":               4,
	})
	if !reply.Success {
		t.Fatalf("expected success, got %q", reply.Error)
	}

	dataBytes, _ := json.Marshal(reply.Data)
	var out handlers.ReorderThresholdResult
	if err := json.Unmarshal(dataBytes, &out); err != nil {
		t.Fatalf("unmarshal reply.Data: %v", err)
	}
	if out.ReorderThreshold != 4 || out.PrevThreshold != 0 || out.ItemCode != "WIDGET" {
		t.Errorf("reply: got %+v, want threshold=4 prev=0 code=WIDGET", out)
	}
	if got := thresholdOf(t, app, "WIDGET"); got != 4 {
		t.Errorf("stored threshold = %d, want 4", got)
	}
}

// TestInventorySetThreshold_IsAbsoluteAndReplaySafe is why the command
// carries no command_id: "set it to 4" converges on 4 however many times it
// arrives, so there is nothing an idempotency key would protect.
func TestInventorySetThreshold_IsAbsoluteAndReplaySafe(t *testing.T) {
	app := setupApp(t)
	seedItem(t, app, "WIDGET", 10)
	d := NewDispatcher(app, "KIOSK01")

	body := map[string]any{
		"controller_admin_id": "ctrl-admin",
		"item_code":           "WIDGET",
		"value":               4,
	}
	for i := 0; i < 3; i++ {
		if reply := setThreshold(t, d, body); !reply.Success {
			t.Fatalf("attempt %d: %s", i+1, reply.Error)
		}
	}
	if got := thresholdOf(t, app, "WIDGET"); got != 4 {
		t.Errorf("after three identical sends threshold = %d, want 4", got)
	}

	// And a different value replaces rather than accumulating — the trap a
	// delta-shaped command would fall into.
	body["value"] = 9
	if reply := setThreshold(t, d, body); !reply.Success {
		t.Fatalf("re-set: %s", reply.Error)
	}
	if got := thresholdOf(t, app, "WIDGET"); got != 9 {
		t.Errorf("threshold = %d, want 9 (replace, not add)", got)
	}
}

// TestInventorySetThreshold_ZeroDisablesTheAlert — 0 means "no low-stock
// alert for this SKU", which is a legitimate setting and must not be
// mistaken for an unset field and ignored.
func TestInventorySetThreshold_ZeroDisablesTheAlert(t *testing.T) {
	app := setupApp(t)
	seedItem(t, app, "WIDGET", 10)
	d := NewDispatcher(app, "KIOSK01")

	if reply := setThreshold(t, d, map[string]any{
		"controller_admin_id": "ctrl-admin", "item_code": "WIDGET", "value": 5,
	}); !reply.Success {
		t.Fatalf("set to 5: %s", reply.Error)
	}
	if reply := setThreshold(t, d, map[string]any{
		"controller_admin_id": "ctrl-admin", "item_code": "WIDGET", "value": 0,
	}); !reply.Success {
		t.Fatalf("set to 0: %s", reply.Error)
	}
	if got := thresholdOf(t, app, "WIDGET"); got != 0 {
		t.Errorf("threshold = %d, want 0 — zero must clear the alert, not be ignored", got)
	}
}

// TestInventorySetThreshold_AllowsSerializedSKUs — unlike inventory.adjust,
// which refuses them because their quantity_on_hand is derived from the
// instance count. A threshold is an alert level against that count and is
// perfectly meaningful: "tell me when fewer than two drills are available".
func TestInventorySetThreshold_AllowsSerializedSKUs(t *testing.T) {
	app := setupApp(t)
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	rec := core.NewRecord(items)
	rec.Set("code", "DRILL")
	rec.Set("name", "Drill")
	rec.Set("type", "tool")
	rec.Set("tracking_mode", "serialized")
	rec.Set("active", true)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	d := NewDispatcher(app, "KIOSK01")
	reply := setThreshold(t, d, map[string]any{
		"controller_admin_id": "ctrl-admin", "item_code": "DRILL", "value": 2,
	})
	if !reply.Success {
		t.Fatalf("serialized SKUs must accept a threshold: %s", reply.Error)
	}
	if got := thresholdOf(t, app, "DRILL"); got != 2 {
		t.Errorf("threshold = %d, want 2", got)
	}
}

func TestInventorySetThreshold_ValidationErrors(t *testing.T) {
	app := setupApp(t)
	seedItem(t, app, "WIDGET", 10)
	d := NewDispatcher(app, "KIOSK01")

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing controller_admin_id", map[string]any{"item_code": "WIDGET", "value": 1}},
		{"blank controller_admin_id", map[string]any{"controller_admin_id": "   ", "item_code": "WIDGET", "value": 1}},
		{"missing item_code", map[string]any{"controller_admin_id": "a", "value": 1}},
		{"negative value", map[string]any{"controller_admin_id": "a", "item_code": "WIDGET", "value": -1}},
		{"unknown item_code", map[string]any{"controller_admin_id": "a", "item_code": "NOPE", "value": 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if reply := setThreshold(t, d, c.body); reply.Success {
				t.Fatalf("expected failure, got success")
			}
		})
	}
	// None of the rejects touched the item.
	if got := thresholdOf(t, app, "WIDGET"); got != 0 {
		t.Errorf("a rejected command wrote anyway: threshold = %d", got)
	}
}

// TestInventorySetThreshold_MalformedBody — a command handler must always
// reply, even on garbage, or the controller renders "kiosk offline".
func TestInventorySetThreshold_MalformedBody(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")
	reply := d.handleInventorySetThreshold(context.Background(), []byte("{not json"))
	if reply.Success {
		t.Fatal("malformed JSON must not succeed")
	}
	if reply.Error == "" {
		t.Fatal("a failed reply must carry error text — the controller shows it to the operator")
	}
}
