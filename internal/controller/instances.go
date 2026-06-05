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

// Instance management endpoints. They mirror the inventory.adjust pattern from
// inventory.go: heartbeat fast-fail → marshal payload → nc.Request(subject,
// ..., 5s) → decode kioskCommandEnvelope → pass `data` through to the SPA as
// raw JSON. Idempotency uses a server-generated command_id for mutations; the
// snapshot read needs none. Editing is cosmetic-only and writes no audit row,
// but still goes through a command for consistency.

// dispatchKioskCommand is the shared shape for "send a command to a
// kiosk, decode the envelope, route the outcome." Extracted because the
// inventory + instance + maintenance handlers all repeat the same closure of
// "online check + Request + envelope decode."
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
	data, done, err := fetchKioskData(re, nc, reg, kioskCode, subject, commandID, payload)
	if done {
		return err
	}
	return re.JSON(http.StatusOK, json.RawMessage(data))
}

// fetchKioskData runs a kiosk command and returns the kiosk's `data` payload.
// When `done` is true the response has ALREADY been written to `re` (kiosk
// offline, a kiosk-level error, or a transport/decode failure) and the caller
// must return `err` unchanged without touching `re` again. When `done` is
// false the command succeeded and `data` holds the reply for the caller to
// pass through or enrich. Split out of dispatchKioskCommand so read handlers
// can decorate the reply before sending it (see snapshot_enrich.go).
func fetchKioskData(
	re *core.RequestEvent,
	nc *nats.Conn,
	reg *HeartbeatRegistry,
	kioskCode, subject, commandID string,
	payload []byte,
) (data json.RawMessage, done bool, err error) {
	if !reg.IsLikelyOnline(kioskCode, heartbeatFreshness) {
		if time.Since(reg.StartedAt()) > heartbeatFreshness {
			return nil, true, re.JSON(http.StatusServiceUnavailable, kioskOfflineResponse{
				Error:     "kiosk_offline",
				KioskCode: kioskCode,
				CommandID: commandID,
			})
		}
	}
	msg, rerr := nc.Request(subject, payload, commandTimeout)
	if rerr != nil {
		if errors.Is(rerr, nats.ErrTimeout) || errors.Is(rerr, nats.ErrNoResponders) {
			return nil, true, re.JSON(http.StatusServiceUnavailable, kioskOfflineResponse{
				Error:     "kiosk_offline",
				KioskCode: kioskCode,
				CommandID: commandID,
			})
		}
		return nil, true, re.InternalServerError("nats request failed", rerr)
	}
	var env kioskCommandEnvelope
	if jerr := json.Unmarshal(msg.Data, &env); jerr != nil {
		return nil, true, re.InternalServerError("decode kiosk reply", jerr)
	}
	if !env.Success {
		return nil, true, re.JSON(http.StatusBadGateway, map[string]any{
			"error":      "kiosk_error",
			"detail":     env.Error,
			"kiosk_code": kioskCode,
			"command_id": commandID,
		})
	}
	return env.Data, false, nil
}

// --- Create ---

type instanceCreateRequest struct {
	ItemCode string `json:"item_code"`
	Code     string `json:"code"`
	Serial   string `json:"serial,omitempty"`
	RFIDEPC  string `json:"rfid_epc,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

type instanceCreateCommandPayload struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	ItemCode          string `json:"item_code"`
	Code              string `json:"code"`
	Serial            string `json:"serial,omitempty"`
	RFIDEPC           string `json:"rfid_epc,omitempty"`
	Notes             string `json:"notes,omitempty"`
}

// InstanceCreate returns the POST /api/controller/kiosks/{code}/instances
// handler. The controller server-generates the command_id; the SPA never
// supplies one. New instances are created in_service.
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

// --- Set status (maintenance / return to service / retire / un-retire) ---

type instanceSetStatusRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type instanceSetStatusCommandPayload struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	InstanceCode      string `json:"instance_code"`
	Status            string `json:"status"`
	Reason            string `json:"reason"`
}

// InstanceSetStatus returns the POST .../instances/{instance_code}/status
// handler. The body carries the target status (in_service | maintenance |
// retired) plus a reason; one endpoint covers send-to-maintenance,
// return-to-service, retire, and un-retire. The kiosk derives the audit action
// verb from the (prev → target) transition.
func (h *Handlers) InstanceSetStatus(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := strings.TrimSpace(re.Request.PathValue("code"))
		instanceCode := strings.TrimSpace(re.Request.PathValue("instance_code"))
		if kioskCode == "" || instanceCode == "" {
			return re.BadRequestError("kiosk_code and instance_code are required", nil)
		}
		var body instanceSetStatusRequest
		if err := re.BindBody(&body); err != nil {
			return re.BadRequestError("invalid request body", err)
		}
		body.Status = strings.TrimSpace(body.Status)
		body.Reason = strings.TrimSpace(body.Reason)
		switch body.Status {
		case "in_service", "maintenance", "retired":
		default:
			return re.BadRequestError("status must be in_service, maintenance, or retired", nil)
		}
		if body.Reason == "" {
			return re.BadRequestError("reason is required", nil)
		}
		commandID := uuid.NewString()
		data, err := json.Marshal(instanceSetStatusCommandPayload{
			CommandID:         commandID,
			ControllerAdminID: re.Auth.Id,
			InstanceCode:      instanceCode,
			Status:            body.Status,
			Reason:            body.Reason,
		})
		if err != nil {
			return re.InternalServerError("marshal command", err)
		}
		return dispatchKioskCommand(re, nc, reg, kioskCode,
			events.InstanceSetStatusCommandSubject(kioskCode), commandID, data)
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
		payload, err := json.Marshal(instanceSnapshotCommandPayload{ItemCode: itemCode})
		if err != nil {
			return re.InternalServerError("marshal command", err)
		}
		raw, done, rerr := fetchKioskData(re, nc, reg, kioskCode,
			events.InstanceSnapshotCommandSubject(kioskCode), "", payload)
		if done {
			return rerr
		}
		// Decorate each instance with its current "out" status, derived from
		// the controller's projected ledger — the kiosk's snapshot doesn't
		// carry it.
		enriched, eerr := enrichInstanceSnapshot(h.App, kioskCode, raw)
		if eerr != nil {
			return re.InternalServerError("enrich instance snapshot", eerr)
		}
		return re.JSON(http.StatusOK, enriched)
	}
}
