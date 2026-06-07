package controller

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
)

// checkoutCloseRequest is the SPA-supplied body for POST
// /api/controller/kiosks/{code}/checkouts/{source_line_id}/close. The
// path's source_line_id is the kiosk-side transaction_lines.id of the
// checkout that opened the row (mirrored on the controller side via the
// projected ledger's source_line_id column).
//
// command_id is generated server-side per attempt — a misbehaving SPA
// can't break idempotency by re-sending the same id.
type checkoutCloseRequest struct {
	Reason string `json:"reason"`
	Notes  string `json:"notes"`
}

// checkoutCloseCommandPayload is the JSON the controller sends to the
// kiosk over NATS. Mirrors the kiosk's commands.checkoutCloseRequest shape.
type checkoutCloseCommandPayload struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	TransactionLineID string `json:"transaction_line_id"`
	Reason            string `json:"reason"`
	Notes             string `json:"notes"`
}

// CheckoutClose returns the POST
// /api/controller/kiosks/{code}/checkouts/{source_line_id}/close handler.
// Mirrors InventoryAdjust: fast-fail on stale heartbeat → marshal → nc.Request
// with 5s timeout → decode envelope → pass kiosk's data through.
func (h *Handlers) CheckoutClose(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := strings.TrimSpace(re.Request.PathValue("code"))
		if kioskCode == "" {
			return re.BadRequestError("kiosk code is required", nil)
		}
		sourceLineID := strings.TrimSpace(re.Request.PathValue("source_line_id"))
		if sourceLineID == "" {
			return re.BadRequestError("source_line_id is required", nil)
		}

		var body checkoutCloseRequest
		if err := re.BindBody(&body); err != nil {
			return re.BadRequestError("invalid request body", err)
		}
		body.Reason = strings.TrimSpace(body.Reason)
		body.Notes = strings.TrimSpace(body.Notes)
		if body.Reason == "" {
			return re.BadRequestError("reason is required", nil)
		}

		commandID := uuid.NewString()
		payload := checkoutCloseCommandPayload{
			CommandID:         commandID,
			ControllerAdminID: re.Auth.Id,
			TransactionLineID: sourceLineID,
			Reason:            body.Reason,
			Notes:             body.Notes,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return re.InternalServerError("marshal command", err)
		}

		// Mutation: the kiosk's reply passes through to the SPA unchanged.
		// Heartbeat fast-fail, request, and envelope decode all live in
		// dispatchKioskCommand (see instances.go).
		return dispatchKioskCommand(re, nc, reg, kioskCode,
			events.CommandSubject(kioskCode, "checkout.close"), commandID, data)
	}
}
