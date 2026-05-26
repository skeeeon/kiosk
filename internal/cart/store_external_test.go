package cart

import (
	"errors"
	"testing"
	"time"
)

// TestStartByExternal_NewCart creates a fresh cart on first fire,
// stamps it with the door_id, and reports reused=false.
func TestStartByExternal_NewCart(t *testing.T) {
	s, _ := newTestStore()
	c, reused := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-A")
	if reused {
		t.Errorf("first call should report reused=false")
	}
	if c.DoorID != "BAY-A" {
		t.Errorf("DoorID = %q, want BAY-A", c.DoorID)
	}
	if c.UserID != "u1" || c.UserCode != "EMP-1" {
		t.Errorf("user fields not populated: %+v", c)
	}
}

// TestStartByExternal_Idempotent: same (user_code, door_id) within
// the idle window returns the same cart_id. This is the contract
// callers (the access-control system firing cart.start multiple
// times) depend on.
func TestStartByExternal_Idempotent(t *testing.T) {
	s, _ := newTestStore()
	c1, _ := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-A")
	c2, reused := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-A")
	if c1.ID != c2.ID {
		t.Errorf("expected same cart id on idempotent fire, got %s vs %s", c1.ID, c2.ID)
	}
	if !reused {
		t.Errorf("second call should report reused=true")
	}
}

// TestStartByExternal_DifferentDoorsAreSeparate: same user at two
// distinct doors gets two distinct carts. This is the case Phase 4's
// design contemplates explicitly — two enclosures sharing a kiosk
// are told apart by door_id.
func TestStartByExternal_DifferentDoorsAreSeparate(t *testing.T) {
	s, _ := newTestStore()
	a, _ := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-A")
	b, _ := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-B")
	if a.ID == b.ID {
		t.Errorf("different doors should yield different carts")
	}
	if a.DoorID == b.DoorID {
		t.Errorf("DoorIDs should be distinct: %q vs %q", a.DoorID, b.DoorID)
	}
}

// TestStartByExternal_DifferentUsersAreSeparate: same door, two
// different workers — distinct carts. The (user_code, door_id) key
// makes this fall out for free, but lock it in so a future refactor
// to door-only keying is caught.
func TestStartByExternal_DifferentUsersAreSeparate(t *testing.T) {
	s, _ := newTestStore()
	a, _ := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-A")
	b, _ := s.StartByExternal("u2", "EMP-2", "Bob", "worker", "BAY-A")
	if a.ID == b.ID {
		t.Errorf("different users at same door should yield different carts")
	}
}

// TestStartByExternal_ExpiredRecreates: after the idle window
// expires, the next fire creates a fresh cart and reports
// reused=false. The old cart is gone from both indexes.
func TestStartByExternal_ExpiredRecreates(t *testing.T) {
	s, now := newTestStore()
	c1, _ := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-A")
	*now = now.Add(6 * time.Minute)
	c2, reused := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-A")
	if reused {
		t.Errorf("after expiry the next fire should report reused=false")
	}
	if c1.ID == c2.ID {
		t.Errorf("expired + new should yield different cart ids")
	}
	// Primary index should hold only the new cart.
	if _, exists := s.carts[c1.ID]; exists {
		t.Errorf("expired cart should be removed from primary index")
	}
}

// TestGetByUserDoor_HappyPath returns the cart started under the
// same key.
func TestGetByUserDoor_HappyPath(t *testing.T) {
	s, _ := newTestStore()
	c, _ := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-A")
	got, err := s.GetByUserDoor("EMP-1", "BAY-A")
	if err != nil {
		t.Fatalf("GetByUserDoor: %v", err)
	}
	if got.ID != c.ID {
		t.Errorf("got %q, want %q", got.ID, c.ID)
	}
}

// TestGetByUserDoor_NotFound: unknown key surfaces as ErrNotFound,
// which read.trigger uses to reject anonymous reads.
func TestGetByUserDoor_NotFound(t *testing.T) {
	s, _ := newTestStore()
	_, err := s.GetByUserDoor("EMP-NONE", "BAY-A")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// TestGetByUserDoor_ExpiredReturnsNotFound and removes the row, like
// the primary index does. This is the lazy-expiry contract.
func TestGetByUserDoor_ExpiredReturnsNotFound(t *testing.T) {
	s, now := newTestStore()
	c, _ := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-A")
	*now = now.Add(6 * time.Minute)
	if _, err := s.GetByUserDoor("EMP-1", "BAY-A"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after expiry, got %v", err)
	}
	if _, exists := s.byUserDoor[userDoorKey("EMP-1", "BAY-A")]; exists {
		t.Errorf("expired cart should be evicted from secondary index")
	}
	if _, exists := s.carts[c.ID]; exists {
		t.Errorf("expired cart should be evicted from primary index")
	}
}

// TestDelete_CleansSecondaryIndex: Delete must wipe both indexes so a
// subsequent StartByExternal for the same key starts fresh rather
// than returning the just-deleted cart.
func TestDelete_CleansSecondaryIndex(t *testing.T) {
	s, _ := newTestStore()
	c, _ := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-A")
	if err := s.Delete(c.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, exists := s.byUserDoor[userDoorKey("EMP-1", "BAY-A")]; exists {
		t.Errorf("Delete should remove the secondary-index entry")
	}
	// New StartByExternal after Delete should give a fresh cart.
	c2, reused := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-A")
	if reused {
		t.Errorf("after Delete, the next StartByExternal should report reused=false")
	}
	if c.ID == c2.ID {
		t.Errorf("after Delete, cart IDs should differ")
	}
}

// TestStart_DoesNotPopulateSecondaryIndex: the regular Start path
// must not write into byUserDoor. counter_scan carts have no door
// identity and a future read.trigger lookup against them via
// GetByUserDoor would be a bug.
func TestStart_DoesNotPopulateSecondaryIndex(t *testing.T) {
	s, _ := newTestStore()
	s.Start("u1", "EMP-1", "Alice", "worker")
	if len(s.byUserDoor) != 0 {
		t.Errorf("Start should not touch byUserDoor; got %d entries", len(s.byUserDoor))
	}
}

// TestStartByExternal_RoleRefreshOnIdempotent: idempotent refire
// updates UserRole, mirroring how Start handles a worker who got
// promoted between badge taps.
func TestStartByExternal_RoleRefreshOnIdempotent(t *testing.T) {
	s, _ := newTestStore()
	c1, _ := s.StartByExternal("u1", "EMP-1", "Alice", "worker", "BAY-A")
	if c1.UserRole != "worker" {
		t.Fatalf("initial UserRole = %q, want worker", c1.UserRole)
	}
	c2, reused := s.StartByExternal("u1", "EMP-1", "Alice", "foreman", "BAY-A")
	if !reused {
		t.Errorf("expected reused=true")
	}
	if c2.UserRole != "foreman" {
		t.Errorf("UserRole not refreshed: got %q, want foreman", c2.UserRole)
	}
}
