package cart

import (
	"errors"
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
	c1 := s.Start("u1", "EMP-1", "Alice", "worker")
	c2 := s.Start("u1", "EMP-1", "Alice", "worker")
	if c1.ID != c2.ID {
		t.Fatalf("expected same cart id, got %s vs %s", c1.ID, c2.ID)
	}
}

func TestStartReturnsNewCartForDifferentUser(t *testing.T) {
	s, _ := newTestStore()
	c1 := s.Start("u1", "EMP-1", "Alice", "worker")
	c2 := s.Start("u2", "EMP-2", "Bob", "worker")
	if c1.ID == c2.ID {
		t.Fatalf("expected different cart ids")
	}
}

func TestExpiredCartIsDeletedOnAccess(t *testing.T) {
	s, now := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice", "worker")
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
	c := s.Start("u1", "EMP-1", "Alice", "worker")
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
	c := s.Start("u1", "EMP-1", "Alice", "worker")
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

// Two scans of the same physical instance in one cart would both pass the
// no-stacking rule above (each becomes its own line), then collide at
// commit-time against the open_checkouts unique-serial index with an
// opaque error. AddLine catches it up front instead.
func TestDuplicateInstanceRejected(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice", "worker")
	_, _, err := s.AddLine(c.ID, &Line{
		ItemID: "item-a", ItemType: "tool", TrackingMode: "serialized",
		Action: "checkout", Qty: 1, Serial: "SN-1",
		ItemInstanceID: "inst-A",
	})
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	_, _, err = s.AddLine(c.ID, &Line{
		ItemID: "item-a", ItemType: "tool", TrackingMode: "serialized",
		Action: "checkout", Qty: 1, Serial: "SN-1",
		ItemInstanceID: "inst-A",
	})
	if !errors.Is(err, ErrDuplicateInstance) {
		t.Fatalf("second add: want ErrDuplicateInstance, got %v", err)
	}
	if len(c.Lines) != 1 {
		t.Errorf("cart should still have 1 line, got %d", len(c.Lines))
	}
}

// Different instances of the same SKU should still get separate lines —
// the duplicate guard only rejects exact ItemInstanceID matches.
func TestDifferentInstancesSameSKUStillSeparate(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice", "worker")
	_, _, _ = s.AddLine(c.ID, &Line{
		ItemID: "item-a", ItemType: "tool", TrackingMode: "serialized",
		Action: "checkout", Qty: 1, Serial: "SN-1",
		ItemInstanceID: "inst-A",
	})
	_, _, err := s.AddLine(c.ID, &Line{
		ItemID: "item-a", ItemType: "tool", TrackingMode: "serialized",
		Action: "checkout", Qty: 1, Serial: "SN-2",
		ItemInstanceID: "inst-B",
	})
	if err != nil {
		t.Fatalf("second add (different instance): %v", err)
	}
	if len(c.Lines) != 2 {
		t.Errorf("expected 2 lines, got %d", len(c.Lines))
	}
}

// A foreman-return line carries OriginalCheckoutUserID to identify whose
// open_checkout it closes. Stacking it onto a same-item self-return would
// silently strip that signal, so the commit-time foreman+group gate would
// not fire. AddLine must treat OriginalCheckoutUserID as part of the merge
// key.
func TestNoStackingWhenOriginalCheckoutUserDiffers(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice", "foreman")
	_, _, _ = s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemType: "tool", TrackingMode: "quantity",
		Action: "return", Qty: 1,
		// self-return: OriginalCheckoutUserID unset
	})
	_, _, _ = s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemType: "tool", TrackingMode: "quantity",
		Action: "return", Qty: 1,
		OriginalCheckoutUserID:   "u2",
		OriginalCheckoutUserName: "Bob",
	})
	if len(c.Lines) != 2 {
		t.Fatalf("expected 2 lines (self-return + foreman-return), got %d", len(c.Lines))
	}
}

func TestNoStackingWhenActionDiffers(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice", "worker")
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
	c := s.Start("u1", "EMP-1", "Alice", "worker")
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
	c := s.Start("u1", "EMP-1", "Alice", "worker")
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
	c := s.Start("u1", "EMP-1", "Alice", "worker")
	if _, _, err := s.AddLine(c.ID, &Line{
		ItemID: "item-1", ItemType: "consumable", TrackingMode: "quantity",
		Action: "consume", Qty: MaxQty + 1,
	}); err != ErrQtyOutOfRange {
		t.Fatalf("want ErrQtyOutOfRange, got %v", err)
	}
}

func TestAddLineStackingRespectsMaxQty(t *testing.T) {
	s, _ := newTestStore()
	c := s.Start("u1", "EMP-1", "Alice", "worker")
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
	c := s.Start("u1", "EMP-1", "Alice", "worker")
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
	c := s.Start("u1", "EMP-1", "Alice", "worker")
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
	c := s.Start("u1", "EMP-1", "Alice", "worker")
	if err := s.Delete(c.ID); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if _, err := s.Get(c.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}
