import { api } from '../lib/api'
import { useSessionStore } from '../stores/session'
import type {
  Cart,
  CartAction,
  CartLine,
  CommitResult,
  RFIDScanResult,
  ScanResult,
} from '../types'

// useCart wraps the /api/kiosk/cart/* endpoints. It writes results into the
// session store so any component (CheckoutView, CartTable, etc.) reacts.
export function useCart() {
  const session = useSessionStore()

  async function scanDispatch(value: string): Promise<ScanResult> {
    return api.post<ScanResult>('/api/kiosk/scan', { value })
  }

  async function start(userCode: string): Promise<Cart> {
    const { cart } = await api.post<{ cart: Cart }>('/api/kiosk/cart/start', { user_code: userCode })
    session.setCart(cart)
    return cart
  }

  async function addItem(itemCode: string): Promise<CartLine> {
    if (!session.cart) throw new Error('no active cart')
    const { cart, line } = await api.post<{ cart: Cart; line: CartLine }>(
      '/api/kiosk/cart/add',
      { cart_id: session.cart.id, item_code: itemCode },
    )
    session.setCart(cart)
    return line
  }

  async function updateLine(lineId: string, patch: { qty?: number; action?: CartAction }) {
    const { cart } = await api.patch<{ cart: Cart; line: CartLine }>(
      `/api/kiosk/cart/lines/${lineId}`,
      patch,
    )
    session.setCart(cart)
  }

  async function deleteLine(lineId: string) {
    const { cart } = await api.delete<{ cart: Cart }>(`/api/kiosk/cart/lines/${lineId}`)
    session.setCart(cart)
  }

  async function cancel() {
    if (!session.cart) return
    await api.post('/api/kiosk/cart/cancel', { cart_id: session.cart.id })
    session.setCart(null)
  }

  async function commit(): Promise<CommitResult> {
    if (!session.cart) throw new Error('no active cart')
    const result = await api.post<CommitResult>('/api/kiosk/cart/commit', {
      cart_id: session.cart.id,
    })
    session.setCart(null)
    return result
  }

  // rfidScan triggers one LLRP inventory cycle on the kiosk's reader
  // (counter_scan mode) and folds matched tags into the active cart.
  // The server runs the read for its configured window and returns the
  // resulting cart plus added_lines + observed_epcs + unresolved_epcs;
  // the caller drives any "Reading…" UI on its own clock since this
  // promise doesn't resolve until the read window completes server-side.
  async function rfidScan(): Promise<RFIDScanResult> {
    if (!session.cart) throw new Error('no active cart')
    const result = await api.post<RFIDScanResult>(
      `/api/kiosk/cart/rfid-scan?cart_id=${encodeURIComponent(session.cart.id)}`,
      {},
    )
    if (result.cart) session.setCart(result.cart)
    return result
  }

  return { scanDispatch, start, addItem, updateLine, deleteLine, cancel, commit, rfidScan }
}
