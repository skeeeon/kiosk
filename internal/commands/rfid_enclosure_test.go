package commands

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/cartevents"
	"github.com/skeeeon/kiosk/internal/handlers"
)

// seedActiveUser creates a users row the cart.start handler can resolve.
func seedActiveUser(t *testing.T, app core.App, code string, active bool) {
	t.Helper()
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	u := core.NewRecord(users)
	u.Set("email", code+"@test.local")
	u.Set("name", "Worker "+code)
	u.Set("code", code)
	u.Set("role", "worker")
	u.Set("active", active)
	u.SetPassword("password-" + code + "-padding")
	if err := app.Save(u); err != nil {
		t.Fatalf("save user %s: %v", code, err)
	}
}

// newRFIDDispatcher wires a dispatcher with the KioskHandlers deps the
// enclosure_diff commands reach into (cart store + SSE broker), but no RFID
// reader — enough to exercise routing, validation, and idempotency.
func newRFIDDispatcher(app core.App) *Dispatcher {
	d := NewDispatcher(app, "KIOSK01")
	d.KioskHandlers = &handlers.Handlers{
		App:        app,
		Carts:      cart.NewStore(time.Hour),
		CartEvents: cartevents.NewBroker(),
	}
	return d
}

func TestHandleCartStart_NilKioskHandlers(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01") // KioskHandlers deliberately unset
	reply := d.handleCartStart(context.Background(), []byte(`{"user_code":"EMP-1","door_id":"BAY-A"}`))
	if reply.Success {
		t.Fatal("expected failure when KioskHandlers is nil")
	}
}

func TestHandleCartStart_Validation(t *testing.T) {
	app := setupApp(t)
	d := newRFIDDispatcher(app)

	cases := []struct{ name, payload string }{
		{"bad json", `{`},
		{"missing user_code", `{"door_id":"BAY-A"}`},
		{"missing door_id", `{"user_code":"EMP-1"}`},
		{"unknown user", `{"user_code":"NOPE","door_id":"BAY-A"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply := d.handleCartStart(context.Background(), []byte(c.payload))
			if reply.Success {
				t.Errorf("expected failure for %s, got success", c.name)
			}
			if reply.Error == "" {
				t.Errorf("expected non-empty error for %s", c.name)
			}
		})
	}
}

func TestHandleCartStart_InactiveUserRejected(t *testing.T) {
	app := setupApp(t)
	seedActiveUser(t, app, "EMP-OFF", false)
	d := newRFIDDispatcher(app)

	reply := d.handleCartStart(context.Background(), []byte(`{"user_code":"EMP-OFF","door_id":"BAY-A"}`))
	if reply.Success {
		t.Fatal("expected inactive user to be rejected")
	}
}

func TestHandleCartStart_SuccessAndIdempotentReuse(t *testing.T) {
	app := setupApp(t)
	seedActiveUser(t, app, "EMP-1", true)
	d := newRFIDDispatcher(app)

	payload := []byte(`{"user_code":"EMP-1","door_id":"BAY-A"}`)
	first := d.handleCartStart(context.Background(), payload)
	if !first.Success {
		t.Fatalf("first cart.start failed: %s", first.Error)
	}
	r1, ok := first.Data.(cartStartReply)
	if !ok {
		t.Fatalf("reply data type: %T", first.Data)
	}
	if r1.Reused {
		t.Error("first fire should not be reused")
	}
	if r1.CartID == "" {
		t.Error("expected a cart_id")
	}

	// Re-fire the same (user, door): must collapse to the same cart, reused=true.
	second := d.handleCartStart(context.Background(), payload)
	r2, ok := second.Data.(cartStartReply)
	if !ok {
		t.Fatalf("reply data type: %T", second.Data)
	}
	if !r2.Reused {
		t.Error("second fire for same (user,door) should be reused")
	}
	if r2.CartID != r1.CartID {
		t.Errorf("re-fire should return same cart: %q vs %q", r2.CartID, r1.CartID)
	}
}

func TestHandleReadTrigger_NilKioskHandlers(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")
	reply := d.handleReadTrigger(context.Background(), []byte(`{"cart_id":"x"}`))
	if reply.Success {
		t.Fatal("expected failure when KioskHandlers is nil")
	}
}

func TestHandleReadTrigger_AnonymousReadRejected(t *testing.T) {
	app := setupApp(t)
	d := newRFIDDispatcher(app)

	// Neither cart_id nor (user_code + door_id) → no cart to anchor the read.
	reply := d.handleReadTrigger(context.Background(), []byte(`{}`))
	if reply.Success {
		t.Fatal("expected failure: read with no resolvable cart must be rejected")
	}

	// A cart_id that doesn't resolve is likewise rejected.
	reply = d.handleReadTrigger(context.Background(), []byte(`{"cart_id":"does-not-exist"}`))
	if reply.Success {
		t.Fatal("expected failure for unknown cart_id")
	}
}

func TestHandleReadTrigger_ReaderNotConnected(t *testing.T) {
	app := setupApp(t)
	seedActiveUser(t, app, "EMP-1", true)
	d := newRFIDDispatcher(app) // KioskHandlers set, RFID left nil

	// Start a cart so resolveTriggerCart succeeds and we reach the RFID gate.
	c, _ := d.KioskHandlers.Carts.StartByExternal("", "EMP-1", "Worker", "worker", "BAY-A")
	payload, _ := json.Marshal(map[string]any{"cart_id": c.ID})
	reply := d.handleReadTrigger(context.Background(), payload)
	if reply.Success {
		t.Fatal("expected failure when no RFID reader is connected")
	}
}
