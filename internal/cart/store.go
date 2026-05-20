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
	ID                       string   `json:"id"`
	ItemID                   string   `json:"item_id"`
	ItemCode                 string   `json:"item_code"`
	ItemName                 string   `json:"item_name"`
	ItemType                 string   `json:"item_type"` // "tool" | "consumable"
	TrackingMode             string   `json:"tracking_mode"`
	Action                   string   `json:"action"` // "checkout" | "return" | "consume"
	Qty                      int      `json:"qty"`
	Serial                   string   `json:"serial,omitempty"`
	OriginalCheckoutUserID   string   `json:"original_checkout_user_id,omitempty"`
	OriginalCheckoutUserName string   `json:"original_checkout_user_name,omitempty"`
	Warnings                 []string `json:"warnings,omitempty"`
}

type Cart struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	UserCode  string    `json:"user_code"`
	UserName  string    `json:"user_name"`
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
	ErrNotFound        = errors.New("cart not found or expired")
	ErrLineNotFound    = errors.New("cart line not found")
	ErrQtyOutOfRange   = errors.New("qty must be between 1 and MaxQty")
	ErrInvalidAction   = errors.New("action is not valid for this item type")
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
}

func NewStore(idleTimeout time.Duration) *Store {
	return &Store{
		idleTimeout: idleTimeout,
		now:         time.Now,
		carts:       make(map[string]*Cart),
	}
}

// Start returns the user's existing non-expired cart if one exists; otherwise
// creates a new cart. Idempotent for the same user within the idle window.
func (s *Store) Start(userID, userCode, userName string) *Cart {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for id, c := range s.carts {
		if now.After(c.ExpiresAt) {
			delete(s.carts, id)
			continue
		}
		if c.UserID == userID {
			c.ExpiresAt = now.Add(s.idleTimeout)
			return c
		}
	}

	c := &Cart{
		ID:        newID(),
		UserID:    userID,
		UserCode:  userCode,
		UserName:  userName,
		StartedAt: now,
		ExpiresAt: now.Add(s.idleTimeout),
		Lines:     []*Line{},
	}
	s.carts[c.ID] = c
	return c
}

func (s *Store) Get(cartID string) (*Cart, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(cartID)
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
			if existing.ItemID == in.ItemID && existing.Action == in.Action {
				if existing.Qty+in.Qty > MaxQty {
					return nil, nil, ErrQtyOutOfRange
				}
				existing.Qty += in.Qty
				s.touchLocked(c)
				return c, existing, nil
			}
		}
	}

	in.ID = newID()
	c.Lines = append(c.Lines, in)
	s.touchLocked(c)
	return c, in, nil
}

// UpdateLine sets qty and/or action. Searches all carts by line ID since
// there's at most one cart per user, so the scan is trivial.
func (s *Store) UpdateLine(lineID string, qty *int, action *string) (*Cart, *Line, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, line := s.findLineLocked(lineID)
	if c == nil {
		return nil, nil, ErrLineNotFound
	}
	if s.now().After(c.ExpiresAt) {
		delete(s.carts, c.ID)
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
		delete(s.carts, c.ID)
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
	delete(s.carts, cartID)
	return nil
}

func (s *Store) getLocked(cartID string) (*Cart, error) {
	c, ok := s.carts[cartID]
	if !ok {
		return nil, ErrNotFound
	}
	if s.now().After(c.ExpiresAt) {
		delete(s.carts, cartID)
		return nil, ErrNotFound
	}
	return c, nil
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
