// Package cart holds the kiosk's in-memory cart state. Carts are ephemeral —
// they belong to whoever is standing at the kiosk right now, expire after
// `idle_timeout`, and vanish on process restart. Expiry is lazy: any access
// to a stale cart deletes it and returns ErrNotFound.
//
// Concurrency model: the store mutex serializes every operation. A kiosk
// has at most one active user at a time, so contention is nil in practice.
package cart

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// Line is one item action in a cart. Item fields are denormalized so the
// SPA can render without follow-up lookups.
type Line struct {
	ID           string `json:"id"`
	ItemID       string `json:"item_id"`
	ItemCode     string `json:"item_code"`
	ItemName     string `json:"item_name"`
	ItemType     string `json:"item_type"` // "tool" | "consumable"
	TrackingMode string `json:"tracking_mode"`
	Action       string `json:"action"` // "checkout" | "return" | "consume"
	Qty          int    `json:"qty"`
	Serial       string `json:"serial,omitempty"`
	// ItemInstanceID is set when a serialized tool was scanned by its
	// instance code / RFID. Serial and ItemCode are populated from the
	// instance (not the SKU item) so the cart and receipt show the
	// physical unit's identity.
	ItemInstanceID           string   `json:"item_instance_id,omitempty"`
	ItemInstanceCode         string   `json:"item_instance_code,omitempty"`
	OriginalCheckoutUserID   string   `json:"original_checkout_user_id,omitempty"`
	OriginalCheckoutUserName string   `json:"original_checkout_user_name,omitempty"`
	RequestMaintenance       bool     `json:"request_maintenance,omitempty"`
	Warnings                 []string `json:"warnings,omitempty"`
}

type Cart struct {
	ID       string `json:"id"`
	UserID   string `json:"user_id"`
	UserCode string `json:"user_code"`
	UserName string `json:"user_name"`
	// UserRole is a denormalized snapshot used by the SPA to gate
	// foreman-only affordances (e.g. the "Return on behalf of…" button).
	// The server re-reads role from the DB at commit and at the
	// foreman-return endpoint, so a stale snapshot here is at worst a UI
	// hint that fails late, never an auth bypass.
	UserRole string `json:"user_role"`
	// DoorID is non-empty when the cart was started via the
	// enclosure_diff path (an external access-control event firing
	// cart.start). Combined with UserCode it forms the secondary
	// index that makes cart.start idempotent — re-fires within the
	// idle window return the existing cart rather than creating a
	// new one. Empty on counter_scan / badge-driven carts.
	DoorID    string    `json:"door_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Lines     []*Line   `json:"lines"`
}

// MaxQty caps a single cart line's quantity. Scan stacking, the +/- buttons,
// and direct PATCH all share this ceiling. 99 covers any realistic crib
// transaction (boxes of fasteners, packs of gloves) while catching fat-finger
// runaway totals that would otherwise pass through to commit.
const MaxQty = 99

var (
	ErrNotFound          = errors.New("cart not found or expired")
	ErrLineNotFound      = errors.New("cart line not found")
	ErrQtyOutOfRange     = errors.New("qty must be between 1 and MaxQty")
	ErrInvalidAction     = errors.New("action is not valid for this item type")
	ErrDuplicateInstance = errors.New("instance already in cart")
)

// ValidActionForType reports whether the action makes sense for the item type.
// Tools accept checkout/return; consumables only consume. Anything else is a
// client bug and is rejected at the cart and commit layers.
func ValidActionForType(action, itemType string) bool {
	switch itemType {
	case "tool":
		return action == "checkout" || action == "return"
	case "consumable":
		return action == "consume"
	}
	return false
}

type Store struct {
	mu          sync.Mutex
	idleTimeout time.Duration
	now         func() time.Time // injectable for tests
	carts       map[string]*Cart
	// byUserDoor is the secondary index that makes cart.start
	// idempotent for the enclosure_diff path. Key:
	// userDoorKey(userCode, doorID). Only populated for carts
	// started via StartByExternal; the regular Start path doesn't
	// touch it because counter_scan carts have no door identity.
	// Cleaned up in Delete and on lazy expiry through Get.
	byUserDoor map[string]*Cart
}

func NewStore(idleTimeout time.Duration) *Store {
	return &Store{
		idleTimeout: idleTimeout,
		now:         time.Now,
		carts:       make(map[string]*Cart),
		byUserDoor:  make(map[string]*Cart),
	}
}

// userDoorKey is the secondary-index key. NUL byte as a separator
// would collide with neither a valid user code nor a door ID
// (both come from operator-managed config).
func userDoorKey(userCode, doorID string) string {
	return userCode + "\x00" + doorID
}

// Start returns the user's existing non-expired cart if one exists; otherwise
// creates a new cart. Idempotent for the same user within the idle window.
// Resuming refreshes the role snapshot so a role change between scans is
// picked up without forcing the worker to cancel and re-badge.
func (s *Store) Start(userID, userCode, userName, userRole string) *Cart {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for id, c := range s.carts {
		if now.After(c.ExpiresAt) {
			s.removeLocked(id)
			continue
		}
		if c.UserID == userID {
			c.UserRole = userRole
			c.ExpiresAt = now.Add(s.idleTimeout)
			return c
		}
	}

	c := &Cart{
		ID:        newID(),
		UserID:    userID,
		UserCode:  userCode,
		UserName:  userName,
		UserRole:  userRole,
		StartedAt: now,
		ExpiresAt: now.Add(s.idleTimeout),
		Lines:     []*Line{},
	}
	s.carts[c.ID] = c
	return c
}

// StartByExternal is the enclosure_diff path's cart-start entry. The
// access-control system fires a cart.start NATS command carrying
// (user_code, door_id); we look up an existing cart for that key and
// return it (refreshing the role snapshot like Start does), or
// create a new one stamped with the door_id and indexed both ways.
//
// Idempotency: repeat fires within the idle window collapse to the
// same cart. After commit / cancel / expiry, the next call creates a
// fresh cart. The (userCode, doorID) key — not the cart_id — is the
// dedup contract callers rely on, since the access-control system
// has no way to learn the cart_id we generated on the first fire.
//
// `reused` tells the caller whether this was a hit or a miss in the
// secondary index, which they can surface to NATS callers as
// command.cart.start's `{reused: bool}`.
func (s *Store) StartByExternal(userID, userCode, userName, userRole, doorID string) (cart *Cart, reused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	key := userDoorKey(userCode, doorID)
	if existing, ok := s.byUserDoor[key]; ok {
		if now.After(existing.ExpiresAt) {
			s.removeLocked(existing.ID)
		} else {
			// Existing cart for this (user, door) within the window —
			// idempotent. Refresh role + expiry the same way Start
			// resume does so a role change between fires is picked up.
			existing.UserRole = userRole
			existing.ExpiresAt = now.Add(s.idleTimeout)
			return existing, true
		}
	}

	c := &Cart{
		ID:        newID(),
		UserID:    userID,
		UserCode:  userCode,
		UserName:  userName,
		UserRole:  userRole,
		DoorID:    doorID,
		StartedAt: now,
		ExpiresAt: now.Add(s.idleTimeout),
		Lines:     []*Line{},
	}
	s.carts[c.ID] = c
	s.byUserDoor[key] = c
	return c, false
}

// GetByUserDoor returns the active cart for an (userCode, doorID)
// key, or ErrNotFound. The read.trigger command uses this when the
// caller doesn't carry the cart_id from the original cart.start
// reply — e.g. a camera/occupancy system that only knows which door
// fired.
func (s *Store) GetByUserDoor(userCode, doorID string) (*Cart, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byUserDoor[userDoorKey(userCode, doorID)]
	if !ok {
		return nil, ErrNotFound
	}
	if s.now().After(c.ExpiresAt) {
		s.removeLocked(c.ID)
		return nil, ErrNotFound
	}
	return c, nil
}

func (s *Store) Get(cartID string) (*Cart, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(cartID)
}

// Snapshot returns a detached deep copy of the cart for cartID, taken under
// the store lock. commit operates on the snapshot rather than the live *Cart
// so a concurrent writer can't mutate c.Lines while commit ranges over it.
//
// The "one user at a time" assumption holds for the badge/HID flow, but the
// RFID enclosure_diff flow is server/NATS-driven (cart.start + read.trigger)
// and runs concurrently with the SPA — a read.trigger appending lines while
// CartCommit iterates the same Lines slice is a genuine data race. The
// snapshot also isolates commit's own line mutations (e.g. resolving a
// serialized cross-user holder) from store-owned state. Lazy expiry applies
// like Get.
func (s *Store) Snapshot(cartID string) (*Cart, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.getLocked(cartID)
	if err != nil {
		return nil, err
	}
	return c.clone(), nil
}

// clone deep-copies a Cart, including independent Line values (and each
// Line's Warnings slice), so the caller can read/mutate the copy without
// touching store-owned state. Caller holds s.mu so the source isn't mutated
// mid-copy.
func (c *Cart) clone() *Cart {
	cp := *c
	cp.Lines = make([]*Line, len(c.Lines))
	for i, l := range c.Lines {
		ln := *l
		if l.Warnings != nil {
			ln.Warnings = append([]string(nil), l.Warnings...)
		}
		cp.Lines[i] = &ln
	}
	return &cp
}

// AddLine appends a line. Non-serialized items with the same item+action are
// stacked (qty incremented). Serialized items always become a new line.
func (s *Store) AddLine(cartID string, in *Line) (*Cart, *Line, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getLocked(cartID)
	if err != nil {
		return nil, nil, err
	}
	if !ValidActionForType(in.Action, in.ItemType) {
		return nil, nil, ErrInvalidAction
	}
	if in.Qty < 1 || in.Qty > MaxQty {
		return nil, nil, ErrQtyOutOfRange
	}

	if in.TrackingMode == "quantity" {
		for _, existing := range c.Lines {
			// OriginalCheckoutUserID is part of the merge key: a foreman-return
			// for Bob's hammer and a self-return for the foreman's own hammer
			// look identical otherwise but mean different things, and merging
			// them would mask the cross-user signal that the commit gate
			// depends on.
			if existing.ItemID == in.ItemID &&
				existing.Action == in.Action &&
				existing.OriginalCheckoutUserID == in.OriginalCheckoutUserID {
				if existing.Qty+in.Qty > MaxQty {
					return nil, nil, ErrQtyOutOfRange
				}
				existing.Qty += in.Qty
				s.touchLocked(c)
				return c, existing, nil
			}
		}
	}

	// Serialized scans can't be stacked (qty must be 1) and the same
	// physical unit can't be scanned twice in one cart — the open_checkouts
	// unique-serial index would reject the second commit row with an opaque
	// error. Catch it here with a friendlier signal.
	if in.ItemInstanceID != "" {
		for _, existing := range c.Lines {
			if existing.ItemInstanceID == in.ItemInstanceID {
				return nil, nil, ErrDuplicateInstance
			}
		}
	}

	in.ID = newID()
	c.Lines = append(c.Lines, in)
	s.touchLocked(c)
	return c, in, nil
}

// UpdateLine sets qty, action, and/or the request-maintenance flag. Searches
// all carts by line ID since there's at most one cart per user, so the scan is
// trivial. requestMaintenance is only meaningful on a serialized return line;
// the handler validates that before calling.
func (s *Store) UpdateLine(lineID string, qty *int, action *string, requestMaintenance *bool) (*Cart, *Line, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, line := s.findLineLocked(lineID)
	if c == nil {
		return nil, nil, ErrLineNotFound
	}
	if s.now().After(c.ExpiresAt) {
		s.removeLocked(c.ID) // also clears the byUserDoor secondary index
		return nil, nil, ErrNotFound
	}
	if qty != nil {
		if *qty < 1 || *qty > MaxQty {
			return nil, nil, ErrQtyOutOfRange
		}
		line.Qty = *qty
	}
	if action != nil {
		if !ValidActionForType(*action, line.ItemType) {
			return nil, nil, ErrInvalidAction
		}
		line.Action = *action
	}
	if requestMaintenance != nil {
		line.RequestMaintenance = *requestMaintenance
	}
	s.touchLocked(c)
	return c, line, nil
}

func (s *Store) DeleteLine(lineID string) (*Cart, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, _ := s.findLineLocked(lineID)
	if c == nil {
		return nil, ErrLineNotFound
	}
	if s.now().After(c.ExpiresAt) {
		s.removeLocked(c.ID) // also clears the byUserDoor secondary index
		return nil, ErrNotFound
	}
	for i, l := range c.Lines {
		if l.ID == lineID {
			c.Lines = append(c.Lines[:i], c.Lines[i+1:]...)
			break
		}
	}
	s.touchLocked(c)
	return c, nil
}

// Delete removes the cart entirely (used by cancel and, later, commit).
func (s *Store) Delete(cartID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.carts[cartID]; !ok {
		return ErrNotFound
	}
	s.removeLocked(cartID)
	return nil
}

// Count returns the number of live carts. Used by the metrics snapshot to
// report active sessions. Expired-but-not-yet-swept carts are counted (lazy
// expiry only fires on access), which is close enough for an operational gauge
// on a single-user kiosk.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.carts)
}

func (s *Store) getLocked(cartID string) (*Cart, error) {
	c, ok := s.carts[cartID]
	if !ok {
		return nil, ErrNotFound
	}
	if s.now().After(c.ExpiresAt) {
		s.removeLocked(cartID)
		return nil, ErrNotFound
	}
	return c, nil
}

// removeLocked drops a cart from both the primary and secondary
// indexes. Centralized so the lazy-expiry path in getLocked,
// StartByExternal's re-fire-after-expiry path, and explicit Delete
// stay in sync. Caller holds s.mu.
func (s *Store) removeLocked(cartID string) {
	c, ok := s.carts[cartID]
	if !ok {
		return
	}
	delete(s.carts, cartID)
	if c.UserCode != "" && c.DoorID != "" {
		// Only StartByExternal-created carts have a populated DoorID,
		// so the secondary index only ever holds those. UserCode is
		// part of the key; we check it defensively in case of a
		// future refactor that strips it.
		delete(s.byUserDoor, userDoorKey(c.UserCode, c.DoorID))
	}
}

func (s *Store) findLineLocked(lineID string) (*Cart, *Line) {
	for _, c := range s.carts {
		for _, l := range c.Lines {
			if l.ID == lineID {
				return c, l
			}
		}
	}
	return nil, nil
}

func (s *Store) touchLocked(c *Cart) {
	c.ExpiresAt = s.now().Add(s.idleTimeout)
}

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
