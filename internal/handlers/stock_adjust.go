package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// AdjustItemStock is the admin endpoint that changes an item's
// quantity_on_hand while writing an audit row in the same transaction.
//
//   POST /api/kiosk/items/{id}/adjust
//   body: { mode: "delta" | "absolute", value: int, reason: string }
//
// Both fields of the item update and the audit insert succeed or fail
// together. Direct PB edits to items.quantity_on_hand are still allowed —
// admins are trusted — but skip the audit log. The admin UI's "Adjust"
// button always goes through here, which is the path that needs the trail.
func (h *Handlers) AdjustItemStock(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	itemID := re.Request.PathValue("id")
	if itemID == "" {
		return re.BadRequestError("item id is required", nil)
	}

	var body struct {
		Mode   string `json:"mode"`
		Value  int    `json:"value"`
		Reason string `json:"reason"`
	}
	if err := re.BindBody(&body); err != nil {
		return re.BadRequestError("invalid request body", err)
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if body.Reason == "" {
		return re.BadRequestError("reason is required", nil)
	}
	if body.Mode != "delta" && body.Mode != "absolute" {
		return re.BadRequestError("mode must be 'delta' or 'absolute'", nil)
	}

	result, err := PerformStockAdjustment(h.App, itemID, re.Auth.Id, body.Mode, body.Value, body.Reason)
	if err != nil {
		if isNotFound(err) {
			return re.NotFoundError("item not found", nil)
		}
		return re.InternalServerError("adjustment failed", err)
	}
	return re.JSON(http.StatusOK, result)
}

// StockAdjustmentResult is what the endpoint returns and what the tests
// inspect — keeps the HTTP layer thin.
type StockAdjustmentResult struct {
	AdjustmentID string `json:"adjustment_id"`
	ItemID       string `json:"item_id"`
	Delta        int    `json:"delta"`
	NewQuantity  int    `json:"new_quantity"`
	PrevQuantity int    `json:"prev_quantity"`
}

// PerformStockAdjustment runs the item update + audit insert atomically.
// Exported so it can be exercised by unit tests without spinning up the
// HTTP stack. Validates inputs minimally; the HTTP layer does the rest.
func PerformStockAdjustment(app core.App, itemID, adminID, mode string, value int, reason string) (*StockAdjustmentResult, error) {
	if mode != "delta" && mode != "absolute" {
		return nil, fmt.Errorf("invalid mode %q", mode)
	}
	if reason == "" {
		return nil, fmt.Errorf("reason is required")
	}
	if adminID == "" {
		return nil, fmt.Errorf("admin id is required")
	}

	var out StockAdjustmentResult
	err := app.RunInTransaction(func(tx core.App) error {
		item, err := tx.FindRecordById("items", itemID)
		if err != nil {
			return err
		}
		prev := item.GetInt("quantity_on_hand")
		var newQty, delta int
		if mode == "delta" {
			delta = value
			newQty = prev + value
		} else {
			newQty = value
			delta = value - prev
		}

		item.Set("quantity_on_hand", newQty)
		if err := tx.Save(item); err != nil {
			return fmt.Errorf("update item quantity: %w", err)
		}

		adjustments, err := tx.FindCollectionByNameOrId("stock_adjustments")
		if err != nil {
			return fmt.Errorf("find stock_adjustments collection: %w", err)
		}
		adj := core.NewRecord(adjustments)
		adj.Set("item", item.Id)
		adj.Set("delta", delta)
		adj.Set("new_quantity", newQty)
		adj.Set("reason", reason)
		adj.Set("admin", adminID)
		if err := tx.Save(adj); err != nil {
			return fmt.Errorf("save stock_adjustment: %w", err)
		}

		out = StockAdjustmentResult{
			AdjustmentID: adj.Id,
			ItemID:       item.Id,
			Delta:        delta,
			NewQuantity:  newQty,
			PrevQuantity: prev,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
