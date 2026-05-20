package handlers

import (
	"errors"
	"fmt"
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

	// Resolve the input string against instances first, then items —
	// same precedence as the scan dispatcher.
	item, instance, err := h.resolveScannableForCart(body.ItemCode)
	if err != nil {
		if isNotFound(err) {
			return re.NotFoundError("item not found", nil)
		}
		return err
	}
	if !item.GetBool("active") {
		return re.BadRequestError("item is inactive", nil)
	}
	// Serialized items must be added via a specific instance — the SKU
	// alone doesn't identify a physical unit. Browse-and-pick by SKU is
	// the path that hits this; the frontend prompts to select an instance.
	if item.GetString("tracking_mode") == "serialized" && instance == nil {
		return re.BadRequestError("select a specific unit (instance) for this serialized item", nil)
	}
	if instance != nil && !instance.GetBool("active") {
		return re.BadRequestError("instance is inactive", nil)
	}

	action, origUserID, origUserName, warnings, err := h.defaultActionFor(item, instance, c.UserID)
	if err != nil {
		return err
	}

	lineCode := item.GetString("code")
	lineSerial := item.GetString("serial")
	var instanceID, instanceCode string
	if instance != nil {
		lineCode = instance.GetString("code")
		lineSerial = instance.GetString("serial")
		instanceID = instance.Id
		instanceCode = instance.GetString("code")
	}

	line := &cart.Line{
		ItemID:                   item.Id,
		ItemCode:                 lineCode,
		ItemName:                 item.GetString("name"),
		ItemType:                 item.GetString("type"),
		TrackingMode:             item.GetString("tracking_mode"),
		Action:                   action,
		Qty:                      1,
		Serial:                   lineSerial,
		ItemInstanceID:           instanceID,
		ItemInstanceCode:         instanceCode,
		OriginalCheckoutUserID:   origUserID,
		OriginalCheckoutUserName: origUserName,
		Warnings:                 warnings,
	}

	c, added, err := h.Carts.AddLine(body.CartID, line)
	if err == nil {
		// Recompute low-stock against the stacked total qty (AddLine may have
		// merged into an existing line). Warning is informational only —
		// commit doesn't reject on it.
		if w, werr := lowStockWarning(h.App, item, added.Action, added.Qty); werr == nil && w != "" {
			added.Warnings = setLowStockWarning(added.Warnings, w)
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, cart.ErrQtyOutOfRange):
			return re.BadRequestError(fmt.Sprintf("qty per line is capped at %d", cart.MaxQty), nil)
		case errors.Is(err, cart.ErrInvalidAction):
			return re.BadRequestError("action is not valid for this item type", nil)
		}
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
	if body.Qty != nil && (*body.Qty < 1 || *body.Qty > cart.MaxQty) {
		return re.BadRequestError(fmt.Sprintf("qty must be between 1 and %d", cart.MaxQty), nil)
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
		switch {
		case errors.Is(err, cart.ErrQtyOutOfRange):
			return re.BadRequestError(fmt.Sprintf("qty must be between 1 and %d", cart.MaxQty), nil)
		case errors.Is(err, cart.ErrInvalidAction):
			return re.BadRequestError("action is not valid for this item type", nil)
		}
		return re.NotFoundError("line not found or cart expired", nil)
	}

	// Recompute low-stock against the line's new qty/action. Failures here
	// (item lookup, count query) don't fail the update — they just leave the
	// chip absent.
	if item, ierr := h.App.FindRecordById("items", line.ItemID); ierr == nil {
		if w, werr := lowStockWarning(h.App, item, line.Action, line.Qty); werr == nil {
			line.Warnings = setLowStockWarning(line.Warnings, w)
		}
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

	result, err := commit.Commit(h.App, c, kioskctx.Get(), commit.Policy{
		AllowCrossUser:    h.Cfg.Returns.CrossUserAllowed(),
		AllowUncorrelated: h.Cfg.Returns.UncorrelatedAllowed(),
	}, events.Publish)
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

// resolveScannableForCart mirrors the scan resolver's precedence: instance
// code → item code → instance RFID → item RFID. Returns (item, instance|nil,
// err). For non-instance matches, instance is nil and the item is the
// matched SKU. For instance matches, the instance is loaded and the item
// is its parent SKU.
func (h *Handlers) resolveScannableForCart(code string) (*core.Record, *core.Record, error) {
	if code == "" {
		return nil, nil, &notFoundErr{}
	}
	if inst, err := h.App.FindFirstRecordByFilter("item_instances", "code = {:code}", dbx.Params{"code": code}); err == nil && inst != nil {
		item, ierr := h.App.FindRecordById("items", inst.GetString("item"))
		if ierr != nil {
			return nil, nil, ierr
		}
		return item, inst, nil
	}
	if item, err := h.App.FindFirstRecordByFilter("items", "code = {:code}", dbx.Params{"code": code}); err == nil && item != nil {
		return item, nil, nil
	}
	if inst, err := h.App.FindFirstRecordByFilter("item_instances", "rfid_epc = {:epc}", dbx.Params{"epc": code}); err == nil && inst != nil {
		item, ierr := h.App.FindRecordById("items", inst.GetString("item"))
		if ierr != nil {
			return nil, nil, ierr
		}
		return item, inst, nil
	}
	if item, err := h.App.FindFirstRecordByFilter("items", "rfid_epc = {:epc}", dbx.Params{"epc": code}); err == nil && item != nil {
		return item, nil, nil
	}
	return nil, nil, &notFoundErr{}
}

// notFoundErr is a tiny sentinel so callers can use isNotFound() uniformly.
type notFoundErr struct{}

func (notFoundErr) Error() string { return "not found" }
func (notFoundErr) Is(target error) bool {
	_, ok := target.(*notFoundErr)
	return ok
}

// defaultActionFor implements the action-defaulting rules:
//
//   - consumable → consume
//   - tool checked out to the cart's user → return
//   - tool checked out to another user → return + cross_user_return warning
//   - tool not checked out → checkout
//
// When an instance is supplied (serialized scan), open-checkout lookups are
// scoped to that exact instance — Bob's drill SN-B doesn't count against
// Alice scanning drill SN-A.
//
// Cross-user and uncorrelated policy toggles in config aren't enforced here —
// the cart freely accepts any action; the commit hook enforces them.
func (h *Handlers) defaultActionFor(item, instance *core.Record, cartUserID string) (
	action, origUserID, origUserName string, warnings []string, err error,
) {
	if item.GetString("type") == "consumable" {
		return "consume", "", "", nil, nil
	}

	filter := "item = {:item} && user = {:user}"
	params := dbx.Params{"item": item.Id, "user": cartUserID}
	if instance != nil {
		filter = "item_instance = {:inst} && user = {:user}"
		params = dbx.Params{"inst": instance.Id, "user": cartUserID}
	}

	self, err := h.App.FindFirstRecordByFilter("open_checkouts", filter, params)
	if err != nil && !isNotFound(err) {
		return "", "", "", nil, err
	}
	if self != nil {
		return "return", "", "", nil, nil
	}

	// To someone else?
	otherFilter := "item = {:item}"
	otherParams := dbx.Params{"item": item.Id}
	if instance != nil {
		otherFilter = "item_instance = {:inst}"
		otherParams = dbx.Params{"inst": instance.Id}
	}
	other, err := h.App.FindFirstRecordByFilter("open_checkouts", otherFilter, otherParams)
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
