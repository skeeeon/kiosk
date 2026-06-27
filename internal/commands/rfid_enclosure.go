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
	"time"

	"github.com/pocketbase/dbx"

	"github.com/skeeeon/kiosk/internal/cart"
)

// ReadTriggerBudget bounds the whole read.trigger command. It must sit below
// the controller's ~5s command-reply timeout and above the maximum enclosure
// read_window (config.MaxEnclosureReadWindow) so a normal read completes but
// a wedged/half-open LLRP reader can't block ReadFor — and therefore the
// reader's serialization lock — indefinitely. When this deadline fires,
// ReadFor's ctx.Done() path unwinds, releases readMu, and the handler replies
// with an error inside the reply window instead of timing the caller out.
const ReadTriggerBudget = 4500 * time.Millisecond

// cartStartRequest is the payload external systems publish on
// <prefix>.<kiosk_code>.command.cart.start. command_id is for
// caller-side traceability; the kiosk doesn't use it for dedup —
// the (user_code, enclosure_id) key in the cart store does that.
type cartStartRequest struct {
	UserCode    string `json:"user_code"`
	EnclosureID string `json:"enclosure_id"`
	CommandID   string `json:"command_id"`
}

// cartStartReply is the {data} payload returned to the caller. Reused
// gives the external system a clear signal that its retry collapsed
// into the existing cart rather than created a new one — useful for
// debugging when several access-control fires arrive in close
// succession.
type cartStartReply struct {
	CartID      string `json:"cart_id"`
	UserCode    string `json:"user_code"`
	EnclosureID string `json:"enclosure_id"`
	Reused      bool   `json:"reused"`
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
	if req.EnclosureID == "" {
		return Reply{Success: false, Error: "enclosure_id is required"}
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
		req.EnclosureID,
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
		"enclosure_id", req.EnclosureID,
		"cart_id", c.ID,
		"reused", reused)

	return Reply{Success: true, Data: cartStartReply{
		CartID:      c.ID,
		UserCode:    req.UserCode,
		EnclosureID: req.EnclosureID,
		Reused:      reused,
	}}
}

// readTriggerRequest accepts either CartID or (UserCode + EnclosureID).
// External systems will typically know one or the other depending
// on which event fired — camera/occupancy systems may not have the
// cart_id from the original cart.start reply but will know the
// enclosure, while a fully-coordinated controller might track cart_ids
// directly.
type readTriggerRequest struct {
	CartID      string `json:"cart_id"`
	UserCode    string `json:"user_code"`
	EnclosureID string `json:"enclosure_id"`
	CommandID   string `json:"command_id"`
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

	rd, ok := d.KioskHandlers.ReaderForEnclosure(c.EnclosureID)
	if !ok || rd.Reader == nil {
		return Reply{Success: false, Error: "rfid reader is not connected"}
	}

	// Bound the read so a slow or half-open reader can't hold the reader's
	// serialization lock past the caller's reply window. The dispatcher hands
	// us a context.Background() (no deadline); derive one here.
	ctx, cancel := context.WithTimeout(ctx, ReadTriggerBudget)
	defer cancel()

	result, err := d.KioskHandlers.PerformReadTrigger(ctx, c, rd)
	if err != nil {
		return Reply{Success: false, Error: err.Error()}
	}

	d.logger.Info("kiosk.commands.read_trigger",
		"command_id", req.CommandID,
		"cart_id", c.ID,
		"enclosure_id", c.EnclosureID,
		"observed", len(result.ObservedEPCs),
		"added", len(result.AddedLines),
		"unresolved", len(result.UnresolvedEPCs),
		"skipped_cross_user", result.SkippedCrossUserCount)

	return Reply{Success: true, Data: result}
}

// resolveTriggerCart picks the right lookup based on the payload.
// Prefers explicit cart_id when set; falls back to (user_code,
// enclosure_id) otherwise. Either path that doesn't find an active cart
// returns an error so callers don't silently start anonymous reads.
func (d *Dispatcher) resolveTriggerCart(req readTriggerRequest) (*cart.Cart, error) {
	if req.CartID != "" {
		c, err := d.KioskHandlers.Carts.Get(req.CartID)
		if err != nil {
			return nil, errors.New("no active cart for cart_id: " + req.CartID)
		}
		return c, nil
	}
	if req.UserCode == "" || req.EnclosureID == "" {
		return nil, errors.New("either cart_id or (user_code + enclosure_id) is required")
	}
	c, err := d.KioskHandlers.Carts.GetByUserEnclosure(req.UserCode, req.EnclosureID)
	if err != nil {
		return nil, errors.New("no active cart for user_code=" + req.UserCode + " enclosure_id=" + req.EnclosureID)
	}
	return c, nil
}
