package cart

import (
	"testing"
	"time"
)

func newTestStore() (*Store, *time.Time) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	s := NewStore(5 * time.Minute)
	s.now = func() time.Time { return now }
	return s, &now
}

func TestStartReturnsExistingCartForSameUser(t *testing.T) {
	s, _ := newTestStore()
	c1 := s.Start("u1", "EMP-1", "Alice")
	c2 := s.Start("u1", "EMP-1", "Alice")
	if c1.ID != c2.ID {
		t.Fatalf("expected same cart id, got %s vs %s", c1.ID, c2.ID)
	}
}

func TestStartReturnsNewCartForDifferentUser(t *testing.T) {
	s, _ := newTestStore()
	c1 := s.Start("u1", "EMP-1", "Alice")
	c2 := s.Start("u2", "EMP-2", "Bob")
	if c1.ID == c2.ID {
		t.Fatalf("expected different cart ids")
	}
}

func TestExpiredCartIsDeletedOnAccess(t *testing.T) {
	s, now := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice")
	*now = now.Add(6 * time.Minute)
	if _, err := s.Get(c.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, exists := s.carts[c.ID]; exists {
		t.Fatalf("expected cart to be removed from store")
	}
}

func TestStackingForNonSerializedSameAction(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice")
	_, _, _ = s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemCode: "SCREW", ItemName: "Screws",
		ItemType: "consumable", TrackingMode: "quantity",
		Action: "consume", Qty: 1,
	})
	_, _, _ = s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemCode: "SCREW", ItemName: "Screws",
		ItemType: "consumable", TrackingMode: "quantity",
		Action: "consume", Qty: 2,
	})
	if len(c.Lines) != 1 {
		t.Fatalf("expected 1 stacked line, got %d", len(c.Lines))
	}
	if c.Lines[0].Qty != 3 {
		t.Fatalf("expected qty 3, got %d", c.Lines[0].Qty)
	}
}

func TestNoStackingForSerializedItems(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice")
	_, _, _ = s.AddLine(c.ID, &Line{
		ItemID: "item-a", ItemType: "tool", TrackingMode: "serialized",
		Action: "checkout", Qty: 1, Serial: "SN-1",
	})
	_, _, _ = s.AddLine(c.ID, &Line{
		ItemID: "item-b", ItemType: "tool", TrackingMode: "serialized",
		Action: "checkout", Qty: 1, Serial: "SN-2",
	})
	if len(c.Lines) != 2 {
		t.Fatalf("expected 2 lines for serialized items, got %d", len(c.Lines))
	}
}

func TestNoStackingWhenActionDiffers(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice")
	_, _, _ = s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemType: "tool", TrackingMode: "quantity",
		Action: "checkout", Qty: 1,
	})
	_, _, _ = s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemType: "tool", TrackingMode: "quantity",
		Action: "return", Qty: 1,
	})
	if len(c.Lines) != 2 {
		t.Fatalf("expected 2 lines for different actions, got %d", len(c.Lines))
	}
}

func TestUpdateLineSetsQtyAndAction(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice")
	_, line, _ := s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemType: "tool", TrackingMode: "quantity",
		Action: "checkout", Qty: 1,
	})
	newQty := 5
	newAction := "return"
	_, updated, err := s.UpdateLine(line.ID, &newQty, &newAction)
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if updated.Qty != 5 {
		t.Fatalf("qty: want 5, got %d", updated.Qty)
	}
	if updated.Action != "return" {
		t.Fatalf("action: want return, got %s", updated.Action)
	}
}

func TestUpdateLineRejectsInvalidAction(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice")
	_, line, _ := s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemType: "tool", TrackingMode: "quantity",
		Action: "checkout", Qty: 1,
	})
	bad := "consume"
	if _, _, err := s.UpdateLine(line.ID, nil, &bad); err != ErrInvalidAction {
		t.Fatalf("want ErrInvalidAction, got %v", err)
	}
}

func TestAddLineRejectsQtyOverMax(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice")
	if _, _, err := s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemType: "consumable", TrackingMode: "quantity",
		Action: "consume", Qty: MaxQty + 1,
	}); err != ErrQtyOutOfRange {
		t.Fatalf("want ErrQtyOutOfRange, got %v", err)
	}
}

func TestAddLineStackingRespectsMaxQty(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice")
	if _, _, err := s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemType: "consumable", TrackingMode: "quantity",
		Action: "consume", Qty: MaxQty,
	}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if _, _, err := s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemType: "consumable", TrackingMode: "quantity",
		Action: "consume", Qty: 1,
	}); err != ErrQtyOutOfRange {
		t.Fatalf("second add: want ErrQtyOutOfRange, got %v", err)
	}
}

func TestDeleteLineRemovesIt(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice")
	_, line, _ := s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemType: "consumable", TrackingMode: "quantity",
		Action: "consume", Qty: 1,
	})
	if _, err := s.DeleteLine(line.ID); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if len(c.Lines) != 0 {
		t.Fatalf("expected 0 lines, got %d", len(c.Lines))
	}
}

func TestAddLineExtendsExpiry(t *testing.T) {
	s, now := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice")
	originalExpiry := c.ExpiresAt

	*now = now.Add(2 * time.Minute)
	_, _, _ = s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemType: "consumable", TrackingMode: "quantity",
		Action: "consume", Qty: 1,
	})
	if !c.ExpiresAt.After(originalExpiry) {
		t.Fatalf("expected expiry to extend, original=%v current=%v", originalExpiry, c.ExpiresAt)
	}
}

func TestDeleteCancelsTheCart(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice")
	if err := s.Delete(c.ID); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if _, err := s.Get(c.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}
