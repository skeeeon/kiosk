package commands

import (
	"context"
	"encoding/json"
	"testing"
)

// TestCheckoutSnapshot verifies the command reads open_checkouts and hydrates
// it into the same DTO shape + id scheme the report uses — critically, the DTO
// id is the transaction_line id, which the admin-close flow keys on.
func TestCheckoutSnapshot(t *testing.T) {
	app := setupApp(t)
	lineID, _ := seedCheckoutScenario(t, app) // one WRENCH-1 out, held by EMP-100

	d := NewDispatcher(app, "KIOSK01")
	reply := d.handleCheckoutSnapshot(context.Background(), nil)
	if !reply.Success {
		t.Fatalf("checkout.snapshot failed: %s", reply.Error)
	}

	raw, err := json.Marshal(reply.Data)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	var got checkoutSnapshotReply
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if len(got.OpenCheckouts) != 1 {
		t.Fatalf("want 1 open checkout, got %d", len(got.OpenCheckouts))
	}
	dto := got.OpenCheckouts[0]
	if dto.ID != lineID {
		t.Errorf("DTO id = %q, want transaction_line id %q (admin-close keys on it)", dto.ID, lineID)
	}
	if dto.KioskCode != "KIOSK01" {
		t.Errorf("kiosk_code = %q, want KIOSK01", dto.KioskCode)
	}
	if dto.Expand.Item == nil || dto.Expand.Item.Code != "WRENCH-1" {
		t.Errorf("expand.item = %+v, want code WRENCH-1", dto.Expand.Item)
	}
	if dto.Expand.User == nil || dto.Expand.User.Code != "EMP-100" {
		t.Errorf("expand.user = %+v, want code EMP-100", dto.Expand.User)
	}
}

// TestInventorySnapshot_OutCount verifies the inventory snapshot reports the
// per-item out-count from open_checkouts (not 0, not replayed).
func TestInventorySnapshot_OutCount(t *testing.T) {
	app := setupApp(t)
	seedCheckoutScenario(t, app) // one WRENCH-1 currently out

	d := NewDispatcher(app, "KIOSK01")
	reply := d.handleInventorySnapshot(context.Background(), nil)
	if !reply.Success {
		t.Fatalf("inventory.snapshot failed: %s", reply.Error)
	}
	raw, err := json.Marshal(reply.Data)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	var got inventorySnapshotReply
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	for _, it := range got.Items {
		if it.ItemCode == "WRENCH-1" {
			if it.Out != 1 {
				t.Fatalf("WRENCH-1 out = %d, want 1", it.Out)
			}
			return
		}
	}
	t.Fatalf("WRENCH-1 not present in snapshot")
}
