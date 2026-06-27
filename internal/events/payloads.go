package events

import "time"

// Adjustment / lifecycle source. Stamped on stock_adjustments, instance_audit,
// transactions, and the matching wire payloads to disambiguate "the local
// admin at the touchscreen" from "the central controller forwarded the
// command over NATS." Lives in this package because the events are where
// the value crosses the wire; commit/handlers/commands all consume the
// constants from here.
const (
	SourceLocal      = "local"
	SourceController = "controller"
)

// TransactionCompleteInput and ItemActionInput are the source-of-truth
// shapes for the two event payloads the kiosk emits. The commit hook (which
// has the values in hand at commit time) and the ledger republish handler
// (which reconstructs them from persisted records) both populate these
// structs, then call Build…Payload to produce the wire map.
//
// Centralizing the wire shape here prevents the "added a field to commit's
// payload but forgot to add it to republish" drift class: a new field is a
// one-place edit to the input struct + the builder, and both callers fail
// loudly at the input-construction site if they don't keep up.
//
// Keys + types of the returned map are part of the wire contract with the
// controller's aggregator (internal/controller/consumer.go's EventPayload).
// Payload key changes here require matching changes there.

// TransactionCompleteInput holds the fields needed to emit a
// transaction.complete event.
type TransactionCompleteInput struct {
	TransactionID string
	KioskCode     string
	LocationCode  string
	TerminalID    string // optional accepting/interacting terminal attribution; on the wire only when set
	EnclosureID   string // optional enclosure_diff cabinet attribution; on the wire only when set
	UserID        string
	UserCode      string
	UserName      string
	UserGroup     string
	StartedAt     time.Time
	CompletedAt   time.Time
	LinesCount    int
	CheckedOut    int
	Returned      int
	Consumed      int
}

// BuildTransactionCompletePayload renders the input into the map shape the
// publisher expects. Map keys + value types are stable across callers.
func BuildTransactionCompletePayload(in TransactionCompleteInput) map[string]any {
	p := map[string]any{
		"transaction_id": in.TransactionID,
		"kiosk_code":     in.KioskCode,
		"location_code":  in.LocationCode,
		"user_id":        in.UserID,
		"user_code":      in.UserCode,
		"user_name":      in.UserName,
		"user_group":     in.UserGroup,
		"started_at":     in.StartedAt,
		"completed_at":   in.CompletedAt,
		"lines_count":    in.LinesCount,
		"checked_out":    in.CheckedOut,
		"returned":       in.Returned,
		"consumed":       in.Consumed,
	}
	// Optional attribution tags — only ride the wire when set, so the common
	// single-kiosk payload is unchanged and the controller's omitempty decode
	// stays a no-op for un-tagged transactions.
	if in.TerminalID != "" {
		p["terminal_id"] = in.TerminalID
	}
	if in.EnclosureID != "" {
		p["enclosure_id"] = in.EnclosureID
	}
	return p
}

// ItemActionInput holds the fields needed to emit one
// item.{checkout|return|consume|admin_close} event.
//
// OriginalCheckoutUserCode is the user-code of the holder being returned to
// (for cross-user foreman returns) or empty for self/no-op cases. The
// controller's transaction_lines projection uses it to populate the
// original_checkout_user FK after a code → record lookup against its own
// (catalog-synced) users collection.
//
// ItemInstanceID is the kiosk-local item_instances.id string for serialized
// lines (and admin_closes on serialized rows). The controller stores it as
// opaque text; matching during open_checkouts projection is by equality
// within a kiosk_code scope, so the value just needs to be consistent
// between the checkout and the matching return.
type ItemActionInput struct {
	TransactionID            string
	LineID                   string
	KioskCode                string
	LocationCode             string
	UserID                   string
	UserCode                 string
	UserGroup                string
	ItemID                   string
	ItemCode                 string
	ItemName                 string
	Action                   string
	Qty                      int
	Serial                   string
	Uncorrelated             bool
	OriginalCheckoutUserCode string
	ItemInstanceID           string
	CompletedAt              time.Time
}

// BuildItemActionPayload renders the input into the map shape the publisher
// expects.
func BuildItemActionPayload(in ItemActionInput) map[string]any {
	return map[string]any{
		"transaction_id":              in.TransactionID,
		"line_id":                     in.LineID,
		"kiosk_code":                  in.KioskCode,
		"location_code":               in.LocationCode,
		"user_id":                     in.UserID,
		"user_code":                   in.UserCode,
		"user_group":                  in.UserGroup,
		"item_id":                     in.ItemID,
		"item_code":                   in.ItemCode,
		"item_name":                   in.ItemName,
		"action":                      in.Action,
		"qty":                         in.Qty,
		"serial":                      in.Serial,
		"uncorrelated":                in.Uncorrelated,
		"original_checkout_user_code": in.OriginalCheckoutUserCode,
		"item_instance_id":            in.ItemInstanceID,
		"completed_at":                in.CompletedAt,
	}
}

// InventoryAdjustInput holds the fields needed to emit an inventory.adjust
// event. Both kiosk-local admin adjustments and the qty side-effect of an
// admin_close go through the same builder so the wire shape stays consistent.
type InventoryAdjustInput struct {
	AdjustmentID      string
	KioskCode         string
	LocationCode      string
	ItemID            string
	ItemCode          string
	ItemName          string
	Mode              string
	Value             int
	Delta             int
	PrevQuantity      int
	NewQuantity       int
	Reason            string
	Source            string // "local" | "controller"
	AdminID           string
	ControllerAdminID string
	CommandID         string
	CompletedAt       time.Time
}

// BuildInventoryAdjustPayload renders the input into the map shape the
// publisher emits. Keys mirror EventPayload in internal/controller/consumer.go.
func BuildInventoryAdjustPayload(in InventoryAdjustInput) map[string]any {
	return map[string]any{
		"adjustment_id":       in.AdjustmentID,
		"kiosk_code":          in.KioskCode,
		"location_code":       in.LocationCode,
		"item_id":             in.ItemID,
		"item_code":           in.ItemCode,
		"item_name":           in.ItemName,
		"mode":                in.Mode,
		"value":               in.Value,
		"delta":               in.Delta,
		"prev_quantity":       in.PrevQuantity,
		"new_quantity":        in.NewQuantity,
		"reason":              in.Reason,
		"source":              in.Source,
		"admin_id":            in.AdminID,
		"controller_admin_id": in.ControllerAdminID,
		"command_id":          in.CommandID,
		"completed_at":        in.CompletedAt,
	}
}

// AdminCloseInput holds the fields needed to emit a checkout.admin_close
// event. Source disambiguates the actor population:
//   - "local" → AdminID is a PB record id in the kiosk's own admins collection.
//   - "controller" → ControllerAdminID is the controller admin's PB record id
//     (which doesn't exist in the kiosk's DB), AdminID is empty.
type AdminCloseInput struct {
	TransactionID     string
	LineID            string
	KioskCode         string
	LocationCode      string
	OpenCheckoutID    string
	ItemID            string
	ItemCode          string
	ItemName          string
	UserID            string // worker whose row was closed
	UserCode          string
	UserGroup         string
	ItemInstanceID    string
	Serial            string
	Qty               int
	ClosureReason     string
	Notes             string
	Source            string // "local" | "controller"
	AdminID           string
	ControllerAdminID string
	CommandID         string
	CompletedAt       time.Time
}

// BuildAdminClosePayload renders the input into the map shape the publisher
// emits. Keys mirror the inventory.adjust wire shape (admin_id /
// controller_admin_id / source / command_id) so downstream consumers can
// reuse the same actor-resolution logic.
func BuildAdminClosePayload(in AdminCloseInput) map[string]any {
	return map[string]any{
		"transaction_id":      in.TransactionID,
		"line_id":             in.LineID,
		"kiosk_code":          in.KioskCode,
		"location_code":       in.LocationCode,
		"open_checkout_id":    in.OpenCheckoutID,
		"item_id":             in.ItemID,
		"item_code":           in.ItemCode,
		"item_name":           in.ItemName,
		"user_id":             in.UserID,
		"user_code":           in.UserCode,
		"user_group":          in.UserGroup,
		"item_instance_id":    in.ItemInstanceID,
		"serial":              in.Serial,
		"qty":                 in.Qty,
		"closure_reason":      in.ClosureReason,
		"notes":               in.Notes,
		"source":              in.Source,
		"admin_id":            in.AdminID,
		"controller_admin_id": in.ControllerAdminID,
		"command_id":          in.CommandID,
		"completed_at":        in.CompletedAt,
	}
}

// TimeclockPunchInput holds the fields needed to emit one timeclock.punch
// event. Both the live punch funnel (which has the values in hand) and the
// timeclock.republish walk (which reconstructs them from persisted rows)
// populate this struct — same anti-drift strategy as the transaction events.
//
// PunchID is the kiosk-side time_punches.id and the controller-side
// idempotency anchor (projected as source_punch_id, unique when non-empty).
// Source is one of timeclock's self/foreman/admin/controller_admin — richer
// than the SourceLocal/SourceController pair because punches carry "who
// physically initiated this" semantics. Exactly one of RecordedByUserCode /
// AdminID / ControllerAdminID is set for non-self sources.
type TimeclockPunchInput struct {
	PunchID            string
	KioskCode          string
	LocationCode       string
	UserID             string
	UserCode           string
	UserName           string
	Direction          string // "in" | "out"
	OccurredAt         time.Time
	Source             string // self | foreman | admin | controller_admin
	RecordedByUserCode string // foreman's user code, when Source == "foreman"
	AdminID            string
	ControllerAdminID  string
	Reason             string
	Force              bool
	CommandID          string
	JobCode            string    // optional job/work-order tag
	RecordedAt         time.Time // when the row was written (≠ OccurredAt for backdated punches)
}

// BuildTimeclockPunchPayload renders the input into the map shape the
// publisher emits. Keys mirror EventPayload in internal/controller/consumer.go.
func BuildTimeclockPunchPayload(in TimeclockPunchInput) map[string]any {
	return map[string]any{
		"punch_id":              in.PunchID,
		"kiosk_code":            in.KioskCode,
		"location_code":         in.LocationCode,
		"user_id":               in.UserID,
		"user_code":             in.UserCode,
		"user_name":             in.UserName,
		"direction":             in.Direction,
		"occurred_at":           in.OccurredAt,
		"source":                in.Source,
		"recorded_by_user_code": in.RecordedByUserCode,
		"admin_id":              in.AdminID,
		"controller_admin_id":   in.ControllerAdminID,
		"reason":                in.Reason,
		"force":                 in.Force,
		"command_id":            in.CommandID,
		"job_code":              in.JobCode,
		"recorded_at":           in.RecordedAt,
	}
}

// InstanceLifecycleInput holds the fields needed to emit one instance.lifecycle
// event. Action is "create" / "to_maintenance" / "return_to_service" /
// "retire" / "unretire". PrevStatus/NewStatus are the item_instances.status
// enum (in_service | maintenance | retired); PrevStatus is empty for create.
//
// SourceAuditID is the kiosk-side instance_audit.id of the row this event
// corresponds to. It's the idempotency anchor for the controller-side
// projection — unique-when-non-empty on instance_lifecycle_audit so
// JetStream redelivery is a no-op.
type InstanceLifecycleInput struct {
	InstanceID        string
	InstanceCode      string
	ItemID            string
	ItemCode          string
	ItemName          string
	KioskCode         string
	LocationCode      string
	Action            string // create | to_maintenance | return_to_service | retire | unretire
	PrevStatus        string
	NewStatus         string
	Reason            string
	Source            string // "local" | "controller"
	AdminID           string
	ControllerAdminID string
	CommandID         string
	SourceAuditID     string
	CompletedAt       time.Time
	// RFIDEPC is the unit's current tag id, threaded so the controller can build
	// its EPC → owning-unit index (location/sightings L3). Empty for untagged
	// units. Advisory: a bare cosmetic EPC change emits no lifecycle event, so
	// the index refreshes on the next create/status transition.
	RFIDEPC string
}

// BuildInstanceLifecyclePayload renders the input into the map shape the
// publisher emits.
func BuildInstanceLifecyclePayload(in InstanceLifecycleInput) map[string]any {
	return map[string]any{
		"instance_id":         in.InstanceID,
		"instance_code":       in.InstanceCode,
		"item_id":             in.ItemID,
		"item_code":           in.ItemCode,
		"item_name":           in.ItemName,
		"kiosk_code":          in.KioskCode,
		"location_code":       in.LocationCode,
		"action":              in.Action,
		"prev_status":         in.PrevStatus,
		"new_status":          in.NewStatus,
		"reason":              in.Reason,
		"source":              in.Source,
		"admin_id":            in.AdminID,
		"controller_admin_id": in.ControllerAdminID,
		"command_id":          in.CommandID,
		"source_audit_id":     in.SourceAuditID,
		"completed_at":        in.CompletedAt,
		"rfid_epc":            in.RFIDEPC,
	}
}

// SightingPayload is the ONE wire contract for the lossy `sighting` family
// (docs/location-sightings-plan.md). It is always RAW: TagID is the observed
// tag id (RFID EPC today, BLE beacon id later) — never a resolved instance
// code, because an external gateway doesn't know our instance codes. Whoever
// subscribes resolves it (standalone node via the scan resolver; controller via
// an EPC index). The same struct serves both publish (marshal) and ingest
// (unmarshal): external gateways publish JSON matching these keys, the node's
// managed-mode custody-read publish marshals it, and every subscriber parses
// it. Lat/Lon/RSSI are pointers so a zone-only sighting omits them; ObservedAt
// is RFC3339 (a publisher that omits it gets defaulted to now at ingest —
// advisory, lossy).
type SightingPayload struct {
	TagID      string    `json:"tag_id"`
	GatewayID  string    `json:"gateway_id,omitempty"`
	Zone       string    `json:"zone,omitempty"`
	Lat        *float64  `json:"lat,omitempty"`
	Lon        *float64  `json:"lon,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	RSSI       *int      `json:"rssi,omitempty"`
}
