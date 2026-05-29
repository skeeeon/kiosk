package commands

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"
)

// These drive onMessage with hand-built *nats.Msg values (no live server).
// msg.Respond on a connectionless message returns an error that onMessage
// logs and ignores, so we can't assert the reply *payload* here — but we can
// assert the routing decisions and the "must never crash the subscriber"
// contract, which is the part that previously had zero coverage. End-to-end
// reply-envelope assertions would need an embedded nats-server (a heavy
// test-only dependency we deliberately don't pull in).

func TestOnMessage_MissingReplyInboxSkipsDispatch(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")
	called := false
	d.HandleFunc("probe", func(context.Context, []byte) Reply {
		called = true
		return Reply{Success: true}
	})
	// No Reply inbox → the caller isn't listening; we must not dispatch (and
	// must not mutate state for a fire-and-forget with nowhere to answer).
	d.onMessage(&nats.Msg{Subject: "kiosk.KIOSK01.command.probe", Data: []byte("{}")})
	if called {
		t.Error("handler must not run when no reply inbox is set")
	}
}

func TestOnMessage_RoutesKnownCommand(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")
	called := false
	d.HandleFunc("probe", func(context.Context, []byte) Reply {
		called = true
		return Reply{Success: true}
	})
	d.onMessage(&nats.Msg{Subject: "kiosk.KIOSK01.command.probe", Reply: "_INBOX.x", Data: []byte("{}")})
	if !called {
		t.Error("known command should route to its handler")
	}
}

func TestOnMessage_PanicInHandlerRecovers(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")
	d.HandleFunc("boom", func(context.Context, []byte) Reply {
		panic("handler blew up")
	})
	// The documented contract: a handler panic must be recovered and answered,
	// never take the subscriber goroutine down. If recover() were missing,
	// this call would panic and fail the test.
	d.onMessage(&nats.Msg{Subject: "kiosk.KIOSK01.command.boom", Reply: "_INBOX.x", Data: []byte("{}")})
}

func TestOnMessage_UnknownCommandDoesNotCrash(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")
	// Unknown suffix → structured "unknown command" reply, no dispatch, no
	// crash. (Reaching the end of the test is the assertion.)
	d.onMessage(&nats.Msg{Subject: "kiosk.KIOSK01.command.nope", Reply: "_INBOX.x", Data: []byte("{}")})
}
