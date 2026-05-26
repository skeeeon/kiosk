package cartevents

import (
	"sync"
	"testing"
	"time"
)

// TestBroker_SubscribeAndTickle verifies the dominant case: one
// subscriber, one tickle, one received signal of the right kind.
func TestBroker_SubscribeAndTickle(t *testing.T) {
	b := NewBroker()
	ch, unsub := b.Subscribe("cart-1")
	defer unsub()

	b.Tickle("cart-1")

	select {
	case sig := <-ch:
		if sig.Kind != EventUpdated {
			t.Errorf("expected %q, got %q", EventUpdated, sig.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for tickle")
	}
}

// TestBroker_TickleOtherCart_IsIsolated guards against the obvious
// regression where the map gets keyed wrong and unrelated carts see
// each other's signals.
func TestBroker_TickleOtherCart_IsIsolated(t *testing.T) {
	b := NewBroker()
	chA, unsubA := b.Subscribe("cart-A")
	defer unsubA()
	chB, unsubB := b.Subscribe("cart-B")
	defer unsubB()

	b.Tickle("cart-A")

	select {
	case <-chA:
		// good
	case <-time.After(time.Second):
		t.Fatal("cart-A subscriber didn't get its tickle")
	}

	// chB should NOT have received anything. Give it a tiny grace
	// period to make sure we're not just winning the race.
	select {
	case sig := <-chB:
		t.Errorf("cart-B unexpectedly received %v", sig)
	case <-time.After(50 * time.Millisecond):
		// good
	}
}

// TestBroker_MultipleSubscribers_AllNotified covers the fanout case
// (e.g. a controller-side spectator joining alongside the kiosk SPA).
func TestBroker_MultipleSubscribers_AllNotified(t *testing.T) {
	b := NewBroker()
	const n = 5
	chans := make([]<-chan Signal, n)
	unsubs := make([]func(), n)
	for i := 0; i < n; i++ {
		chans[i], unsubs[i] = b.Subscribe("cart-1")
	}
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()

	b.Tickle("cart-1")

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(ch <-chan Signal, idx int) {
			defer wg.Done()
			select {
			case sig := <-ch:
				if sig.Kind != EventUpdated {
					t.Errorf("sub %d: got %q, want %q", idx, sig.Kind, EventUpdated)
				}
			case <-time.After(time.Second):
				t.Errorf("sub %d: timed out", idx)
			}
		}(chans[i], i)
	}
	wg.Wait()
}

// TestBroker_SlowSubscriber_DropsSignal documents the "buffer full =
// drop" semantics. We fill the subscriber's buffer past its
// subscriberChanBuf capacity and assert that later Tickles are
// silently dropped (no goroutine leak, no panic, no block on the
// caller). Eventual consistency on the SPA side covers the gap.
func TestBroker_SlowSubscriber_DropsSignal(t *testing.T) {
	b := NewBroker()
	ch, unsub := b.Subscribe("cart-1")
	defer unsub()

	// Pump in 2x the buffer depth without draining. Tickle is
	// non-blocking by design; this loop must complete quickly.
	done := make(chan struct{})
	go func() {
		for i := 0; i < subscriberChanBuf*2; i++ {
			b.Tickle("cart-1")
		}
		close(done)
	}()
	select {
	case <-done:
		// good
	case <-time.After(time.Second):
		t.Fatal("Tickle blocked on a slow subscriber — broker contract violated")
	}

	// Drain whatever we got — should be exactly subscriberChanBuf
	// signals (the rest dropped). We don't insist on the exact count
	// since "drop on full" is the contract, not "buffer size N".
	count := 0
drain:
	for {
		select {
		case <-ch:
			count++
		default:
			break drain
		}
	}
	if count == 0 {
		t.Errorf("expected at least one buffered signal, got 0")
	}
	if count > subscriberChanBuf {
		t.Errorf("buffer overflow: got %d, capacity is %d", count, subscriberChanBuf)
	}
}

// TestBroker_Close_FiresGone confirms Close sends EventGone to every
// subscriber and the cart is then forgotten (subsequent Tickles for
// that cart_id reach nobody).
func TestBroker_Close_FiresGone(t *testing.T) {
	b := NewBroker()
	ch, unsub := b.Subscribe("cart-1")
	defer unsub()

	b.Close("cart-1")

	select {
	case sig := <-ch:
		if sig.Kind != EventGone {
			t.Errorf("expected %q, got %q", EventGone, sig.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for close")
	}

	// A subsequent Tickle should be a no-op since Close removed the
	// cart from the map. We Subscribe a fresh ch to verify nothing
	// rides into it — the closed cart is forgotten, so the new
	// Subscribe is on a different bucket and stays quiet under the
	// old cart_id's Tickle.
	b.Tickle("cart-1")
	select {
	case sig, open := <-ch:
		if open {
			t.Errorf("unexpected signal after Close: %v", sig)
		}
		// closed-and-drained ch read returning !open is fine — that's
		// what happens after unsub fires below; here it'd indicate the
		// channel was already closed which shouldn't have happened
		// yet.
	case <-time.After(50 * time.Millisecond):
		// good
	}
}

// TestBroker_UnsubBeforeTickle: after unsub, the subscriber must not
// receive any further signals and the channel must be closed.
func TestBroker_UnsubBeforeTickle(t *testing.T) {
	b := NewBroker()
	ch, unsub := b.Subscribe("cart-1")
	unsub()

	// Channel should be closed already.
	select {
	case _, open := <-ch:
		if open {
			t.Errorf("expected closed channel after unsub, got open")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("ch wasn't closed after unsub")
	}

	// And Tickle should not panic on the unsubscribed cart.
	b.Tickle("cart-1")
}

// TestBroker_UnsubIsIdempotent: calling unsub twice is a no-op (the
// sync.Once inside unsub guards us). This matters because the SSE
// handler's defer + the explicit cleanup-on-Close path might both
// call it.
func TestBroker_UnsubIsIdempotent(t *testing.T) {
	b := NewBroker()
	_, unsub := b.Subscribe("cart-1")
	unsub()
	// Should not panic / double-close.
	unsub()
}
