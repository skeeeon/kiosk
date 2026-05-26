package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/commit"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/scan"
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

	c := h.Carts.Start(user.Id, user.GetString("code"), user.GetString("name"), user.GetString("role"))
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

	c, added, err := h.addCodeToCart(body.CartID, body.ItemCode)
	if err != nil {
		return cartAddErrorToResponse(re, err)
	}
	return re.JSON(http.StatusOK, map[string]any{"cart": c, "line": added})
}

// addCodeToCart is the shared cart-write path used by both CartAdd
// (one HTTP scan event) and RFIDScan (a batched LLRP read). It
// resolves the code through the same item-instance → item → rfid_epc
// precedence as the scan dispatcher, validates active flags, applies
// the default action via defaultActionFor, and appends via the cart
// store. Returns the updated cart and the line that landed; callers
// translate errors per their context (HTTP responses vs per-EPC
// skip-and-log).
func (h *Handlers) addCodeToCart(cartID, code string) (*cart.Cart, *cart.Line, error) {
	c, err := h.Carts.Get(cartID)
	if err != nil {
		return nil, nil, errCartNotFound
	}

	item, instance, err := h.resolveScannableForCart(code)
	if err != nil {
		if isNotFound(err) {
			return nil, nil, errCodeNotFound
		}
		return nil, nil, err
	}
	if !item.GetBool("active") {
		return nil, nil, errItemInactive
	}
	if item.GetString("tracking_mode") == "serialized" && instance == nil {
		return nil, nil, errSerializedNeedsInstance
	}
	if instance != nil && !instance.GetBool("active") {
		return nil, nil, errInstanceInactive
	}

	action, err := h.defaultActionFor(item, instance, c.UserID)
	if err != nil {
		return nil, nil, err
	}

	lineCode := item.GetString("code")
	var lineSerial, instanceID, instanceCode string
	if instance != nil {
		lineCode = instance.GetString("code")
		lineSerial = instance.GetString("serial")
		instanceID = instance.Id
		instanceCode = instance.GetString("code")
	}

	line := &cart.Line{
		ItemID:           item.Id,
		ItemCode:         lineCode,
		ItemName:         item.GetString("name"),
		ItemType:         item.GetString("type"),
		TrackingMode:     item.GetString("tracking_mode"),
		Action:           action,
		Qty:              1,
		Serial:           lineSerial,
		ItemInstanceID:   instanceID,
		ItemInstanceCode: instanceCode,
	}

	c, added, err := h.Carts.AddLine(cartID, line)
	if err != nil {
		return nil, nil, err
	}
	// Low-stock warning is informational and shouldn't fail the add.
	if w, werr := lowStockWarning(h.App, item, added.Action, added.Qty); werr == nil && w != "" {
		added.Warnings = setLowStockWarning(added.Warnings, w)
	}
	return c, added, nil
}

// Sentinel errors returned by addCodeToCart. RFIDScan branches on
// these to decide skip vs surface; CartAdd's response builder
// translates them to HTTP responses. Distinct from cart.Err* values
// (those bubble through unchanged) — these cover the resolve /
// activity-flag layer above the cart store.
var (
	errCartNotFound            = errors.New("cart not found or expired")
	errCodeNotFound            = errors.New("code not found")
	errItemInactive            = errors.New("item is inactive")
	errInstanceInactive        = errors.New("instance is inactive")
	errSerializedNeedsInstance = errors.New("serialized item needs a specific instance")
)

// cartAddErrorToResponse maps the sentinel + cart.Err* errors
// addCodeToCart can return into RequestEvent responses. Kept in one
// place so CartAdd, the existing rescan path, and the new RFIDScan
// per-EPC fallback (when surfacing rather than skipping) all phrase
// the user-visible message identically.
func cartAddErrorToResponse(re *core.RequestEvent, err error) error {
	switch {
	case errors.Is(err, errCartNotFound):
		return re.NotFoundError("cart not found or expired", nil)
	case errors.Is(err, errCodeNotFound):
		return re.NotFoundError("item not found", nil)
	case errors.Is(err, errItemInactive):
		return re.BadRequestError("item is inactive", nil)
	case errors.Is(err, errInstanceInactive):
		return re.BadRequestError("instance is inactive", nil)
	case errors.Is(err, errSerializedNeedsInstance):
		return re.BadRequestError("select a specific unit (instance) for this serialized item", nil)
	case errors.Is(err, cart.ErrQtyOutOfRange):
		return re.BadRequestError(fmt.Sprintf("qty per line is capped at %d", cart.MaxQty), nil)
	case errors.Is(err, cart.ErrInvalidAction):
		return re.BadRequestError("action is not valid for this item type", nil)
	case errors.Is(err, cart.ErrDuplicateInstance):
		return re.BadRequestError("this unit is already in your cart", nil)
	}
	return err
}

// ForemanReturnWorker is one entry in the picker the SPA renders for the
// "Return on behalf of…" dialog. Only workers with at least one open
// checkout are included — empty rows are dead UI.
type ForemanReturnWorker struct {
	UserID        string                    `json:"user_id"`
	UserCode      string                    `json:"user_code"`
	UserName      string                    `json:"user_name"`
	OpenCheckouts []scan.OpenCheckoutDetail `json:"open_checkouts"`
}

// CartForemanReturnOptions powers the picker for the
// "Return on behalf of…" dialog: the list of workers in the foreman's
// group who have at least one open checkout, each hydrated with their
// outstanding items. The SPA renders this directly — no follow-up
// per-worker fetch.
//
// Pre-flight is the same as CartForemanReturn (cart user is a foreman with
// a non-empty group). The cart user themselves is excluded from the list;
// a foreman returning their own item uses the normal scan path.
func (h *Handlers) CartForemanReturnOptions(re *core.RequestEvent) error {
	cartID := re.Request.URL.Query().Get("cart_id")
	if cartID == "" {
		return re.BadRequestError("cart_id is required", nil)
	}

	c, err := h.Carts.Get(cartID)
	if err != nil {
		return re.NotFoundError("cart not found or expired", nil)
	}

	cartUser, err := h.App.FindRecordById("users", c.UserID)
	if err != nil {
		return re.NotFoundError("cart user not found", nil)
	}
	if cartUser.GetString("role") != "foreman" {
		return re.ForbiddenError("only a foreman can access return-on-behalf options", nil)
	}
	groupID := cartUser.GetString("group")
	if groupID == "" {
		return re.ForbiddenError("foreman has no group set", nil)
	}

	group, err := h.App.FindRecordById("groups", groupID)
	if err != nil {
		return re.InternalServerError("find group", err)
	}

	// Inactive workers are deliberately *included*: the dialog's whole
	// purpose is closing out absent workers' items, and "absent" often
	// correlates with "inactive."
	members, err := h.App.FindRecordsByFilter("users",
		"group = {:g} && id != {:self}",
		"name", 0, 0,
		dbx.Params{"g": groupID, "self": c.UserID})
	if err != nil {
		return re.InternalServerError("find group members", err)
	}

	workers := make([]ForemanReturnWorker, 0, len(members))
	for _, m := range members {
		details := h.openCheckoutsForUser(m.Id)
		if len(details) == 0 {
			continue
		}
		workers = append(workers, ForemanReturnWorker{
			UserID:        m.Id,
			UserCode:      m.GetString("code"),
			UserName:      m.GetString("name"),
			OpenCheckouts: details,
		})
	}

	return re.JSON(http.StatusOK, map[string]any{
		"group_code": group.GetString("code"),
		"workers":    workers,
	})
}

// CartForemanReturn adds a return line on behalf of another worker. This is
// the *only* path that populates Line.OriginalCheckoutUserID — keeping the
// trust invariant documented in CLAUDE.md tight: the cart never reads that
// id from the client, only from a server-side resolve of target_user_code.
//
// Pre-flight enforces the same rules commit.go enforces at transaction time
// (cart user is a foreman, has a group, and the target shares that group)
// so a confused foreman gets an immediate error instead of a cryptic
// failure five scans later. Commit remains the trust boundary; this is just
// good UX.
func (h *Handlers) CartForemanReturn(re *core.RequestEvent) error {
	var body struct {
		CartID         string `json:"cart_id"`
		ItemCode       string `json:"item_code"`
		TargetUserCode string `json:"target_user_code"`
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

	cartUser, err := h.App.FindRecordById("users", c.UserID)
	if err != nil {
		return re.NotFoundError("cart user not found", nil)
	}
	if cartUser.GetString("role") != "foreman" {
		return re.ForbiddenError("only a foreman can return items on behalf of another worker", nil)
	}
	cartUserGroup := cartUser.GetString("group")
	if cartUserGroup == "" {
		return re.ForbiddenError("foreman has no group set; cross-user returns require a group", nil)
	}

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
	if item.GetString("type") == "consumable" {
		return re.BadRequestError("consumables cannot be returned", nil)
	}
	if item.GetString("tracking_mode") == "serialized" && instance == nil {
		return re.BadRequestError("select a specific unit (instance) for this serialized item", nil)
	}

	// Two input shapes:
	//   - target_user_code provided → caller picked from the workers list
	//     (or supplied a known code). Validate as before.
	//   - target_user_code omitted  → "I have the physical tool in hand"
	//     shortcut. Only valid for serialized items: the instance's open
	//     checkout uniquely identifies the holder, so we can derive
	//     target_user_code server-side from the open_checkouts row.
	//
	// Either way the same-group check applies, and the open_checkouts row
	// must exist (no uncorrelated returns from this endpoint).
	var target *core.Record
	if body.TargetUserCode != "" {
		target, err = h.App.FindFirstRecordByFilter("users", "code = {:code}", dbx.Params{"code": body.TargetUserCode})
		if isNotFound(err) {
			return re.NotFoundError("target worker not found", nil)
		}
		if err != nil {
			return err
		}
	} else {
		if instance == nil {
			return re.BadRequestError("target_user_code is required when the item is not serialized", nil)
		}
		open, err := h.App.FindFirstRecordByFilter("open_checkouts",
			"item_instance = {:inst}", dbx.Params{"inst": instance.Id})
		if isNotFound(err) {
			return re.NotFoundError(fmt.Sprintf("%s is not currently checked out", instance.GetString("code")), nil)
		}
		if err != nil {
			return err
		}
		target, err = h.App.FindRecordById("users", open.GetString("user"))
		if err != nil {
			return re.InternalServerError("resolve holder", err)
		}
	}

	if target.Id == c.UserID {
		return re.BadRequestError("this unit is checked out to you; scan it normally to return your own", nil)
	}
	if target.GetString("group") != cartUserGroup {
		return re.ForbiddenError(fmt.Sprintf("%s is held by %s in a different group; an admin must handle cross-group returns", item.GetString("name"), target.GetString("name")), nil)
	}

	// Confirm the target actually has this item/instance out. For the
	// derive-from-instance path above this is a redundant check on the
	// instance's row, but it also covers the worker-pick path's case where
	// the row may have been returned between the picker fetch and submit.
	openFilter := "item = {:item} && user = {:user}"
	openParams := dbx.Params{"item": item.Id, "user": target.Id}
	if instance != nil {
		openFilter = "item_instance = {:inst} && user = {:user}"
		openParams = dbx.Params{"inst": instance.Id, "user": target.Id}
	}
	if _, err := h.App.FindFirstRecordByFilter("open_checkouts", openFilter, openParams); err != nil {
		if isNotFound(err) {
			return re.NotFoundError(fmt.Sprintf("%s does not have %s out", target.GetString("name"), item.GetString("name")), nil)
		}
		return err
	}

	lineCode := item.GetString("code")
	var lineSerial, instanceID, instanceCode string
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
		Action:                   "return",
		Qty:                      1,
		Serial:                   lineSerial,
		ItemInstanceID:           instanceID,
		ItemInstanceCode:         instanceCode,
		OriginalCheckoutUserID:   target.Id,
		OriginalCheckoutUserName: target.GetString("name"),
	}

	c, added, err := h.Carts.AddLine(body.CartID, line)
	if err != nil {
		switch {
		case errors.Is(err, cart.ErrQtyOutOfRange):
			return re.BadRequestError(fmt.Sprintf("qty per line is capped at %d", cart.MaxQty), nil)
		case errors.Is(err, cart.ErrInvalidAction):
			return re.BadRequestError("action is not valid for this item type", nil)
		case errors.Is(err, cart.ErrDuplicateInstance):
			return re.BadRequestError("this unit is already in your cart", nil)
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

	id := kioskctx.Get()
	result, err := commit.Commit(h.App, c, id, commit.Policy{
		AllowCrossUser:    h.Cfg.Returns.CrossUserAllowed(),
		AllowUncorrelated: h.Cfg.Returns.UncorrelatedAllowed(),
	}, events.Publish)
	if err != nil {
		return re.InternalServerError("commit failed", err)
	}

	// Receipt + low-stock dispatch. Two flows, mutually exclusive:
	//
	//   - Managed mode (controller.enabled=true): publish the
	//     ReceiptContext over NATS on the receipt.transaction subject and
	//     let the controller render + send via its centralized SMTP. The
	//     local Notifier.Send is suppressed because the controller now owns
	//     this flow; the kiosk's notification_send_log stays empty in
	//     managed mode and all audit lives at the controller.
	//   - Standalone mode: same behavior as v1 — render locally via the
	//     kiosk's Notifier against its own template rows.
	//
	// BuildReceiptContext does one extra users-table read for the email
	// address; failures there log and drop the receipt without affecting
	// the commit response.
	if rc, berr := notifications.BuildReceiptContext(h.App, c, id, result, time.Now().UTC()); berr == nil {
		if h.Cfg.Controller.Enabled {
			events.Publish(events.ReceiptTransactionSubject(id.KioskCode), rc)
		} else if h.Notifier != nil {
			h.Notifier.Send(notifications.EventTypeReceiptTransaction, rc)
		}
	}
	h.fireLowStockAlerts(c, id)

	_ = h.Carts.Delete(body.CartID)
	return re.JSON(http.StatusOK, result)
}

// fireLowStockAlerts inspects the just-committed cart for consume lines
// whose item crossed its reorder threshold and dispatches one alert per
// item. Managed mode publishes the LowStockContext to the alert.lowstock
// NATS subject for the controller to render + send; standalone mode uses
// the local Notifier's dedupe-gated SendIfFirst path. Quietly skips on
// lookup or math errors — alerts must never affect the commit response.
func (h *Handlers) fireLowStockAlerts(c *cart.Cart, id kioskctx.Identity) {
	consumeQty := map[string]int{}
	for _, l := range c.Lines {
		if l.Action != "consume" {
			continue
		}
		consumeQty[l.ItemID] += l.Qty
	}
	for itemID, qty := range consumeQty {
		item, err := h.App.FindRecordById("items", itemID)
		if err != nil {
			continue
		}
		threshold := item.GetInt("reorder_threshold")
		if threshold <= 0 {
			continue
		}
		available, err := availableForItem(h.App, item)
		if err != nil {
			continue
		}
		if !crossedLowStock(available+qty, available, threshold) {
			continue
		}
		ctx := notifications.LowStockContext{
			Kiosk: notifications.KioskInfo{
				Code:         id.KioskCode,
				LocationCode: id.LocationCode,
			},
			Item: notifications.ItemInfo{
				ID:       item.Id,
				Code:     item.GetString("code"),
				Name:     item.GetString("name"),
				Category: item.GetString("category"),
				Unit:     item.GetString("unit"),
			},
			PrevQty:   available + qty,
			NewQty:    available,
			Threshold: threshold,
			Available: available,
			Trigger:   "consume",
		}
		if h.Cfg.Controller.Enabled {
			events.Publish(events.LowStockAlertSubject(id.KioskCode), ctx)
		} else if h.Notifier != nil {
			h.Notifier.SendIfFirst(notifications.EventTypeLowStock, item.Id, ctx)
		}
	}
}

// crossedLowStock is the pure threshold-crossing predicate split out for
// unit tests. Fires only when "before" was strictly above threshold and
// "after" is at-or-below — repeated consumes that all stay below threshold
// don't keep alerting (the daily dedupe row protects this too, but the
// edge guard keeps the alert semantically meaningful in tests).
func crossedLowStock(prev, current, threshold int) bool {
	return threshold > 0 && prev > threshold && current <= threshold
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
// code → item code → instance RFID. Returns (item, instance|nil, err). For
// non-instance matches, instance is nil and the item is the matched SKU.
// For instance matches, the instance is loaded and the item is its parent
// SKU. RFID lookups are instance-only — EPCs live on item_instances.
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
//   - tool not checked out (by this user) → checkout
//
// When an instance is supplied (serialized scan), open-checkout lookups are
// scoped to that exact instance — Bob's drill SN-B doesn't count against
// Alice scanning drill SN-A.
//
// Returning a tool another worker has out is *not* an implicit default — for
// quantity-tracked tools the natural read of "Bob has one out, I scan the
// SKU" is "give me one too," and even for serialized the action of taking
// over someone else's open checkout deserves explicit intent. That path
// lives in CartForemanReturn, which is the sole writer of
// OriginalCheckoutUserID on cart lines (preserving the trust invariant
// documented in CLAUDE.md).
func (h *Handlers) defaultActionFor(item, instance *core.Record, cartUserID string) (string, error) {
	if item.GetString("type") == "consumable" {
		return "consume", nil
	}

	filter := "item = {:item} && user = {:user}"
	params := dbx.Params{"item": item.Id, "user": cartUserID}
	if instance != nil {
		filter = "item_instance = {:inst} && user = {:user}"
		params = dbx.Params{"inst": instance.Id, "user": cartUserID}
	}

	self, err := h.App.FindFirstRecordByFilter("open_checkouts", filter, params)
	if err != nil && !isNotFound(err) {
		return "", err
	}
	if self != nil {
		return "return", nil
	}
	return "checkout", nil
}
