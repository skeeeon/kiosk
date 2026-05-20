// Package scan resolves raw barcode/QR scan values into either a user, an
// item, or "unknown". The resolution order encodes the kiosk's disambiguation
// policy: explicit prefix wins; otherwise we try items first (since they're
// scanned far more often than badges) and fall back to users.
package scan

import "strings"

type User struct {
	ID    string `json:"id"`
	Code  string `json:"code"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	Email string `json:"email,omitempty"`
}

type Item struct {
	ID           string `json:"id"`
	Code         string `json:"code"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	Unit         string `json:"unit,omitempty"`
	TrackingMode string `json:"tracking_mode"`
	Serial       string `json:"serial,omitempty"`
	Category     string `json:"category,omitempty"`
}

type ResultType string

const (
	ResultUser    ResultType = "user"
	ResultItem    ResultType = "item"
	ResultUnknown ResultType = "unknown"
)

// Result is the JSON shape returned by /api/kiosk/scan. Record holds either
// *User, *Item, or nil (when Type is Unknown).
type Result struct {
	Type   ResultType `json:"type"`
	Record any        `json:"record,omitempty"`
	Value  string     `json:"value,omitempty"`
}

// Lookups is the data-access side of resolution, injected so the resolver
// can be unit-tested without a database. All three functions return (nil, nil)
// when no record matches; any non-nil error short-circuits with Unknown.
type Lookups struct {
	UserByCode func(code string) (*User, error)
	ItemByCode func(code string) (*Item, error)
	ItemByRFID func(epc string) (*Item, error)
}

type Resolver struct {
	UserPrefix string
	ItemPrefix string
	Lookups    Lookups
}

// Resolve dispatches a raw scan value to one of: user, item, or unknown.
//
// Order:
//  1. If UserPrefix is set and value has it: strip and look up user only.
//  2. If ItemPrefix is set and value has it: strip and try item code then rfid.
//  3. Otherwise: try item code → item rfid → user code.
func (r *Resolver) Resolve(raw string) Result {
	value := strings.TrimSpace(raw)
	if value == "" {
		return Result{Type: ResultUnknown}
	}

	if r.UserPrefix != "" && strings.HasPrefix(value, r.UserPrefix) {
		code := strings.TrimPrefix(value, r.UserPrefix)
		if u, _ := r.Lookups.UserByCode(code); u != nil {
			return Result{Type: ResultUser, Record: u}
		}
		return Result{Type: ResultUnknown, Value: value}
	}

	if r.ItemPrefix != "" && strings.HasPrefix(value, r.ItemPrefix) {
		code := strings.TrimPrefix(value, r.ItemPrefix)
		if i, _ := r.Lookups.ItemByCode(code); i != nil {
			return Result{Type: ResultItem, Record: i}
		}
		if i, _ := r.Lookups.ItemByRFID(code); i != nil {
			return Result{Type: ResultItem, Record: i}
		}
		return Result{Type: ResultUnknown, Value: value}
	}

	if i, _ := r.Lookups.ItemByCode(value); i != nil {
		return Result{Type: ResultItem, Record: i}
	}
	if i, _ := r.Lookups.ItemByRFID(value); i != nil {
		return Result{Type: ResultItem, Record: i}
	}
	if u, _ := r.Lookups.UserByCode(value); u != nil {
		return Result{Type: ResultUser, Record: u}
	}
	return Result{Type: ResultUnknown, Value: value}
}
