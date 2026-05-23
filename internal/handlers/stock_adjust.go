package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/dberr"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// Adjustment sources. The kiosk distinguishes "the local admin clicked
// Adjust at the touchscreen" from "the central controller forwarded an
// inventory.adjust command over NATS" so the audit log answers "who and
// from where" in one place.
const (
	SourceLocal      = "local"
	SourceController = "controller"
)

// errIdempotentReplay is a sentinel returned from inside the txn callback
// when the dispatcher recognises a duplicate command_id. The outer code
// catches it and returns the prior result — the (empty) txn is rolled back
// silently because no writes happened in this attempt.
var errIdempotentReplay = errors.New("idempotent replay")

// AdjustItemStock is the admin endpoint that changes an item's
// quantity_on_hand while writing an audit row in the same transaction.
//
//	POST /api/kiosk/items/{id}/adjust
//	body: { mode: "delta" | "absolute", value: int, reason: string }
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

	// Local path: no command_id (idempotency is browser-side via the SPA's
	// disabled-button trick — admins aren't retrying CLI requests).
	result, err := PerformStockAdjustment(h.App, itemID, re.Auth.Id, SourceLocal, "",
		body.Mode, body.Value, body.Reason)
	if err != nil {
		if isNotFound(err) {
			return re.NotFoundError("item not found", nil)
		}
		return re.InternalServerError("adjustment failed", err)
	}

	PublishInventoryAdjustEvent(h.App, result, SourceLocal, re.Auth.Id,
		body.Mode, body.Value, body.Reason)

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
// Exported so it can be exercised by unit tests AND by the NATS command
// dispatcher (which calls this from internal/commands/ when the central
// controller forwards a remote adjust). The function signature carries:
//
//   - actorID: the PB record ID of whoever initiated the change.
//   - source: SourceLocal (writes actorID to the kiosk's `admin` FK) or
//     SourceController (writes actorID to controller_admin_id, leaves
//     `admin` null because the controller's admin doesn't exist in the
//     kiosk's PB).
//   - commandID: idempotency key for remote commands. Empty for local
//     adjustments; non-empty values are unique-indexed in the schema, so
//     a replayed command returns the prior result instead of double-
//     applying. Always empty when source == SourceLocal.
func PerformStockAdjustment(app core.App, itemID, actorID, source, commandID, mode string, value int, reason string) (*StockAdjustmentResult, error) {
	if mode != "delta" && mode != "absolute" {
		return nil, fmt.Errorf("invalid mode %q", mode)
	}
	if reason == "" {
		return nil, fmt.Errorf("reason is required")
	}
	if actorID == "" {
		return nil, fmt.Errorf("actor id is required")
	}
	if source != SourceLocal && source != SourceController {
		return nil, fmt.Errorf("invalid source %q", source)
	}

	var out StockAdjustmentResult
	err := app.RunInTransaction(func(tx core.App) error {
		// Idempotency: a remote command_id that's already been processed
		// must return its prior result without re-applying. Check up
		// front so we don't re-add the delta on top of a quantity that
		// already reflects this command.
		if commandID != "" {
			existing, lerr := tx.FindFirstRecordByFilter("stock_adjustments",
				"command_id = {:c}", dbx.Params{"c": commandID})
			if lerr == nil && existing != nil {
				delta := existing.GetInt("delta")
				newQty := existing.GetInt("new_quantity")
				out = StockAdjustmentResult{
					AdjustmentID: existing.Id,
					ItemID:       existing.GetString("item"),
					Delta:        delta,
					NewQuantity:  newQty,
					PrevQuantity: newQty - delta,
				}
				return errIdempotentReplay
			}
			if lerr != nil && !dberr.IsNotFound(lerr) {
				return fmt.Errorf("idempotency lookup: %w", lerr)
			}
		}

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
		adj.Set("source", source)
		switch source {
		case SourceLocal:
			adj.Set("admin", actorID)
		case SourceController:
			adj.Set("controller_admin_id", actorID)
		}
		if commandID != "" {
			adj.Set("command_id", commandID)
		}
		if err := tx.Save(adj); err != nil {
			// Concurrent insert under the same command_id (vanishingly
			// rare with SQLite's single-writer, but defensive). The
			// whole txn rolls back together — including the item
			// update — so the duplicate doesn't apply twice.
			if commandID != "" && dberr.IsUniqueViolation(err) {
				return errIdempotentReplay
			}
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
	if errors.Is(err, errIdempotentReplay) {
		// The fast path (upfront lookup) populated `out` directly. The
		// slow path (concurrent-insert race) needs a re-fetch outside
		// the rolled-back txn — `out` will be zero in that case.
		if out.AdjustmentID != "" {
			return &out, nil
		}
		return fetchAdjustmentByCommandID(app, commandID)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// fetchAdjustmentByCommandID re-reads the prior row after a unique-violation
// rollback so the caller still gets a consistent result.
func fetchAdjustmentByCommandID(app core.App, commandID string) (*StockAdjustmentResult, error) {
	rec, err := app.FindFirstRecordByFilter("stock_adjustments",
		"command_id = {:c}", dbx.Params{"c": commandID})
	if err != nil {
		return nil, fmt.Errorf("idempotent replay re-fetch: %w", err)
	}
	delta := rec.GetInt("delta")
	newQty := rec.GetInt("new_quantity")
	return &StockAdjustmentResult{
		AdjustmentID: rec.Id,
		ItemID:       rec.GetString("item"),
		Delta:        delta,
		NewQuantity:  newQty,
		PrevQuantity: newQty - delta,
	}, nil
}

// PublishInventoryAdjustEvent emits the inventory.adjust NATS event after a
// successful PerformStockAdjustment. Factored out of the HTTP handler so the
// NATS command dispatcher can call the same publish path — the controller
// aggregator receives one event shape regardless of whether the adjustment
// originated from the local UI or from a remote command.
//
// Item code/name are re-fetched (cheap single-row read) so callers don't
// have to thread the item record through. Empty result or item-not-found
// is silently skipped — the event is best-effort.
func PublishInventoryAdjustEvent(app core.App, result *StockAdjustmentResult, source, actorID, mode string, value int, reason string) {
	if result == nil {
		return
	}
	item, err := app.FindRecordById("items", result.ItemID)
	if err != nil {
		return
	}
	id := kioskctx.Get()
	payload := map[string]any{
		"adjustment_id": result.AdjustmentID,
		"kiosk_code":    id.KioskCode,
		"location_code": id.LocationCode,
		"item_id":       result.ItemID,
		"item_code":     item.GetString("code"),
		"item_name":     item.GetString("name"),
		"mode":          mode,
		"value":         value,
		"delta":         result.Delta,
		"prev_quantity": result.PrevQuantity,
		"new_quantity":  result.NewQuantity,
		"reason":        reason,
		"source":        source,
		"completed_at":  time.Now().UTC(),
	}
	// admin_id keeps its historic meaning ("the local admin's PB record id")
	// for back-compat with the controller's existing audit-log handler;
	// controller_admin_id is only populated for source=controller rows so
	// downstream consumers can tell which population the ID lives in.
	switch source {
	case SourceLocal:
		payload["admin_id"] = actorID
	case SourceController:
		payload["controller_admin_id"] = actorID
	}
	events.Publish(events.InventoryAdjustSubject(id.KioskCode), payload)
}
