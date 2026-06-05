package controller

import (
	"encoding/json"
	"testing"
	"time"
)

// enrichInventorySnapshot should decorate each item with the ledger-derived
// "out" count and the catalog "type", leaving the kiosk's own fields intact.
func TestEnrichInventorySnapshot_AddsOutAndType(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "DRILL", "Drill") // seedItem makes a tool

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()
	mustProjectTx(t, agg, "tx-1", "WORKER-1", "KIOSK-A", t0)
	mustProjectLine(t, agg, EventPayload{
		LineID: "line-1", KioskCode: "KIOSK-A", TransactionID: "tx-1",
		ItemCode: "DRILL", UserCode: "WORKER-1", Action: "checkout", Qty: 2, CompletedAt: t0,
	})

	raw := json.RawMessage(`{"items":[{"item_code":"DRILL","item_name":"Drill","quantity_on_hand":5,"reorder_threshold":2,"tracking_mode":"quantity","active":true}]}`)
	out, err := enrichInventorySnapshot(app, "KIOSK-A", raw)
	if err != nil {
		t.Fatalf("enrichInventorySnapshot: %v", err)
	}

	var got struct {
		Items []struct {
			ItemCode       string `json:"item_code"`
			QuantityOnHand int    `json:"quantity_on_hand"`
			Out            int    `json:"out"`
			Type           string `json:"type"`
		} `json:"items"`
	}
	mustRemarshal(t, out, &got)

	if len(got.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(got.Items))
	}
	it := got.Items[0]
	if it.Out != 2 {
		t.Errorf("out: want 2, got %d", it.Out)
	}
	if it.Type != "tool" {
		t.Errorf("type: want tool, got %q", it.Type)
	}
	if it.QuantityOnHand != 5 {
		t.Errorf("quantity_on_hand passthrough: want 5, got %d", it.QuantityOnHand)
	}
}

// An item with nothing out should get out=0 (Go's map zero value), and an item
// the catalog hasn't synced should get type="" rather than an error.
func TestEnrichInventorySnapshot_ZeroOutAndUnknownType(t *testing.T) {
	app := setupApp(t)
	seedItem(t, app, "DRILL", "Drill")

	raw := json.RawMessage(`{"items":[{"item_code":"DRILL","quantity_on_hand":3},{"item_code":"GHOST","quantity_on_hand":1}]}`)
	out, err := enrichInventorySnapshot(app, "KIOSK-A", raw)
	if err != nil {
		t.Fatalf("enrichInventorySnapshot: %v", err)
	}
	var got struct {
		Items []struct {
			ItemCode string `json:"item_code"`
			Out      int    `json:"out"`
			Type     string `json:"type"`
		} `json:"items"`
	}
	mustRemarshal(t, out, &got)

	for _, it := range got.Items {
		if it.Out != 0 {
			t.Errorf("%s out: want 0, got %d", it.ItemCode, it.Out)
		}
	}
	byCode := map[string]string{}
	for _, it := range got.Items {
		byCode[it.ItemCode] = it.Type
	}
	if byCode["DRILL"] != "tool" {
		t.Errorf("DRILL type: want tool, got %q", byCode["DRILL"])
	}
	if byCode["GHOST"] != "" {
		t.Errorf("unsynced item type: want empty, got %q", byCode["GHOST"])
	}
}

// enrichInstanceSnapshot should mark exactly the instances currently out,
// derived from the ledger's serialized open rows.
func TestEnrichInstanceSnapshot_MarksOut(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")
	seedItem(t, app, "SPLICE", "Fiber Splicer")

	agg := NewAggregator(app, nil, "")
	t0 := time.Now().UTC()
	mustProjectTx(t, agg, "tx-1", "WORKER-1", "KIOSK-A", t0)
	mustProjectLine(t, agg, EventPayload{
		LineID: "line-1", KioskCode: "KIOSK-A", TransactionID: "tx-1",
		ItemCode: "SPLICE", UserCode: "WORKER-1", Action: "checkout", Qty: 1,
		ItemInstanceID: "inst-42", CompletedAt: t0,
	})

	raw := json.RawMessage(`{"instances":[{"instance_id":"inst-42","status":"in_service"},{"instance_id":"inst-99","status":"in_service"}]}`)
	out, err := enrichInstanceSnapshot(app, "KIOSK-A", raw)
	if err != nil {
		t.Fatalf("enrichInstanceSnapshot: %v", err)
	}
	var got struct {
		Instances []struct {
			InstanceID string `json:"instance_id"`
			Out        bool   `json:"out"`
		} `json:"instances"`
	}
	mustRemarshal(t, out, &got)

	outByID := map[string]bool{}
	for _, inst := range got.Instances {
		outByID[inst.InstanceID] = inst.Out
	}
	if !outByID["inst-42"] {
		t.Errorf("inst-42 should be out")
	}
	if outByID["inst-99"] {
		t.Errorf("inst-99 should not be out")
	}
}

func mustRemarshal(t *testing.T, v any, dst any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal enriched reply: %v", err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("unmarshal enriched reply: %v", err)
	}
}
