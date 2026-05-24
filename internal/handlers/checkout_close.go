package handlers

import (
	"net/http"
	"strings"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/commit"
	"github.com/skeeeon/kiosk/internal/dberr"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// AdminCloseCheckout closes a single open_checkouts row administratively
// (lost / returned offline / damaged / other). The invariant "ledger writes
// only happen via the commit package" stays intact: this handler resolves
// the line id to an open_checkouts row and forwards to commit.AdminClose,
// which owns the txn + event semantics.
//
//	POST /api/kiosk/checkouts/by-line/{transaction_line_id}/close
//	body: { reason: "lost"|"returned_offline"|"damaged"|"other", notes?: string }
//
// The URL keys on transaction_line_id (not open_checkouts.id) so the kiosk
// and controller SPAs share one shape: the Aging report's DTO surfaces line
// ids, and on the controller side open_checkouts.id isn't projected at all.
//
// The trust invariant on original_checkout_user is preserved: only the line
// id is taken from the client; the affected worker is looked up server-side
// from the open_checkouts row inside commit.AdminClose.
func (h *Handlers) AdminCloseCheckout(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	lineID := re.Request.PathValue("transaction_line_id")
	if lineID == "" {
		return re.BadRequestError("transaction_line_id is required", nil)
	}

	var body struct {
		Reason string `json:"reason"`
		Notes  string `json:"notes"`
	}
	if err := re.BindBody(&body); err != nil {
		return re.BadRequestError("invalid request body", err)
	}
	body.Reason = strings.TrimSpace(body.Reason)
	body.Notes = strings.TrimSpace(body.Notes)
	if body.Reason == "" {
		return re.BadRequestError("reason is required", nil)
	}

	openCheckoutID, err := commit.OpenCheckoutIDForLine(h.App, lineID)
	if err != nil {
		return re.NotFoundError(err.Error(), nil)
	}

	result, err := commit.AdminClose(h.App, commit.AdminCloseInput{
		OpenCheckoutID: openCheckoutID,
		ActorID:        re.Auth.Id,
		Source:         commit.SourceLocal,
		Reason:         body.Reason,
		Notes:          body.Notes,
		Identity:       kioskctx.Get(),
	}, events.Publish)
	if err != nil {
		if dberr.IsNotFound(err) {
			return re.NotFoundError("open_checkout not found", nil)
		}
		if isValidationLikeError(err) {
			return re.BadRequestError(err.Error(), err)
		}
		return re.InternalServerError("admin close failed", err)
	}
	return re.JSON(http.StatusOK, result)
}

// isValidationLikeError keeps the HTTP status hygiene close to other kiosk
// handlers: the commit.AdminClose function returns plain errors for invalid
// input (bad reason, missing identity); those should surface as 400 instead
// of 500. The pattern is a string prefix check to avoid changing
// commit.AdminClose's API surface for one HTTP handler — if we ever build
// a third caller, promote these to typed sentinels.
func isValidationLikeError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, prefix := range []string{
		"invalid source",
		"invalid closure_reason",
		"open_checkout_id is required",
		"actor id is required",
		"command_id is required",
		"kiosk identity is not set",
	} {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}
