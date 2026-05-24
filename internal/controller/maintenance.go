package controller

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
)

// Maintenance endpoints — operator tools that fix or refill state on a
// remote kiosk. Both wrap the existing kiosk-side handlers via the
// command bus, sharing the dispatchKioskCommand helper from instances.go
// for the heartbeat fast-fail + envelope decode boilerplate.

// IntegrityRebuild returns the POST /api/controller/kiosks/{code}/integrity/rebuild
// handler. Wipes + repopulates the kiosk's open_checkouts from its ledger.
// command_id is server-generated for log correlation; the operation is
// idempotent on its own (replay produces identical state).
func (h *Handlers) IntegrityRebuild(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := strings.TrimSpace(re.Request.PathValue("code"))
		if kioskCode == "" {
			return re.BadRequestError("kiosk code is required", nil)
		}
		commandID := uuid.NewString()
		data, err := json.Marshal(map[string]string{
			"command_id":          commandID,
			"controller_admin_id": re.Auth.Id,
		})
		if err != nil {
			return re.InternalServerError("marshal command", err)
		}
		return dispatchKioskCommand(re, nc, reg, kioskCode,
			events.IntegrityRebuildCommandSubject(kioskCode), commandID, data)
	}
}

// ledgerRepublishRequest is the SPA-supplied body. Both ends of the window
// are optional; an empty body re-emits the whole completed ledger.
type ledgerRepublishRequest struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

type ledgerRepublishCommandPayload struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	From              string `json:"from,omitempty"`
	To                string `json:"to,omitempty"`
}

// LedgerRepublish returns the POST /api/controller/kiosks/{code}/ledger/republish
// handler. Re-emits transaction.complete + item.{action} events for every
// completed transaction in the (optional) window. The controller's
// projection is idempotent on source_line_id, so duplicates are no-ops —
// the typical use is recovering from a NATS outage where events were
// buffered and lost.
func (h *Handlers) LedgerRepublish(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := strings.TrimSpace(re.Request.PathValue("code"))
		if kioskCode == "" {
			return re.BadRequestError("kiosk code is required", nil)
		}
		var body ledgerRepublishRequest
		// Body is optional — bind only if present.
		_ = re.BindBody(&body)
		commandID := uuid.NewString()
		data, err := json.Marshal(ledgerRepublishCommandPayload{
			CommandID:         commandID,
			ControllerAdminID: re.Auth.Id,
			From:              strings.TrimSpace(body.From),
			To:                strings.TrimSpace(body.To),
		})
		if err != nil {
			return re.InternalServerError("marshal command", err)
		}
		return dispatchKioskCommand(re, nc, reg, kioskCode,
			events.LedgerRepublishCommandSubject(kioskCode), commandID, data)
	}
}
