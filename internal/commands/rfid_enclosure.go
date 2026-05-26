// rfid_enclosure.go holds the Phase-4 enclosure_diff command
// handlers. cart.start and read.trigger are the two NATS commands
// external systems (access-control gates, camera/occupancy
// monitors) publish to drive a kiosk through one enclosure visit.
// See docs/rfid.md.
//
// Both handlers reach through d.KioskHandlers for the cart store,
// SSE broker, and LLRP reader. That field is populated by
// cmd/kiosk/main.go after Handlers and the Dispatcher are both
// constructed; tests that exercise only inventory/instance commands
// don't need to set it. cart.start and read.trigger will return a
// structured error if it's nil.
package commands

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pocketbase/dbx"

	"github.com/skeeeon/kiosk/internal/cart"
)

// cartStartRequest is the payload external systems publish on
// <prefix>.<kiosk_code>.command.cart.start. command_id is for
// caller-side traceability; the kiosk doesn't use it for dedup —
// the (user_code, door_id) key in the cart store does that.
type cartStartRequest struct {
	UserCode  string `json:"user_code"`
	DoorID    string `json:"door_id"`
	CommandID string `json:"command_id"`
}

// cartStartReply is the {data} payload returned to the caller. Reused
// gives the external system a clear signal that its retry collapsed
// into the existing cart rather than created a new one — useful for
// debugging when several access-control fires arrive in close
// succession.
type cartStartReply struct {
	CartID    string `json:"cart_id"`
	UserCode  string `json:"user_code"`
	DoorID    string `json:"door_id"`
	Reused    bool   `json:"reused"`
}

func (d *Dispatcher) handleCartStart(_ context.Context, payload []byte) Reply {
	if d.KioskHandlers == nil {
		return Reply{Success: false, Error: "kiosk handlers not wired (enclosure_diff disabled or misconfigured)"}
	}
	var req cartStartRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return Reply{Success: false, Error: "invalid payload: " + err.Error()}
	}
	if req.UserCode == "" {
		return Reply{Success: false, Error: "user_code is required"}
	}
	if req.DoorID == "" {
		return Reply{Success: false, Error: "door_id is required"}
	}

	user, err := d.app.FindFirstRecordByFilter("users",
		"code = {:c}", dbx.Params{"c": req.UserCode})
	if err != nil {
		return Reply{Success: false, Error: "user not found: " + req.UserCode}
	}
	if !user.GetBool("active") {
		return Reply{Success: false, Error: "user is inactive: " + req.UserCode}
	}

	c, reused := d.KioskHandlers.Carts.StartByExternal(
		user.Id, user.GetString("code"), user.GetString("name"), user.GetString("role"),
		req.DoorID,
	)

	// SSE tickle: a fresh cart needs the SPA to refetch so the cart
	// shows up immediately. Reused calls also tickle so any second
	// client (outside-enclosure screen) sees consistent state. The
	// broker's drop-on-full semantics make this safe to fire even
	// when nothing's subscribed.
	d.KioskHandlers.CartEvents.Tickle(c.ID)

	d.logger.Info("kiosk.commands.cart_start",
		"command_id", req.CommandID,
		"user_code", req.UserCode,
		"door_id", req.DoorID,
		"cart_id", c.ID,
		"reused", reused)

	return Reply{Success: true, Data: cartStartReply{
		CartID:   c.ID,
		UserCode: req.UserCode,
		DoorID:   req.DoorID,
		Reused:   reused,
	}}
}

// readTriggerRequest accepts either CartID or (UserCode + DoorID).
// External systems will typically know one or the other depending
// on which event fired — camera/occupancy systems may not have the
// cart_id from the original cart.start reply but will know the
// door, while a fully-coordinated controller might track cart_ids
// directly.
type readTriggerRequest struct {
	CartID    string `json:"cart_id"`
	UserCode  string `json:"user_code"`
	DoorID    string `json:"door_id"`
	CommandID string `json:"command_id"`
}

func (d *Dispatcher) handleReadTrigger(ctx context.Context, payload []byte) Reply {
	if d.KioskHandlers == nil {
		return Reply{Success: false, Error: "kiosk handlers not wired (enclosure_diff disabled or misconfigured)"}
	}
	var req readTriggerRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return Reply{Success: false, Error: "invalid payload: " + err.Error()}
	}

	c, err := d.resolveTriggerCart(req)
	if err != nil {
		// Anonymous-read rejection. The design doc is explicit:
		// "Failing loud surfaces the problem" — better than starting
		// an unanchored cart that the worker can't see.
		return Reply{Success: false, Error: err.Error()}
	}

	if d.KioskHandlers.RFID == nil {
		return Reply{Success: false, Error: "rfid reader is not connected"}
	}

	result, err := d.KioskHandlers.PerformReadTrigger(ctx, c)
	if err != nil {
		return Reply{Success: false, Error: err.Error()}
	}

	d.logger.Info("kiosk.commands.read_trigger",
		"command_id", req.CommandID,
		"cart_id", c.ID,
		"door_id", c.DoorID,
		"observed", len(result.ObservedEPCs),
		"added", len(result.AddedLines),
		"unresolved", len(result.UnresolvedEPCs),
		"skipped_cross_user", result.SkippedCrossUserCount)

	return Reply{Success: true, Data: result}
}

// resolveTriggerCart picks the right lookup based on the payload.
// Prefers explicit cart_id when set; falls back to (user_code,
// door_id) otherwise. Either path that doesn't find an active cart
// returns an error so callers don't silently start anonymous reads.
func (d *Dispatcher) resolveTriggerCart(req readTriggerRequest) (*cart.Cart, error) {
	if req.CartID != "" {
		c, err := d.KioskHandlers.Carts.Get(req.CartID)
		if err != nil {
			return nil, errors.New("no active cart for cart_id: " + req.CartID)
		}
		return c, nil
	}
	if req.UserCode == "" || req.DoorID == "" {
		return nil, errors.New("either cart_id or (user_code + door_id) is required")
	}
	c, err := d.KioskHandlers.Carts.GetByUserDoor(req.UserCode, req.DoorID)
	if err != nil {
		return nil, errors.New("no active cart for user_code=" + req.UserCode + " door_id=" + req.DoorID)
	}
	return c, nil
}
