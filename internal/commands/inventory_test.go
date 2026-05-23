package commands

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

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

func seedItem(t *testing.T, app core.App, code string, qty int) string {
	t.Helper()
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	item := core.NewRecord(items)
	item.Set("code", code)
	item.Set("name", "Test "+code)
	item.Set("type", "consumable")
	item.Set("tracking_mode", "quantity")
	item.Set("active", true)
	item.Set("quantity_on_hand", qty)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}
	return item.Id
}

// TestInventoryAdjust_HappyPath exercises the dispatcher's inventory.adjust
// handler end-to-end (minus NATS). The handler is a thin wrapper over
// PerformStockAdjustment — what we verify here is JSON parsing, validation,
// and reply shape.
func TestInventoryAdjust_HappyPath(t *testing.T) {
	app := setupApp(t)
	seedItem(t, app, "WIDGET", 10)

	d := NewDispatcher(app, "KIOSK01")

	payload, _ := json.Marshal(map[string]any{
		"command_id":          "cmd-1",
		"controller_admin_id": "ctrl-admin",
		"item_code":           "WIDGET",
		"mode":                "delta",
		"value":               3,
		"reason":              "restock",
	})
	reply := d.handleInventoryAdjust(context.Background(), payload)

	if !reply.Success {
		t.Fatalf("expected success, got error %q", reply.Error)
	}
	dataBytes, err := json.Marshal(reply.Data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	var out inventoryAdjustReply
	if err := json.Unmarshal(dataBytes, &out); err != nil {
		t.Fatalf("unmarshal reply.Data: %v", err)
	}
	if out.NewQuantity != 13 || out.Delta != 3 || out.PrevQuantity != 10 {
		t.Errorf("reply: got %+v, want new=13 delta=3 prev=10", out)
	}
}

// TestInventoryAdjust_ValidationErrors checks every required-field guard.
// Each returns success=false with a descriptive error — no DB mutation.
func TestInventoryAdjust_ValidationErrors(t *testing.T) {
	app := setupApp(t)
	seedItem(t, app, "WIDGET", 10)

	d := NewDispatcher(app, "KIOSK01")

	cases := []struct {
		name string
		body map[string]any
		want string // substring of expected error
	}{
		{
			name: "missing command_id",
			body: map[string]any{"controller_admin_id": "a", "item_code": "WIDGET", "mode": "delta", "value": 1, "reason": "r"},
			want: "command_id is required",
		},
		{
			name: "missing controller_admin_id",
			body: map[string]any{"command_id": "c", "item_code": "WIDGET", "mode": "delta", "value": 1, "reason": "r"},
			want: "controller_admin_id is required",
		},
		{
			name: "missing item_code",
			body: map[string]any{"command_id": "c", "controller_admin_id": "a", "mode": "delta", "value": 1, "reason": "r"},
			want: "item_code is required",
		},
		{
			name: "bad mode",
			body: map[string]any{"command_id": "c", "controller_admin_id": "a", "item_code": "WIDGET", "mode": "weird", "value": 1, "reason": "r"},
			want: "mode must be",
		},
		{
			name: "missing reason",
			body: map[string]any{"command_id": "c", "controller_admin_id": "a", "item_code": "WIDGET", "mode": "delta", "value": 1, "reason": ""},
			want: "reason is required",
		},
		{
			name: "unknown item",
			body: map[string]any{"command_id": "c", "controller_admin_id": "a", "item_code": "NOPE", "mode": "delta", "value": 1, "reason": "r"},
			want: "not found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(tc.body)
			reply := d.handleInventoryAdjust(context.Background(), payload)
			if reply.Success {
				t.Fatalf("expected failure, got success with data %+v", reply.Data)
			}
			if !contains(reply.Error, tc.want) {
				t.Errorf("error %q does not contain %q", reply.Error, tc.want)
			}
		})
	}
}

// TestInventoryAdjust_IdempotentReplay verifies that the dispatcher relays
// PerformStockAdjustment's idempotency: a duplicate command_id returns the
// same reply without re-applying.
func TestInventoryAdjust_IdempotentReplay(t *testing.T) {
	app := setupApp(t)
	seedItem(t, app, "WIDGET", 10)

	d := NewDispatcher(app, "KIOSK01")

	body := map[string]any{
		"command_id":          "cmd-replay",
		"controller_admin_id": "ctrl",
		"item_code":           "WIDGET",
		"mode":                "delta",
		"value":               5,
		"reason":              "first",
	}
	payload, _ := json.Marshal(body)
	first := d.handleInventoryAdjust(context.Background(), payload)
	if !first.Success {
		t.Fatalf("first call: %s", first.Error)
	}
	// Replay with same command_id, different value/reason — must return
	// prior result, not re-apply.
	body["value"] = 999
	body["reason"] = "retry"
	payload2, _ := json.Marshal(body)
	second := d.handleInventoryAdjust(context.Background(), payload2)
	if !second.Success {
		t.Fatalf("replay: %s", second.Error)
	}

	// Decode both
	firstBytes, _ := json.Marshal(first.Data)
	secondBytes, _ := json.Marshal(second.Data)
	var a, b inventoryAdjustReply
	_ = json.Unmarshal(firstBytes, &a)
	_ = json.Unmarshal(secondBytes, &b)

	if a.AdjustmentID != b.AdjustmentID {
		t.Errorf("replay returned different adjustment: first=%s second=%s",
			a.AdjustmentID, b.AdjustmentID)
	}
	if b.NewQuantity != 15 {
		t.Errorf("qty after replay: want 15 (single application), got %d", b.NewQuantity)
	}
}

// TestInventorySnapshot_AllItems returns every active item by default.
func TestInventorySnapshot_AllItems(t *testing.T) {
	app := setupApp(t)
	seedItem(t, app, "AAA", 1)
	seedItem(t, app, "BBB", 2)
	seedItem(t, app, "CCC", 3)

	d := NewDispatcher(app, "KIOSK01")
	reply := d.handleInventorySnapshot(context.Background(), nil)
	if !reply.Success {
		t.Fatalf("snapshot: %s", reply.Error)
	}
	bytes, _ := json.Marshal(reply.Data)
	var out inventorySnapshotReply
	if err := json.Unmarshal(bytes, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// At least the three we seeded (the init migration may seed others).
	codes := map[string]bool{}
	for _, it := range out.Items {
		codes[it.ItemCode] = true
	}
	for _, want := range []string{"AAA", "BBB", "CCC"} {
		if !codes[want] {
			t.Errorf("snapshot missing %q; got codes %v", want, codes)
		}
	}
}

// TestInventorySnapshot_FilteredCodes narrows by item_codes.
func TestInventorySnapshot_FilteredCodes(t *testing.T) {
	app := setupApp(t)
	seedItem(t, app, "AAA", 1)
	seedItem(t, app, "BBB", 2)
	seedItem(t, app, "CCC", 3)

	d := NewDispatcher(app, "KIOSK01")
	payload, _ := json.Marshal(map[string]any{
		"item_codes": []string{"AAA", "CCC"},
	})
	reply := d.handleInventorySnapshot(context.Background(), payload)
	if !reply.Success {
		t.Fatalf("snapshot: %s", reply.Error)
	}
	bytes, _ := json.Marshal(reply.Data)
	var out inventorySnapshotReply
	_ = json.Unmarshal(bytes, &out)
	if len(out.Items) != 2 {
		t.Errorf("filtered snapshot: want 2, got %d (%+v)", len(out.Items), out.Items)
	}
	for _, it := range out.Items {
		if it.ItemCode == "BBB" {
			t.Errorf("filter leaked BBB through")
		}
	}
}

// TestSubjectSuffix verifies the dispatcher's subject-suffix extraction —
// the routing primitive that maps an incoming NATS subject to a handler.
func TestSubjectSuffix(t *testing.T) {
	d := NewDispatcher(nil, "K01")
	got := d.subjectSuffix("kiosk.K01.command.inventory.adjust")
	if got != "inventory.adjust" {
		t.Errorf("suffix: got %q, want %q", got, "inventory.adjust")
	}
	got2 := d.subjectSuffix("kiosk.K01.command.something.else")
	if got2 != "something.else" {
		t.Errorf("suffix: got %q, want %q", got2, "something.else")
	}
}

// contains is a tiny helper to avoid pulling strings into this test.
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

