import { onScopeDispose, watch, type Ref } from 'vue'

// useCartEvents wires CheckoutView to /api/kiosk/cart/events. The
// server is the source of truth for "this cart changed" — we don't
// poll, we don't try to be clever about which write happened, we just
// refetch via GET cart on every tickle. See docs/rfid.md, Phase 3.
//
// EventSource auto-reconnects on transient failures (network blips,
// proxy idle timeouts after our 15s heartbeats). The only thing we
// have to handle ourselves is "cart is gone" — when the server sends
// a `cart.gone` event we close the source explicitly so the browser
// doesn't keep reconnecting against a 404.
//
// Usage from CheckoutView:
//
//   useCartEvents(cartId, { onUpdated: refetchCart, onGone: clearCart })
//
// cartId is a Ref so the composable can subscribe / unsubscribe in
// response to cart lifecycle changes (badge in → subscribe; commit /
// cancel → unsubscribe). Passing null/empty closes any active
// subscription without opening a new one.
export function useCartEvents(
  cartId: Ref<string | null | undefined>,
  handlers: { onUpdated: () => void; onGone?: () => void },
) {
  let source: EventSource | null = null

  function close() {
    if (source) {
      source.close()
      source = null
    }
  }

  function open(id: string) {
    close()
    // EventSource doesn't accept request bodies or custom headers, so
    // cart_id rides as a query parameter — same shape as the
    // /cart/foreman-return/options endpoint and the GET endpoint we
    // refetch through.
    const url = `/api/kiosk/cart/events?cart_id=${encodeURIComponent(id)}`
    source = new EventSource(url)
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
    // onerror fires on any disconnect; the browser's default behavior
    // is to auto-reconnect, which is what we want for transient
    // network problems. We deliberately don't add a handler — silent
    // reconnect is the right UX. cart.gone covers the terminal case.
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
