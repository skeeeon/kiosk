package handlers

import (
	"net/http"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/commit"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// CartStart begins (or resumes) a cart for the badged-in user.
func (h *Handlers) CartStart(re *core.RequestEvent) error {
	var body struct {
		UserCode string `json:"user_code"`
	}
	if err := re.BindBody(&body); err != nil {
		return re.BadRequestError("invalid request body", err)
	}
	if body.UserCode == "" {
		return re.BadRequestError("user_code is required", nil)
	}

	user, err := h.App.FindFirstRecordByFilter("users", "code = {:code}", dbx.Params{"code": body.UserCode})
	if isNotFound(err) {
		return re.NotFoundError("user not found", nil)
	}
	if err != nil {
		return err
	}
	if !user.GetBool("active") {
		return re.BadRequestError("user is inactive", nil)
	}

	c := h.Carts.Start(user.Id, user.GetString("code"), user.GetString("name"))
	return re.JSON(http.StatusOK, map[string]any{"cart": c})
}

// CartAdd appends an item line to the cart, computing the default action
// from the item's type and current open-checkout state.
func (h *Handlers) CartAdd(re *core.RequestEvent) error {
	var body struct {
		CartID   string `json:"cart_id"`
		ItemCode string `json:"item_code"`
	}
	if err := re.BindBody(&body); err != nil {
		return re.BadRequestError("invalid request body", err)
	}
	if body.CartID == "" || body.ItemCode == "" {
		return re.BadRequestError("cart_id and item_code are required", nil)
	}

	c, err := h.Carts.Get(body.CartID)
	if err != nil {
		return re.NotFoundError("cart not found or expired", nil)
	}

	item, err := h.App.FindFirstRecordByFilter("items", "code = {:code}", dbx.Params{"code": body.ItemCode})
	if isNotFound(err) {
		// Try RFID as a courtesy — same as the scan dispatcher.
		item, err = h.App.FindFirstRecordByFilter("items", "rfid_epc = {:epc}", dbx.Params{"epc": body.ItemCode})
	}
	if isNotFound(err) {
		return re.NotFoundError("item not found", nil)
	}
	if err != nil {
		return err
	}
	if !item.GetBool("active") {
		return re.BadRequestError("item is inactive", nil)
	}

	action, origUserID, origUserName, warnings, err := h.defaultActionFor(item, c.UserID)
	if err != nil {
		return err
	}

	line := &cart.Line{
		ItemID:                   item.Id,
		ItemCode:                 item.GetString("code"),
		ItemName:                 item.GetString("name"),
		ItemType:                 item.GetString("type"),
		TrackingMode:             item.GetString("tracking_mode"),
		Action:                   action,
		Qty:                      1,
		Serial:                   item.GetString("serial"),
		OriginalCheckoutUserID:   origUserID,
		OriginalCheckoutUserName: origUserName,
		Warnings:                 warnings,
	}

	c, added, err := h.Carts.AddLine(body.CartID, line)
	if err != nil {
		return re.NotFoundError("cart not found or expired", nil)
	}
	return re.JSON(http.StatusOK, map[string]any{"cart": c, "line": added})
}

// CartUpdateLine sets qty and/or action on a cart line.
func (h *Handlers) CartUpdateLine(re *core.RequestEvent) error {
	lineID := re.Request.PathValue("id")
	if lineID == "" {
		return re.BadRequestError("line id is required", nil)
	}
	var body struct {
		Qty    *int    `json:"qty,omitempty"`
		Action *string `json:"action,omitempty"`
	}
	if err := re.BindBody(&body); err != nil {
		return re.BadRequestError("invalid request body", err)
	}
	if body.Qty == nil && body.Action == nil {
		return re.BadRequestError("at least one of qty, action is required", nil)
	}
	if body.Qty != nil && *body.Qty < 1 {
		return re.BadRequestError("qty must be >= 1", nil)
	}
	if body.Action != nil {
		switch *body.Action {
		case "checkout", "return", "consume":
		default:
			return re.BadRequestError("action must be one of checkout, return, consume", nil)
		}
	}

	c, line, err := h.Carts.UpdateLine(lineID, body.Qty, body.Action)
	if err != nil {
		return re.NotFoundError("line not found or cart expired", nil)
	}
	return re.JSON(http.StatusOK, map[string]any{"cart": c, "line": line})
}

// CartDeleteLine removes a line from its cart.
func (h *Handlers) CartDeleteLine(re *core.RequestEvent) error {
	lineID := re.Request.PathValue("id")
	if lineID == "" {
		return re.BadRequestError("line id is required", nil)
	}
	c, err := h.Carts.DeleteLine(lineID)
	if err != nil {
		return re.NotFoundError("line not found or cart expired", nil)
	}
	return re.JSON(http.StatusOK, map[string]any{"cart": c})
}

// CartCommit promotes the cart to a persisted transaction. All DB writes
// happen atomically in commit.Commit; on success the cart is dropped from
// the in-memory store and the result is returned.
func (h *Handlers) CartCommit(re *core.RequestEvent) error {
	var body struct {
		CartID string `json:"cart_id"`
	}
	if err := re.BindBody(&body); err != nil {
		return re.BadRequestError("invalid request body", err)
	}
	if body.CartID == "" {
		return re.BadRequestError("cart_id is required", nil)
	}

	c, err := h.Carts.Get(body.CartID)
	if err != nil {
		return re.NotFoundError("cart not found or expired", nil)
	}

	result, err := commit.Commit(h.App, c, kioskctx.Get(), events.Publish)
	if err != nil {
		return re.InternalServerError("commit failed", err)
	}

	_ = h.Carts.Delete(body.CartID)
	return re.JSON(http.StatusOK, result)
}

// CartCancel discards an in-progress cart without committing.
func (h *Handlers) CartCancel(re *core.RequestEvent) error {
	var body struct {
		CartID string `json:"cart_id"`
	}
	if err := re.BindBody(&body); err != nil {
		return re.BadRequestError("invalid request body", err)
	}
	if body.CartID == "" {
		return re.BadRequestError("cart_id is required", nil)
	}
	// Treat already-gone as success — idempotent cancel.
	_ = h.Carts.Delete(body.CartID)
	return re.JSON(http.StatusOK, map[string]any{"ok": true})
}

// defaultActionFor implements the action-defaulting rules from §6 of the plan:
//
//   - consumable → consume
//   - tool checked out to the cart's user → return
//   - tool checked out to another user → return + cross_user_return warning
//   - tool not checked out → checkout
//
// Cross-user and uncorrelated policy toggles in config aren't enforced here —
// the cart freely accepts any action; the commit hook (Phase 4) enforces
// the kiosk's return policy when finalizing the transaction.
func (h *Handlers) defaultActionFor(item *core.Record, cartUserID string) (
	action, origUserID, origUserName string, warnings []string, err error,
) {
	if item.GetString("type") == "consumable" {
		return "consume", "", "", nil, nil
	}

	// Tool: is it already checked out to this user?
	self, err := h.App.FindFirstRecordByFilter(
		"open_checkouts",
		"item = {:item} && user = {:user}",
		dbx.Params{"item": item.Id, "user": cartUserID},
	)
	if err != nil && !isNotFound(err) {
		return "", "", "", nil, err
	}
	if self != nil {
		return "return", "", "", nil, nil
	}

	// To someone else?
	other, err := h.App.FindFirstRecordByFilter(
		"open_checkouts",
		"item = {:item}",
		dbx.Params{"item": item.Id},
	)
	if err != nil && !isNotFound(err) {
		return "", "", "", nil, err
	}
	if other != nil {
		holderID := other.GetString("user")
		var holderName string
		if u, e := h.App.FindRecordById("users", holderID); e == nil && u != nil {
			holderName = u.GetString("name")
		}
		return "return", holderID, holderName,
			[]string{"cross_user_return:" + holderName}, nil
	}

	return "checkout", "", "", nil, nil
}
