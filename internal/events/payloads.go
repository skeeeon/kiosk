package events

import "time"

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
	return map[string]any{
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
}

// ItemActionInput holds the fields needed to emit one
// item.{checkout|return|consume} event.
type ItemActionInput struct {
	TransactionID string
	LineID        string
	KioskCode     string
	LocationCode  string
	UserID        string
	UserCode      string
	UserGroup     string
	ItemID        string
	ItemCode      string
	ItemName      string
	Action        string
	Qty           int
	Serial        string
	Uncorrelated  bool
	CompletedAt   time.Time
}

// BuildItemActionPayload renders the input into the map shape the publisher
// expects.
func BuildItemActionPayload(in ItemActionInput) map[string]any {
	return map[string]any{
		"transaction_id": in.TransactionID,
		"line_id":        in.LineID,
		"kiosk_code":     in.KioskCode,
		"location_code":  in.LocationCode,
		"user_id":        in.UserID,
		"user_code":      in.UserCode,
		"user_group":     in.UserGroup,
		"item_id":        in.ItemID,
		"item_code":      in.ItemCode,
		"item_name":      in.ItemName,
		"action":         in.Action,
		"qty":            in.Qty,
		"serial":         in.Serial,
		"uncorrelated":   in.Uncorrelated,
		"completed_at":   in.CompletedAt,
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

// InstanceLifecycleInput holds the fields needed to emit one instance.lifecycle
// event. Action is "create" / "decommission" / "reactivate" / "delete".
type InstanceLifecycleInput struct {
	InstanceID        string
	InstanceCode      string
	ItemID            string
	ItemCode          string
	ItemName          string
	KioskCode         string
	LocationCode      string
	Action            string // create | decommission | reactivate | delete
	PrevActive        bool
	NewActive         bool
	Reason            string
	Source            string // "local" | "controller"
	AdminID           string
	ControllerAdminID string
	CommandID         string
	CompletedAt       time.Time
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
		"prev_active":         in.PrevActive,
		"new_active":          in.NewActive,
		"reason":              in.Reason,
		"source":              in.Source,
		"admin_id":            in.AdminID,
		"controller_admin_id": in.ControllerAdminID,
		"command_id":          in.CommandID,
		"completed_at":        in.CompletedAt,
	}
}
