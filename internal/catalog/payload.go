// Package catalog defines the wire shape of catalog records as they travel
// between the central kiosk-controller and the fleet of kiosks over NATS
// JetStream KV. Both the controller (publisher) and the kiosk (projector)
// import this package so the encoding lives in exactly one place.
//
// Only the fields that should sync are carried — system fields (id, created,
// updated), auth secrets (password, tokenKey), and kiosk-local state
// (quantity_on_hand, reorder_threshold) are deliberately excluded.
package catalog

import (
	"encoding/json"
	"fmt"
)

// ItemPayload is the cross-fleet view of an items record. `Code` is the
// natural join key; the same SKU at multiple kiosks shares this code but
// each kiosk holds its own row (with its own qty / instances) locally.
//
// RFID EPCs and per-unit serials live on item_instances, not on the SKU,
// so neither field is part of this payload.
type ItemPayload struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	Type         string `json:"type"`          // "tool" | "consumable"
	Unit         string `json:"unit,omitempty"`
	TrackingMode string `json:"tracking_mode"` // "quantity" | "serialized"
	Category     string `json:"category,omitempty"`
	Active       bool   `json:"active"`
	Notes        string `json:"notes,omitempty"`
}

// UserPayload is the cross-fleet view of a users record. Password and
// PB-internal auth fields are intentionally absent — workers don't log in
// in v1, and projecting an opaque random password locally is enough to
// satisfy PB's auth-collection constraints without exposing a vector.
type UserPayload struct {
	Code   string `json:"code"`
	Name   string `json:"name"`
	Email  string `json:"email,omitempty"`
	Role   string `json:"role"` // "worker" | "foreman"
	Active bool   `json:"active"`
}

// MarshalItem encodes an item for storage in the catalog_items KV bucket.
func MarshalItem(p ItemPayload) ([]byte, error) {
	if p.Code == "" {
		return nil, fmt.Errorf("item payload missing code")
	}
	return json.Marshal(p)
}

// UnmarshalItem decodes a KV value into an ItemPayload.
func UnmarshalItem(data []byte) (ItemPayload, error) {
	var p ItemPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return ItemPayload{}, fmt.Errorf("decode item payload: %w", err)
	}
	if p.Code == "" {
		return ItemPayload{}, fmt.Errorf("item payload missing code")
	}
	return p, nil
}

// MarshalUser encodes a user for storage in the catalog_users KV bucket.
func MarshalUser(p UserPayload) ([]byte, error) {
	if p.Code == "" {
		return nil, fmt.Errorf("user payload missing code")
	}
	return json.Marshal(p)
}

// UnmarshalUser decodes a KV value into a UserPayload.
func UnmarshalUser(data []byte) (UserPayload, error) {
	var p UserPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return UserPayload{}, fmt.Errorf("decode user payload: %w", err)
	}
	if p.Code == "" {
		return UserPayload{}, fmt.Errorf("user payload missing code")
	}
	return p, nil
}

// Bucket names. Centralized so both sides agree without a config dance.
const (
	ItemsBucket = "catalog_items"
	UsersBucket = "catalog_users"
)
