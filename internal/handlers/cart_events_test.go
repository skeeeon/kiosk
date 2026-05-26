package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/cartevents"
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/handlers"
	"github.com/skeeeon/kiosk/internal/notifications"
)

// runCartEventsStream invokes CartEventsStream against a real
// ResponseRecorder + RequestEvent in a goroutine. Returns a `done`
// channel that closes after the handler returns and the recorder so
// the caller can poll the body for emitted events. Context cancel is
// what tears the stream down from the test side.
func runCartEventsStream(
	t *testing.T, h *handlers.Handlers, cartID string, ctx context.Context,
) (*httptest.ResponseRecorder, <-chan error) {
	t.Helper()

	url := "/api/kiosk/cart/events"
	if cartID != "" {
		url += "?cart_id=" + cartID
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	rec := httptest.NewRecorder()

	e := new(core.RequestEvent)
	e.App = h.App
	e.Request = req
	e.Response = rec

	done := make(chan error, 1)
	go func() {
		done <- h.CartEventsStream(e)
	}()
	return rec, done
}

// bodyContainsWithin polls the recorder body until it includes the
// substring or the deadline expires. The polling pattern (10ms ticks)
// is fine here — the handler writes synchronously after Tickle, so
// the body grows within microseconds; this loop just avoids racing on
// the first iteration.
func bodyContainsWithin(t *testing.T, rec *httptest.ResponseRecorder, want string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(rec.Body.String(), want) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestCartEventsStream_TickleDelivered: subscribe, broker.Tickle once,
// assert the handler emits a `cart.updated` SSE event within the read
// window. Tear down via ctx cancel.
func TestCartEventsStream_TickleDelivered(t *testing.T) {
	app := setupApp(t)

	store := cart.NewStore(5 * time.Minute)
	c := store.Start("u1", "U-001", "User One", "worker")

	h := handlers.New(app, &config.Config{}, store, notifications.New(app))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec, done := runCartEventsStream(t, h, c.ID, ctx)

	// Briefly wait for the handler to install its subscription; the
	// goroutine's Subscribe runs before Tickle would be a no-op if it
	// fired immediately. A small sleep is the cleanest signal-free way
	// to wait for that.
	time.Sleep(50 * time.Millisecond)

	h.CartEvents.Tickle(c.ID)

	if !bodyContainsWithin(t, rec, "event: cart.updated", time.Second) {
		t.Fatalf("did not see cart.updated; body: %q", rec.Body.String())
	}

	cancel()
	select {
	case <-done:
		// handler returned cleanly on ctx cancel
	case <-time.After(time.Second):
		t.Errorf("handler did not return within 1s of ctx cancel")
	}
}

// TestCartEventsStream_CloseTerminates: broker.Close fires cart.gone,
// the handler writes it and returns. Important regression guard:
// the SSE stream must terminate so the EventSource doesn't reconnect
// against a now-404 cart.
func TestCartEventsStream_CloseTerminates(t *testing.T) {
	app := setupApp(t)
	store := cart.NewStore(5 * time.Minute)
	c := store.Start("u1", "U-001", "User One", "worker")
	h := handlers.New(app, &config.Config{}, store, notifications.New(app))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec, done := runCartEventsStream(t, h, c.ID, ctx)
	time.Sleep(50 * time.Millisecond)

	h.CartEvents.Close(c.ID)

	if !bodyContainsWithin(t, rec, "event: cart.gone", time.Second) {
		t.Fatalf("did not see cart.gone; body: %q", rec.Body.String())
	}

	// Handler should return on its own after writing cart.gone — no ctx
	// cancel needed.
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("handler returned error after cart.gone: %v", err)
		}
	case <-time.After(time.Second):
		t.Errorf("handler didn't return after cart.gone")
	}
}

// TestCartEventsStream_MultipleTickles: confirms each Tickle produces
// a distinct event in the body. Same broker subscriber receives N
// signals; the SPA refetches N times. Buffer size > 1 so this
// doesn't get drop-on-full clipped.
func TestCartEventsStream_MultipleTickles(t *testing.T) {
	app := setupApp(t)
	store := cart.NewStore(5 * time.Minute)
	c := store.Start("u1", "U-001", "User One", "worker")
	h := handlers.New(app, &config.Config{}, store, notifications.New(app))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec, done := runCartEventsStream(t, h, c.ID, ctx)
	time.Sleep(50 * time.Millisecond)

	h.CartEvents.Tickle(c.ID)
	h.CartEvents.Tickle(c.ID)
	h.CartEvents.Tickle(c.ID)

	// Wait until at least three event lines land.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(rec.Body.String(), "event: cart.updated") >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := strings.Count(rec.Body.String(), "event: cart.updated"); got < 3 {
		t.Errorf("want >=3 cart.updated lines, got %d; body: %q", got, rec.Body.String())
	}

	cancel()
	<-done
}

// TestCartEventsStream_MissingCartID: connect-time validation. Empty
// cart_id is a 400; we use PB's bad-request error which carries a 400
// status in its returned Error type. We check that error path is
// taken (non-nil return) and the body isn't streamed.
func TestCartEventsStream_MissingCartID(t *testing.T) {
	app := setupApp(t)
	store := cart.NewStore(5 * time.Minute)
	h := handlers.New(app, &config.Config{}, store, notifications.New(app))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec, done := runCartEventsStream(t, h, "", ctx)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error for missing cart_id, got nil")
		}
		// PB's BadRequestError capitalizes the first letter; match
		// case-insensitively so we're not coupled to that detail.
		if !strings.Contains(strings.ToLower(err.Error()), "cart_id") {
			t.Errorf("error should mention cart_id; got %q", err.Error())
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not return promptly for missing cart_id")
	}
	if strings.Contains(rec.Body.String(), "event:") {
		t.Errorf("no SSE events should have been streamed; body: %q", rec.Body.String())
	}
}

// TestCartEventsStream_UnknownCartID: connect-time validation. An
// unknown cart_id is a 404. We assert via the returned error rather
// than the recorder status because PB error helpers return error
// values rather than write directly until the router unwraps them.
func TestCartEventsStream_UnknownCartID(t *testing.T) {
	app := setupApp(t)
	store := cart.NewStore(5 * time.Minute)
	h := handlers.New(app, &config.Config{}, store, notifications.New(app))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, done := runCartEventsStream(t, h, "no-such-cart", ctx)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error for unknown cart_id, got nil")
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not return promptly for unknown cart_id")
	}
}

// TestCartEventsStream_IndependentSubscribers: two subscribers on the
// same cart each get their own copy of the tickle (broker fan-out).
// This is the property Phase 4's inside-enclosure-screen and
// outside-enclosure-screen pair will eventually rely on.
func TestCartEventsStream_IndependentSubscribers(t *testing.T) {
	app := setupApp(t)
	store := cart.NewStore(5 * time.Minute)
	c := store.Start("u1", "U-001", "User One", "worker")
	h := handlers.New(app, &config.Config{}, store, notifications.New(app))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recA, doneA := runCartEventsStream(t, h, c.ID, ctx)
	recB, doneB := runCartEventsStream(t, h, c.ID, ctx)
	time.Sleep(50 * time.Millisecond)

	h.CartEvents.Tickle(c.ID)

	if !bodyContainsWithin(t, recA, "event: cart.updated", time.Second) {
		t.Errorf("subscriber A missed the tickle")
	}
	if !bodyContainsWithin(t, recB, "event: cart.updated", time.Second) {
		t.Errorf("subscriber B missed the tickle")
	}

	cancel()
	<-doneA
	<-doneB
}

// TestCartAdd_FiresTickle is the integration-side check that the
// broker is actually wired into the cart write path. Without this, a
// future refactor could quietly drop the tickle and the per-handler
// unit test wouldn't catch it.
func TestCartAdd_FiresTickle(t *testing.T) {
	app := setupApp(t)

	// Seed: one item the cart can add.
	items, _ := app.FindCollectionByNameOrId("items")
	item := core.NewRecord(items)
	item.Set("code", "WIDGET")
	item.Set("name", "Widget")
	item.Set("type", "consumable")
	item.Set("tracking_mode", "quantity")
	item.Set("active", true)
	item.Set("quantity_on_hand", 100)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}

	store := cart.NewStore(5 * time.Minute)
	c := store.Start("u1", "U-001", "User One", "worker")
	h := handlers.New(app, &config.Config{}, store, notifications.New(app))

	// Subscribe before any writes so we see the tickle.
	ch, unsub := h.CartEvents.Subscribe(c.ID)
	defer unsub()

	// Call CartAdd through its HTTP shape. We construct a real
	// RequestEvent the same way PB's apis tests do.
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/cart/add",
		strings.NewReader(`{"cart_id":"`+c.ID+`","item_code":"WIDGET"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e := new(core.RequestEvent)
	e.App = app
	e.Request = req
	e.Response = rec

	if err := h.CartAdd(e); err != nil {
		t.Fatalf("CartAdd: %v", err)
	}

	select {
	case sig := <-ch:
		if sig.Kind != cartevents.EventUpdated {
			t.Errorf("expected EventUpdated, got %q", sig.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("CartAdd did not fire a broker tickle")
	}
}
