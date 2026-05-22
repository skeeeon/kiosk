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
