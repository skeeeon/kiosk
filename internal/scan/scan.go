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
	// OpenCount is the number of open_checkouts rows currently held by this
	// user. Populated on /scan responses so the SPA can welcome a worker
	// with context ("3 items out") without a second round-trip.
	OpenCount int `json:"open_count"`
}

type Item struct {
	ID             string `json:"id"`
	Code           string `json:"code"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Unit           string `json:"unit,omitempty"`
	TrackingMode   string `json:"tracking_mode"`
	Category       string `json:"category,omitempty"`
	Active         bool   `json:"active"`
	QuantityOnHand int    `json:"quantity_on_hand"`
	// OpenCount is how many physical units of this item are currently out
	// (sum across all holders). Useful for the splash identify panel; not
	// meaningful for consumables, which the SPA hides.
	OpenCount int `json:"open_count"`
	// Holder names the worker(s) currently holding open checkouts of this
	// item. One name when the item is single-issued; empty string for
	// consumables or items nobody has out. For multi-issue items the
	// handler picks one representative holder; the count above tells the
	// caller whether there are more.
	Holder string `json:"holder,omitempty"`
}

// ItemInstance represents one physical unit of a serialized item. Returned
// (with its parent Item) when a scan matches an instance's code or RFID.
type ItemInstance struct {
	ID      string `json:"id"`
	ItemID  string `json:"item_id"`
	Code    string `json:"code"`
	Serial  string `json:"serial,omitempty"`
	RFIDEPC string `json:"rfid_epc,omitempty"`
	Active  bool   `json:"active"`
	Notes   string `json:"notes,omitempty"`
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
//
// RFID lookups are instance-only — EPCs are per-tag and live on
// item_instances, never on the SKU itself.
type Lookups struct {
	UserByCode         func(code string) (*User, error)
	ItemByCode         func(code string) (*Item, error)
	ItemInstanceByCode func(code string) (*InstanceMatch, error)
	ItemInstanceByRFID func(epc string) (*InstanceMatch, error)
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
//     item code → instance rfid.
//  3. Otherwise: try instance code → item code → instance rfid → user code.
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
