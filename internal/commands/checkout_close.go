package commands

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/skeeeon/kiosk/internal/commit"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// checkoutCloseRequest is the JSON the controller sends. transaction_line_id
// is the kiosk-side primary key of the checkout line that opened the row;
// the controller's projected ledger stores it as source_line_id, so it's
// the natural way for the controller (which has no view of the kiosk's
// open_checkouts.id) to identify which row to close.
//
//   - For serialized items there is exactly one open_checkouts row per
//     transaction_line — that's the one we close.
//   - For non-serialized qty=N rows there are N fungible rows sharing the
//     same transaction_line; we close the oldest one. Controllers calling
//     this N times in a row close all N (each call closes one).
type checkoutCloseRequest struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	TransactionLineID string `json:"transaction_line_id"`
	Reason            string `json:"reason"`
	Notes             string `json:"notes"`
}

// checkoutCloseReply is what the controller endpoint forwards back to the
// SPA. Field names match commit.AdminCloseResult for pass-through.
type checkoutCloseReply struct {
	TransactionID  string `json:"transaction_id"`
	LineID         string `json:"line_id"`
	OpenCheckoutID string `json:"open_checkout_id"`
	ItemID         string `json:"item_id"`
	ItemCode       string `json:"item_code"`
	UserID         string `json:"user_id"`
	UserCode       string `json:"user_code"`
	ClosureReason  string `json:"closure_reason"`
}

// handleCheckoutClose executes a controller→kiosk admin force-close. Mirrors
// handleInventoryAdjust's flow: validate, resolve naturals to local ids,
// invoke commit.AdminClose, return either an envelope error or the close
// result. Always replies within the dispatcher's deadline — even on
// validation errors — so the controller never renders a false "kiosk
// offline" banner.
func (d *Dispatcher) handleCheckoutClose(_ context.Context, payload []byte) Reply {
	var req checkoutCloseRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return Reply{Success: false, Error: "invalid request body: " + err.Error()}
	}
	if req.CommandID == "" {
		return Reply{Success: false, Error: "command_id is required"}
	}
	if req.ControllerAdminID == "" {
		return Reply{Success: false, Error: "controller_admin_id is required"}
	}
	if req.TransactionLineID == "" {
		return Reply{Success: false, Error: "transaction_line_id is required"}
	}
	if req.Reason == "" {
		return Reply{Success: false, Error: "reason is required"}
	}

	openCheckoutID, err := commit.OpenCheckoutIDForLine(d.app, req.TransactionLineID)
	if err != nil {
		// not-found surfaces as a normal error reply, not a 503-style
		// transport failure. The controller decodes it as a bad-gateway
		// in the envelope's Error field.
		return Reply{Success: false, Error: err.Error()}
	}

	result, err := commit.AdminClose(d.app, commit.AdminCloseInput{
		OpenCheckoutID: openCheckoutID,
		ActorID:        req.ControllerAdminID,
		Source:         events.SourceController,
		CommandID:      req.CommandID,
		Reason:         strings.TrimSpace(req.Reason),
		Notes:          strings.TrimSpace(req.Notes),
		Identity:       kioskctx.Get(),
	}, events.Publish)
	if err != nil {
		return Reply{Success: false, Error: "close failed: " + err.Error()}
	}

	return Reply{Success: true, Data: checkoutCloseReply{
		TransactionID:  result.TransactionID,
		LineID:         result.LineID,
		OpenCheckoutID: result.OpenCheckoutID,
		ItemID:         result.ItemID,
		ItemCode:       result.ItemCode,
		UserID:         result.UserID,
		UserCode:       result.UserCode,
		ClosureReason:  result.ClosureReason,
	}}
}
