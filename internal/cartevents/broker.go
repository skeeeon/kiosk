// Package cartevents brokers "this cart changed" notifications between
// the cart write paths (Add / Update / Delete / ForemanReturn /
// RFIDScan) and any SSE subscribers watching a cart. It is
// deliberately tiny: tickle + close are the only signals, payloads are
// empty, the SPA refetches the cart via GET when a tickle arrives.
// "Push the signal, pull the data." See docs/rfid.md, Phase 3.
//
// Cart state itself stays in-memory in internal/cart.Store. This
// package owns the wiring around that store, not the store itself, so
// the in-memory invariant documented in CLAUDE.md is preserved.
package cartevents

import "sync"

// Event names match the SSE event types sent on the wire.
const (
	EventUpdated = "cart.updated" // ordinary tickle: write happened, refetch
	EventGone    = "cart.gone"    // terminal: cart committed/cancelled, close the stream
)

// Signal is what subscribers receive on their channel. The kind is
// either EventUpdated or EventGone; carrying it on the channel rather
// than as a separate "done" channel keeps the SSE handler's select
// loop linear.
type Signal struct {
	Kind string
}

// subscriberChanBuf is the per-subscriber buffer depth. Four is plenty
// for the kiosk's actual cadence (worker-driven, sub-second-burst at
// worst), and the dropped-on-full semantics mean a slow client falls
// behind one notification rather than stalling the broker. The next
// tickle catches them up.
const subscriberChanBuf = 4

// Broker tracks subscribers per cart_id. Subscribe returns a channel
// and an unsubscribe func; callers (the SSE handler) drain the channel
// in their own goroutine and call unsub on disconnect. Tickle fires
// EventUpdated to every current subscriber for a cart_id; Close fires
// EventGone and forgets the cart so subsequent Tickles are no-ops.
//
// Zero value is usable. Methods are safe for concurrent use.
type Broker struct {
	mu   sync.Mutex
	subs map[string][]chan Signal
}

// NewBroker returns a ready-to-use Broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[string][]chan Signal)}
}

// Subscribe registers a new subscriber for cartID and returns the
// channel it will receive on plus an idempotent unsubscribe func. The
// unsubscribe func is also safe to call after Close has fired — it's a
// no-op in that case.
func (b *Broker) Subscribe(cartID string) (<-chan Signal, func()) {
	ch := make(chan Signal, subscriberChanBuf)

	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[string][]chan Signal)
	}
	b.subs[cartID] = append(b.subs[cartID], ch)
	b.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			list := b.subs[cartID]
			for i, c := range list {
				if c == ch {
					// Remove without preserving order — order doesn't
					// matter and swap-delete avoids a slice copy.
					list[i] = list[len(list)-1]
					b.subs[cartID] = list[:len(list)-1]
					break
				}
			}
			if len(b.subs[cartID]) == 0 {
				delete(b.subs, cartID)
			}
			close(ch)
		})
	}
	return ch, unsub
}

// Tickle sends EventUpdated to every subscriber of cartID. Sends are
// non-blocking: if a subscriber's buffer is full, the signal is
// dropped on the floor. This is intentional — the SPA refetches on
// each signal, and "fell behind one tickle" converges to "current
// state" on the next tickle. We never want a slow client to back up a
// hot cart write path.
func (b *Broker) Tickle(cartID string) {
	b.mu.Lock()
	list := b.subs[cartID]
	// Take a snapshot under the lock so we can do the (non-blocking)
	// sends without holding it. Subscribe/Unsubscribe stay snappy.
	snapshot := make([]chan Signal, len(list))
	copy(snapshot, list)
	b.mu.Unlock()

	for _, ch := range snapshot {
		select {
		case ch <- Signal{Kind: EventUpdated}:
		default:
			// dropped — see comment above
		}
	}
}

// Close fires EventGone to every subscriber of cartID and removes the
// cart from the broker. Subscribers see one final Signal and then
// their channel closes (via the unsub triggered by the SSE handler
// after it forwards EventGone to the client). Calling Close on an
// already-closed or never-subscribed cart is a no-op.
//
// We don't close the subscriber channels here — that's the SSE
// handler's job, via the unsub it already holds. If we closed the
// channels here too, a racing unsub would panic trying to close
// twice. Letting the SSE handler do the close-on-the-way-out keeps
// ownership clean: Subscribe creates the channel, unsub closes it.
func (b *Broker) Close(cartID string) {
	b.mu.Lock()
	list := b.subs[cartID]
	delete(b.subs, cartID)
	b.mu.Unlock()

	for _, ch := range list {
		select {
		case ch <- Signal{Kind: EventGone}:
		default:
			// dropped — same rationale as Tickle. The SSE handler
			// will also see its ctx cancel or the channel close
			// shortly anyway since the broker no longer holds the
			// cart.
		}
	}
}
