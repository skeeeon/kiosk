import { onScopeDispose, watch, type Ref } from 'vue'

// useCartEvents wires CheckoutView to /api/kiosk/cart/events. The
// server is the source of truth for "this cart changed" — we don't
// poll, we don't try to be clever about which write happened, we just
// refetch via GET cart on every tickle. See docs/rfid.md, Phase 3.
//
// EventSource auto-reconnects on *network-level* drops (the readyState
// goes to CONNECTING and the browser retries on its own). It does NOT
// reconnect when the server returns a non-2xx HTTP status — the browser
// sets readyState to CLOSED and gives up permanently. That happens for a
// transient 503 during a restart/proxy hiccup, or a 404 if the server was
// restarted and no longer knows this cart_id. Left unhandled, the cart then
// silently freezes for the rest of the session (server-driven RFID / foreman
// writes never arrive). So we watch for the CLOSED state and reconnect
// ourselves with capped exponential backoff, refetching on recovery since
// the stream has no replay. `cart.gone` is the one terminal case where we
// stop for good.
//
// Usage from CheckoutView:
//
//   useCartEvents(cartId, { onUpdated: refetchCart, onGone: clearCart })
//
// cartId is a Ref so the composable can subscribe / unsubscribe in
// response to cart lifecycle changes (badge in → subscribe; commit /
// cancel → unsubscribe). Passing null/empty closes any active
// subscription without opening a new one.
const RECONNECT_BASE_MS = 1500
const RECONNECT_MAX_MS = 30000

export function useCartEvents(
  cartId: Ref<string | null | undefined>,
  handlers: { onUpdated: () => void; onGone?: () => void },
) {
  let source: EventSource | null = null
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null
  let reconnectAttempts = 0

  function clearReconnect() {
    if (reconnectTimer) {
      clearTimeout(reconnectTimer)
      reconnectTimer = null
    }
  }

  function close() {
    clearReconnect()
    if (source) {
      source.close()
      source = null
    }
  }

  function open(id: string) {
    close()
    reconnectAttempts = 0 // fresh subscription — reset backoff
    openInternal(id)
  }

  function openInternal(id: string) {
    // EventSource doesn't accept request bodies or custom headers, so
    // cart_id rides as a query parameter — same shape as the
    // /cart/foreman-return/options endpoint and the GET endpoint we
    // refetch through.
    const url = `/api/kiosk/cart/events?cart_id=${encodeURIComponent(id)}`
    source = new EventSource(url)
    source.onopen = () => {
      if (reconnectAttempts > 0) {
        // Recovered from a disconnect — refetch once to catch any writes
        // that landed while the stream was down (SSE has no replay).
        handlers.onUpdated()
      }
      reconnectAttempts = 0
    }
    source.addEventListener('cart.updated', () => {
      handlers.onUpdated()
    })
    source.addEventListener('cart.gone', () => {
      // Stop trying to reconnect once the server has told us the cart
      // is committed/cancelled. Without explicit close() the browser
      // would interpret the connection close as a transient failure
      // and reopen → 404 loop.
      close()
      handlers.onGone?.()
    })
    source.onerror = () => {
      // readyState CONNECTING means the browser is already retrying a
      // transient network drop — leave it. CLOSED means it gave up (a
      // non-2xx response); recover ourselves with capped backoff so the
      // cart doesn't freeze. We keep retrying indefinitely (capped delay)
      // rather than giving up — a kiosk runs unattended for days and the
      // server may come back at any point.
      if (!source || source.readyState !== EventSource.CLOSED) return
      source.close()
      source = null
      const delay = Math.min(RECONNECT_MAX_MS, RECONNECT_BASE_MS * 2 ** reconnectAttempts)
      reconnectAttempts++
      clearReconnect()
      reconnectTimer = setTimeout(() => {
        // Only reconnect if this is still the active cart.
        if (cartId.value === id) openInternal(id)
      }, delay)
    }
  }

  // React to cart_id changes. We use { immediate: true } so an active
  // cart at mount time subscribes immediately rather than waiting for
  // the next assignment.
  watch(
    cartId,
    (id) => {
      if (id) {
        open(id)
      } else {
        close()
      }
    },
    { immediate: true },
  )

  // Tear down on unmount.  onScopeDispose fires for component
  // unmount and for explicit setup scopes; either way our EventSource
  // must close so the server-side defer unsub runs.
  onScopeDispose(() => {
    close()
  })

  return { close }
}
