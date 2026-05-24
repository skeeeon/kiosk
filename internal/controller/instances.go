package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
)

// Instance management endpoints. All five mirror the inventory.adjust
// pattern from inventory.go: heartbeat fast-fail → marshal payload →
// nc.Request(subject, ..., 5s) → decode kioskCommandEnvelope → pass
// `data` through to the SPA as raw JSON. Idempotency uses a
// server-generated command_id for mutations; the snapshot read needs
// none. Editing is cosmetic-only and writes no audit row, but still
// goes through a command for consistency.

// dispatchKioskCommand is the shared shape for "send a command to a
// kiosk, decode the envelope, route the outcome." Extracted because the
// inventory + instance + (upcoming) maintenance handlers all repeat the
// same closure of "online check + Request + envelope decode."
//
// commandID may be empty (read-only commands); the offline response
// echoes it back for log correlation when set. Returns nil after writing
// the response — caller's handler returns it through.
func dispatchKioskCommand(
	re *core.RequestEvent,
	nc *nats.Conn,
	reg *HeartbeatRegistry,
	kioskCode, subject, commandID string,
	payload []byte,
) error {
	if !reg.IsLikelyOnline(kioskCode, heartbeatFreshness) {
		if time.Since(reg.StartedAt()) > heartbeatFreshness {
			return re.JSON(http.StatusServiceUnavailable, kioskOfflineResponse{
				Error:     "kiosk_offline",
				KioskCode: kioskCode,
				CommandID: commandID,
			})
		}
	}
	msg, err := nc.Request(subject, payload, commandTimeout)
	if err != nil {
		if errors.Is(err, nats.ErrTimeout) || errors.Is(err, nats.ErrNoResponders) {
			return re.JSON(http.StatusServiceUnavailable, kioskOfflineResponse{
				Error:     "kiosk_offline",
				KioskCode: kioskCode,
				CommandID: commandID,
			})
		}
		return re.InternalServerError("nats request failed", err)
	}
	var env kioskCommandEnvelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return re.InternalServerError("decode kiosk reply", err)
	}
	if !env.Success {
		return re.JSON(http.StatusBadGateway, map[string]any{
			"error":      "kiosk_error",
			"detail":     env.Error,
			"kiosk_code": kioskCode,
			"command_id": commandID,
		})
	}
	return re.JSON(http.StatusOK, json.RawMessage(env.Data))
}

// --- Create ---

type instanceCreateRequest struct {
	ItemCode string `json:"item_code"`
	Code     string `json:"code"`
	Serial   string `json:"serial,omitempty"`
	RFIDEPC  string `json:"rfid_epc,omitempty"`
	Notes    string `json:"notes,omitempty"`
	Active   *bool  `json:"active,omitempty"`
}

type instanceCreateCommandPayload struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	ItemCode          string `json:"item_code"`
	Code              string `json:"code"`
	Serial            string `json:"serial,omitempty"`
	RFIDEPC           string `json:"rfid_epc,omitempty"`
	Notes             string `json:"notes,omitempty"`
	Active            *bool  `json:"active,omitempty"`
}

// InstanceCreate returns the POST /api/controller/kiosks/{code}/instances
// handler. The controller server-generates the command_id; the SPA never
// supplies one.
func (h *Handlers) InstanceCreate(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := strings.TrimSpace(re.Request.PathValue("code"))
		if kioskCode == "" {
			return re.BadRequestError("kiosk code is required", nil)
		}
		var body instanceCreateRequest
		if err := re.BindBody(&body); err != nil {
			return re.BadRequestError("invalid request body", err)
		}
		body.ItemCode = strings.TrimSpace(body.ItemCode)
		body.Code = strings.TrimSpace(body.Code)
		if body.ItemCode == "" {
			return re.BadRequestError("item_code is required", nil)
		}
		if body.Code == "" {
			return re.BadRequestError("code is required", nil)
		}

		commandID := uuid.NewString()
		data, err := json.Marshal(instanceCreateCommandPayload{
			CommandID:         commandID,
			ControllerAdminID: re.Auth.Id,
			ItemCode:          body.ItemCode,
			Code:              body.Code,
			Serial:            body.Serial,
			RFIDEPC:           body.RFIDEPC,
			Notes:             body.Notes,
			Active:            body.Active,
		})
		if err != nil {
			return re.InternalServerError("marshal command", err)
		}
		return dispatchKioskCommand(re, nc, reg, kioskCode,
			events.InstanceCreateCommandSubject(kioskCode), commandID, data)
	}
}

// --- Edit (cosmetic) ---

type instanceEditRequest struct {
	Code    *string `json:"code,omitempty"`
	Serial  *string `json:"serial,omitempty"`
	RFIDEPC *string `json:"rfid_epc,omitempty"`
	Notes   *string `json:"notes,omitempty"`
}

type instanceEditCommandPayload struct {
	InstanceCode string  `json:"instance_code"`
	Code         *string `json:"code,omitempty"`
	Serial       *string `json:"serial,omitempty"`
	RFIDEPC      *string `json:"rfid_epc,omitempty"`
	Notes        *string `json:"notes,omitempty"`
}

// InstanceEdit returns the PATCH /api/controller/kiosks/{code}/instances/{instance_code}
// handler. Cosmetic-only — no audit, no lifecycle event. No command_id
// because there's nothing to dedupe on (a retried edit just overwrites
// with the same values).
func (h *Handlers) InstanceEdit(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := strings.TrimSpace(re.Request.PathValue("code"))
		instanceCode := strings.TrimSpace(re.Request.PathValue("instance_code"))
		if kioskCode == "" || instanceCode == "" {
			return re.BadRequestError("kiosk_code and instance_code are required", nil)
		}
		var body instanceEditRequest
		if err := re.BindBody(&body); err != nil {
			return re.BadRequestError("invalid request body", err)
		}
		if body.Code == nil && body.Serial == nil && body.RFIDEPC == nil && body.Notes == nil {
			return re.BadRequestError("at least one field is required", nil)
		}
		data, err := json.Marshal(instanceEditCommandPayload{
			InstanceCode: instanceCode,
			Code:         body.Code,
			Serial:       body.Serial,
			RFIDEPC:      body.RFIDEPC,
			Notes:        body.Notes,
		})
		if err != nil {
			return re.InternalServerError("marshal command", err)
		}
		return dispatchKioskCommand(re, nc, reg, kioskCode,
			events.InstanceEditCommandSubject(kioskCode), "", data)
	}
}

// --- Decommission / Reactivate ---

type instanceToggleRequest struct {
	Reason string `json:"reason"`
}

type instanceToggleCommandPayload struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	InstanceCode      string `json:"instance_code"`
	Reason            string `json:"reason"`
}

// InstanceDecommission returns the POST .../instances/{instance_code}/decommission
// handler.
func (h *Handlers) InstanceDecommission(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return h.instanceToggleHandler(nc, reg, events.InstanceDecommissionCommandSubject)
}

// InstanceReactivate returns the POST .../instances/{instance_code}/reactivate
// handler.
func (h *Handlers) InstanceReactivate(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return h.instanceToggleHandler(nc, reg, events.InstanceReactivateCommandSubject)
}

func (h *Handlers) instanceToggleHandler(nc *nats.Conn, reg *HeartbeatRegistry, subjectFn func(string) string) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := strings.TrimSpace(re.Request.PathValue("code"))
		instanceCode := strings.TrimSpace(re.Request.PathValue("instance_code"))
		if kioskCode == "" || instanceCode == "" {
			return re.BadRequestError("kiosk_code and instance_code are required", nil)
		}
		var body instanceToggleRequest
		if err := re.BindBody(&body); err != nil {
			return re.BadRequestError("invalid request body", err)
		}
		body.Reason = strings.TrimSpace(body.Reason)
		if body.Reason == "" {
			return re.BadRequestError("reason is required", nil)
		}
		commandID := uuid.NewString()
		data, err := json.Marshal(instanceToggleCommandPayload{
			CommandID:         commandID,
			ControllerAdminID: re.Auth.Id,
			InstanceCode:      instanceCode,
			Reason:            body.Reason,
		})
		if err != nil {
			return re.InternalServerError("marshal command", err)
		}
		return dispatchKioskCommand(re, nc, reg, kioskCode,
			subjectFn(kioskCode), commandID, data)
	}
}

// --- Snapshot (read) ---

type instanceSnapshotCommandPayload struct {
	ItemCode string `json:"item_code,omitempty"`
}

// InstanceSnapshot returns the GET /api/controller/kiosks/{code}/instances
// handler. Optional ?item_code= query filters; empty returns every instance.
func (h *Handlers) InstanceSnapshot(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := strings.TrimSpace(re.Request.PathValue("code"))
		if kioskCode == "" {
			return re.BadRequestError("kiosk code is required", nil)
		}
		itemCode := strings.TrimSpace(re.Request.URL.Query().Get("item_code"))
		data, err := json.Marshal(instanceSnapshotCommandPayload{ItemCode: itemCode})
		if err != nil {
			return re.InternalServerError("marshal command", err)
		}
		return dispatchKioskCommand(re, nc, reg, kioskCode,
			events.InstanceSnapshotCommandSubject(kioskCode), "", data)
	}
}
