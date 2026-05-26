// cart_events.go owns the SSE endpoint that lets the SPA react to
// cart writes it didn't initiate (Phase 3 in docs/rfid.md). The wire
// protocol is plain Server-Sent Events: one named event per signal,
// empty-ish JSON data payloads. The SPA refetches via GET cart on
// every signal, so payload size is intentionally tiny.
package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cartevents"
)

// sseHeartbeatInterval keeps the connection warm through any proxy
// that closes idle TCP. 15s is comfortably under the typical 30-60s
// proxy timeout. The line starts with `:` so EventSource ignores it
// per the SSE spec — it's a no-op event purely to push bytes.
const sseHeartbeatInterval = 15 * time.Second

// CartEventsStream is a long-lived SSE handler. It subscribes to the
// broker for the cart_id in the query string, then loops forwarding
// signals to the client until either (a) the broker sends EventGone,
// (b) the client disconnects, or (c) the heartbeat ticker fails to
// write because the connection is dead.
//
// Endpoint: GET /api/kiosk/cart/events?cart_id=<id>
//
// Wire format (one record per signal):
//
//	event: cart.updated
//	data: {}
//
//	event: cart.gone
//	data: {}
//
// Heartbeats are SSE comments (`: ping\n\n`) — they don't fire any
// EventSource handler and don't generate UI noise.
func (h *Handlers) CartEventsStream(re *core.RequestEvent) error {
	cartID := re.Request.URL.Query().Get("cart_id")
	if cartID == "" {
		return re.BadRequestError("cart_id is required", nil)
	}
	// Validate cart existence up front so a bogus cart_id fails as
	// 404 at connect time rather than silently going dark.
	if _, err := h.Carts.Get(cartID); err != nil {
		return re.NotFoundError("cart not found or expired", nil)
	}

	// SSE response headers. WriteHeader must come after the headers
	// are set; PocketBase/Echo's response writer is a plain
	// http.ResponseWriter underneath so this is the standard pattern.
	re.Response.Header().Set("Content-Type", "text/event-stream")
	re.Response.Header().Set("Cache-Control", "no-cache")
	re.Response.Header().Set("Connection", "keep-alive")
	// X-Accel-Buffering: no opts out of nginx's response buffering
	// (which would batch our line-at-a-time writes into chunks the
	// client only sees on flush). Harmless when no nginx is in front.
	re.Response.Header().Set("X-Accel-Buffering", "no")
	re.Response.WriteHeader(http.StatusOK)

	flusher, ok := re.Response.(http.Flusher)
	if !ok {
		// Shouldn't happen against standard net/http but document it
		// clearly if it ever does — better than a silently-buffered
		// stream that never delivers.
		return fmt.Errorf("response writer does not support flushing")
	}

	ch, unsub := h.CartEvents.Subscribe(cartID)
	defer unsub()

	// Initial flush so the client gets the 200 + headers immediately
	// rather than waiting for the first signal. EventSource considers
	// the connection "open" once headers arrive.
	flusher.Flush()

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	ctx := re.Request.Context()
	for {
		select {
		case <-ctx.Done():
			// Client disconnected (tab closed, navigated away,
			// network died). unsub via defer cleans up.
			return nil

		case sig, open := <-ch:
			if !open {
				// Channel closed unexpectedly (broker shut down, or
				// unsub racing — defensive). Treat as end-of-stream.
				return nil
			}
			if _, err := fmt.Fprintf(re.Response, "event: %s\ndata: {}\n\n", sig.Kind); err != nil {
				// Write failure means the client connection is dead;
				// no point continuing. Returning here triggers defer
				// unsub.
				return nil
			}
			flusher.Flush()
			if sig.Kind == cartevents.EventGone {
				// Terminal signal — cart committed or cancelled. We've
				// already forwarded EventGone; close the stream so the
				// SPA's EventSource onerror / our explicit close in
				// useCartEvents tears down the subscription.
				return nil
			}

		case <-heartbeat.C:
			if _, err := fmt.Fprint(re.Response, ": ping\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}
