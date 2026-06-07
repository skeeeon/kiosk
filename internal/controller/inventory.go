package controller

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
)

// commandTimeout caps how long the controller waits for a kiosk's command
// reply. Local NATS RTT is sub-millisecond and even a complex DB write on
// the kiosk side returns in tens of ms — 5s leaves plenty of headroom
// without making "kiosk offline" feel like a hung browser.
const commandTimeout = 5 * time.Second

// heartbeatFreshness is the window within which we consider a kiosk
// online enough to bother routing a command to it. Slightly larger than
// the kiosk-side heartbeat interval (45s) so a single missed beat
// doesn't lock out remote admin.
const heartbeatFreshness = 90 * time.Second

// inventoryAdjustRequest is the SPA-supplied body for POST
// /api/controller/kiosks/:code/inventory/adjust. command_id is intentionally
// not exposed — the controller generates it server-side so a misbehaving
// SPA can't break idempotency.
type inventoryAdjustRequest struct {
	ItemCode string `json:"item_code"`
	Mode     string `json:"mode"`
	Value    int    `json:"value"`
	Reason   string `json:"reason"`
}

// inventoryAdjustCommandPayload is the JSON the controller sends to the
// kiosk over NATS. Mirrors the shape the kiosk's commands.inventoryAdjust
// handler expects.
type inventoryAdjustCommandPayload struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	ItemCode          string `json:"item_code"`
	Mode              string `json:"mode"`
	Value             int    `json:"value"`
	Reason            string `json:"reason"`
}

// kioskCommandEnvelope is the standard {success, error, data} reply the
// kiosk's dispatcher writes. Generic data because each command type has
// its own data shape — endpoints unmarshal it again per command.
type kioskCommandEnvelope struct {
	Success bool            `json:"success"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// kioskOfflineResponse is the body returned to the SPA when the kiosk's
// heartbeat is stale or its NATS reply doesn't arrive in time. command_id
// is echoed so an operator could (in the future) reconcile by querying
// the kiosk: "did command X actually land?"
type kioskOfflineResponse struct {
	Error     string `json:"error"`
	KioskCode string `json:"kiosk_code"`
	CommandID string `json:"command_id,omitempty"`
}

// InventoryAdjust returns the POST /api/controller/kiosks/:code/inventory/adjust
// handler. Closure pattern keeps the *nats.Conn and *HeartbeatRegistry out
// of the Handlers struct — those are wired only on the controller binary,
// not shared with the kiosk binary which imports the same Handlers type.
func (h *Handlers) InventoryAdjust(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := strings.TrimSpace(re.Request.PathValue("code"))
		if kioskCode == "" {
			return re.BadRequestError("kiosk code is required", nil)
		}

		var body inventoryAdjustRequest
		if err := re.BindBody(&body); err != nil {
			return re.BadRequestError("invalid request body", err)
		}
		body.ItemCode = strings.TrimSpace(body.ItemCode)
		body.Reason = strings.TrimSpace(body.Reason)
		if body.ItemCode == "" {
			return re.BadRequestError("item_code is required", nil)
		}
		if body.Reason == "" {
			return re.BadRequestError("reason is required", nil)
		}
		if body.Mode != "delta" && body.Mode != "absolute" {
			return re.BadRequestError("mode must be 'delta' or 'absolute'", nil)
		}

		commandID := uuid.NewString()
		payload := inventoryAdjustCommandPayload{
			CommandID:         commandID,
			ControllerAdminID: re.Auth.Id,
			ItemCode:          body.ItemCode,
			Mode:              body.Mode,
			Value:             body.Value,
			Reason:            body.Reason,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return re.InternalServerError("marshal command", err)
		}

		// Mutation: the kiosk's reply passes through to the SPA unchanged.
		// Heartbeat fast-fail, request, and envelope decode all live in
		// dispatchKioskCommand (see instances.go).
		return dispatchKioskCommand(re, nc, reg, kioskCode,
			events.InventoryAdjustCommandSubject(kioskCode), commandID, data)
	}
}

// InventorySnapshot returns the GET /api/controller/kiosks/:code/inventory
// handler. Read-only command: same offline behavior as InventoryAdjust but
// no command_id (replays are harmless). The kiosk's reply is enriched with
// out/type the controller derives from its own ledger + catalog before it
// reaches the SPA (see enrichInventorySnapshot).
func (h *Handlers) InventorySnapshot(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := strings.TrimSpace(re.Request.PathValue("code"))
		if kioskCode == "" {
			return re.BadRequestError("kiosk code is required", nil)
		}

		// Empty request body = "all items" on the kiosk side.
		raw, done, err := fetchKioskData(re, nc, reg, kioskCode,
			events.InventorySnapshotCommandSubject(kioskCode), "", []byte("{}"))
		if done {
			return err
		}
		enriched, eerr := enrichInventorySnapshot(h.App, kioskCode, raw)
		if eerr != nil {
			return re.InternalServerError("enrich inventory snapshot", eerr)
		}
		return re.JSON(http.StatusOK, enriched)
	}
}
