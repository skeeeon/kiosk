package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/dberr"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/handlers"
)

// inventoryAdjustRequest is the payload the controller sends. command_id is
// the idempotency anchor — the controller generates a UUID per attempt and
// retries with the same value if the original reply was lost. item_code
// (not item_id) is what travels on the wire because the controller doesn't
// know the kiosk's local PB record IDs.
type inventoryAdjustRequest struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	ItemCode          string `json:"item_code"`
	Mode              string `json:"mode"`
	Value             int    `json:"value"`
	Reason            string `json:"reason"`
}

// inventoryAdjustReply is what the controller endpoint forwards back to the
// SPA after a successful adjust. Field names match StockAdjustmentResult so
// the controller can pass them through without remapping.
type inventoryAdjustReply struct {
	AdjustmentID string `json:"adjustment_id"`
	ItemID       string `json:"item_id"`
	ItemCode     string `json:"item_code"`
	Delta        int    `json:"delta"`
	NewQuantity  int    `json:"new_quantity"`
	PrevQuantity int    `json:"prev_quantity"`
}

// handleInventoryAdjust executes a controller→kiosk inventory adjustment.
// Mirrors the local AdjustItemStock HTTP handler's flow except: input is
// JSON over NATS, the item lookup is by code (not id), and the audit row
// is tagged source='controller' with the supplied command_id. The event
// published afterward is identical in shape to a local adjust — the
// controller's aggregator can't tell them apart at the wire.
func (d *Dispatcher) handleInventoryAdjust(_ context.Context, payload []byte) Reply {
	var req inventoryAdjustRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return Reply{Success: false, Error: "invalid request body: " + err.Error()}
	}
	if req.CommandID == "" {
		return Reply{Success: false, Error: "command_id is required"}
	}
	if req.ControllerAdminID == "" {
		return Reply{Success: false, Error: "controller_admin_id is required"}
	}
	if req.ItemCode == "" {
		return Reply{Success: false, Error: "item_code is required"}
	}
	if req.Mode != "delta" && req.Mode != "absolute" {
		return Reply{Success: false, Error: "mode must be 'delta' or 'absolute'"}
	}
	if req.Reason == "" {
		return Reply{Success: false, Error: "reason is required"}
	}

	item, err := d.app.FindFirstRecordByFilter("items",
		"code = {:c}", dbx.Params{"c": req.ItemCode})
	if err != nil {
		if dberr.IsNotFound(err) {
			return Reply{Success: false, Error: fmt.Sprintf("item_code %q not found", req.ItemCode)}
		}
		return Reply{Success: false, Error: "item lookup failed: " + err.Error()}
	}

	result, err := handlers.PerformStockAdjustment(d.app, item.Id, req.ControllerAdminID,
		events.SourceController, req.CommandID, req.Mode, req.Value, req.Reason)
	if err != nil {
		if errors.Is(err, handlers.ErrSerializedNotAdjustable) {
			return Reply{Success: false, Error: handlers.ErrSerializedNotAdjustable.Error()}
		}
		return Reply{Success: false, Error: "adjustment failed: " + err.Error()}
	}

	// Publish the audit event with the same shape the local path emits, so
	// the controller's aggregator sees one event type regardless of origin.
	// Idempotent replays still publish — the event is the audit signal, and
	// downstream consumers dedupe on adjustment_id if needed.
	handlers.PublishInventoryAdjustEvent(d.app, result, events.SourceController,
		req.ControllerAdminID, req.Mode, req.Value, req.Reason)

	return Reply{Success: true, Data: inventoryAdjustReply{
		AdjustmentID: result.AdjustmentID,
		ItemID:       result.ItemID,
		ItemCode:     req.ItemCode,
		Delta:        result.Delta,
		NewQuantity:  result.NewQuantity,
		PrevQuantity: result.PrevQuantity,
	}}
}

// inventorySnapshotRequest filters to a subset of items, or returns all
// stocked items at this kiosk when item_codes is empty. The SPA's inventory
// panel uses the empty form on initial load and may pass codes for a
// targeted refresh in the future.
type inventorySnapshotRequest struct {
	ItemCodes []string `json:"item_codes,omitempty"`
}

// inventorySnapshotItem is one row in the snapshot reply. Fields are the
// minimum the SPA needs to render the table and pre-populate the adjust
// dialog (current qty for the "absolute" mode).
type inventorySnapshotItem struct {
	ItemCode         string `json:"item_code"`
	ItemName         string `json:"item_name"`
	QuantityOnHand   int    `json:"quantity_on_hand"`
	ReorderThreshold int    `json:"reorder_threshold"`
	TrackingMode     string `json:"tracking_mode"`
	Active           bool   `json:"active"`
	// Maintenance is the count of this SKU's serialized units parked in
	// maintenance (serialized only; 0 otherwise). Carried so the controller's
	// inventory panel can subtract it from available the same way the local
	// Items view does. quantity_on_hand already includes these (non-retired),
	// so available = on_hand − maintenance − out.
	Maintenance int `json:"maintenance"`
}

type inventorySnapshotReply struct {
	Items []inventorySnapshotItem `json:"items"`
}

// handleInventorySnapshot returns the kiosk's current on-hand quantities.
// Read-only: no DB writes, no events, no idempotency. Size is bounded by
// kiosk inventory (tens to low hundreds of items) — well under NATS's 1MB
// default message limit, so no pagination is needed today.
func (d *Dispatcher) handleInventorySnapshot(_ context.Context, payload []byte) Reply {
	var req inventorySnapshotRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return Reply{Success: false, Error: "invalid request body: " + err.Error()}
		}
	}

	var (
		rows []*core.Record
		err  error
	)
	if len(req.ItemCodes) == 0 {
		// All active items. Inactive items still appear in adjustments
		// history but are out of scope for the live snapshot view.
		rows, err = d.app.FindRecordsByFilter("items", "active = true", "code", 0, 0)
	} else {
		// Filter by the supplied codes. PB's filter expressions don't
		// support SQL-style IN, so we build a chain of OR'd equalities.
		// Parameterized — never inline the values into the filter string.
		params := dbx.Params{}
		clauses := make([]string, 0, len(req.ItemCodes))
		for i, code := range req.ItemCodes {
			key := fmt.Sprintf("c%d", i)
			params[key] = code
			clauses = append(clauses, "code = {:"+key+"}")
		}
		filter := joinOr(clauses)
		rows, err = d.app.FindRecordsByFilter("items", filter, "code", 0, 0, params)
	}
	if err != nil {
		return Reply{Success: false, Error: "items lookup failed: " + err.Error()}
	}

	// Maintenance counts for serialized items, gathered in one query and
	// grouped by item id. Quantity-tracked items have no instances, so they
	// stay 0. Cheap even with no maintenance rows (empty result set).
	maintByItem := map[string]int{}
	if maintRows, merr := d.app.FindRecordsByFilter("item_instances",
		"status = {:s}", "", 0, 0, dbx.Params{"s": "maintenance"}); merr == nil {
		for _, mr := range maintRows {
			maintByItem[mr.GetString("item")]++
		}
	}

	items := make([]inventorySnapshotItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, inventorySnapshotItem{
			ItemCode:         r.GetString("code"),
			ItemName:         r.GetString("name"),
			QuantityOnHand:   r.GetInt("quantity_on_hand"),
			ReorderThreshold: r.GetInt("reorder_threshold"),
			TrackingMode:     r.GetString("tracking_mode"),
			Active:           r.GetBool("active"),
			Maintenance:      maintByItem[r.Id],
		})
	}
	return Reply{Success: true, Data: inventorySnapshotReply{Items: items}}
}

// joinOr concatenates filter clauses with " || ". PB's filter language
// uses doubled-pipe for OR — we build the chain manually because the only
// alternative is pulling strings.Join into this one call site.
func joinOr(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += " || "
		}
		out += v
	}
	return out
}
