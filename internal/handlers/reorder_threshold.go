package handlers

import (
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// ReorderThresholdResult is what the set-threshold path returns. Prev is
// carried so the controller's SPA (and an operator reading a log line) can
// see what changed without a follow-up snapshot.
type ReorderThresholdResult struct {
	ItemID           string `json:"item_id"`
	ItemCode         string `json:"item_code"`
	ReorderThreshold int    `json:"reorder_threshold"`
	PrevThreshold    int    `json:"prev_threshold"`
}

// PerformSetReorderThreshold sets one SKU's low-stock alert level at THIS
// kiosk.
//
// The threshold is deliberately kiosk-local (see the package comment on
// internal/catalog): a busy main crib and a quiet cross-dock stocking the
// same SKU want different levels, and the alert fires against the kiosk's
// own available count. It therefore does not cross the catalogue wire, and
// until this existed a controller-managed kiosk had no way to receive one
// at all — the controller's Inventory panel rendered a "Reorder ≤" column
// it could not fill, and low-stock alerting could not fire anywhere in a
// managed fleet.
//
// Three things it deliberately does NOT do, each for a reason:
//
//   - No command_id. The operation is absolute ("set it to 3"), so a
//     replay converges on the same value. There is nothing for an
//     idempotency key to protect, unlike a delta-capable stock adjustment.
//   - No audit row. stock_adjustments is a ledger of physical count
//     changes; a threshold is a policy knob and moves nothing. Inventing an
//     audit table for it would be inventing a requirement.
//   - No event. The controller does not project item state — its Inventory
//     panel reads a live inventory.snapshot, which already carries
//     reorder_threshold, so the new value shows up on the next refresh
//     with nothing extra on the wire.
//
// Serialized SKUs are allowed, unlike PerformStockAdjustment: their
// quantity_on_hand is derived, but the threshold is an alert level against
// it and is perfectly meaningful ("tell me when fewer than two drills are
// available").
func PerformSetReorderThreshold(app core.App, itemCode string, value int) (*ReorderThresholdResult, error) {
	if itemCode == "" {
		return nil, fmt.Errorf("item_code is required")
	}
	if value < 0 {
		return nil, fmt.Errorf("reorder_threshold must be zero or greater (got %d)", value)
	}

	item, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": itemCode})
	if err != nil {
		return nil, err
	}

	prev := item.GetInt("reorder_threshold")
	if prev != value {
		item.Set("reorder_threshold", value)
		if err := app.Save(item); err != nil {
			return nil, fmt.Errorf("save item: %w", err)
		}
	}

	return &ReorderThresholdResult{
		ItemID:           item.Id,
		ItemCode:         item.GetString("code"),
		ReorderThreshold: value,
		PrevThreshold:    prev,
	}, nil
}
