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

// ItemInstance represents one physical unit of a serialized item. Returned
// (with its parent Item) when a scan matches an instance's code or RFID.
type ItemInstance struct {
	ID       string `json:"id"`
	ItemID   string `json:"item_id"`
	Code     string `json:"code"`
	Serial   string `json:"serial,omitempty"`
	RFIDEPC  string `json:"rfid_epc,omitempty"`
	Active   bool   `json:"active"`
	Notes    string `json:"notes,omitempty"`
}

// InstanceMatch bundles an instance with its already-loaded parent item so
// the caller doesn't have to make a second round-trip.
type InstanceMatch struct {
	Instance *ItemInstance `json:"instance"`
	Item     *Item         `json:"item"`
}

type ResultType string

const (
	ResultUser         ResultType = "user"
	ResultItem         ResultType = "item"
	ResultItemInstance ResultType = "item_instance"
	ResultUnknown      ResultType = "unknown"
)

// Result is the JSON shape returned by /api/kiosk/scan. Record holds either
// *User, *Item, *InstanceMatch, or nil (when Type is Unknown).
type Result struct {
	Type   ResultType `json:"type"`
	Record any        `json:"record,omitempty"`
	Value  string     `json:"value,omitempty"`
}

// Lookups is the data-access side of resolution, injected so the resolver
// can be unit-tested without a database. Functions return (nil, nil) when
// no record matches; any non-nil error short-circuits with Unknown.
type Lookups struct {
	UserByCode             func(code string) (*User, error)
	ItemByCode             func(code string) (*Item, error)
	ItemByRFID             func(epc string) (*Item, error)
	ItemInstanceByCode     func(code string) (*InstanceMatch, error)
	ItemInstanceByRFID     func(epc string) (*InstanceMatch, error)
}

type Resolver struct {
	UserPrefix string
	ItemPrefix string
	Lookups    Lookups
}

// Resolve dispatches a raw scan value to one of: user, item, item_instance,
// or unknown.
//
// Order: instance lookups precede item lookups so a more-specific match
// (e.g., a barcode on a physical unit) wins over a less-specific one
// (e.g., the SKU code happening to collide).
//
//  1. If UserPrefix is set and value has it: strip and look up user only.
//  2. If ItemPrefix is set and value has it: strip and try instance code →
//     item code → instance rfid → item rfid.
//  3. Otherwise: try instance code → item code → instance rfid → item rfid
//     → user code.
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
		if m, _ := tryInstanceByCode(r.Lookups, code); m != nil {
			return Result{Type: ResultItemInstance, Record: m}
		}
		if i, _ := r.Lookups.ItemByCode(code); i != nil {
			return Result{Type: ResultItem, Record: i}
		}
		if m, _ := tryInstanceByRFID(r.Lookups, code); m != nil {
			return Result{Type: ResultItemInstance, Record: m}
		}
		if i, _ := r.Lookups.ItemByRFID(code); i != nil {
			return Result{Type: ResultItem, Record: i}
		}
		return Result{Type: ResultUnknown, Value: value}
	}

	if m, _ := tryInstanceByCode(r.Lookups, value); m != nil {
		return Result{Type: ResultItemInstance, Record: m}
	}
	if i, _ := r.Lookups.ItemByCode(value); i != nil {
		return Result{Type: ResultItem, Record: i}
	}
	if m, _ := tryInstanceByRFID(r.Lookups, value); m != nil {
		return Result{Type: ResultItemInstance, Record: m}
	}
	if i, _ := r.Lookups.ItemByRFID(value); i != nil {
		return Result{Type: ResultItem, Record: i}
	}
	if u, _ := r.Lookups.UserByCode(value); u != nil {
		return Result{Type: ResultUser, Record: u}
	}
	return Result{Type: ResultUnknown, Value: value}
}

// tryInstanceByCode and tryInstanceByRFID guard against a nil lookup so old
// tests / callers that don't populate the new fields continue to work.
func tryInstanceByCode(l Lookups, v string) (*InstanceMatch, error) {
	if l.ItemInstanceByCode == nil {
		return nil, nil
	}
	return l.ItemInstanceByCode(v)
}

func tryInstanceByRFID(l Lookups, v string) (*InstanceMatch, error) {
	if l.ItemInstanceByRFID == nil {
		return nil, nil
	}
	return l.ItemInstanceByRFID(v)
}
