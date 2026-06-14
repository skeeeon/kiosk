<script setup lang="ts">
import { computed, nextTick, onScopeDispose, ref } from 'vue'
import { useRoute } from 'vue-router'
import { storeToRefs } from 'pinia'
import ScanInput from '../components/ScanInput.vue'
import CartTable from '../components/CartTable.vue'
import ItemBrowseDialog from '../components/ItemBrowseDialog.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import ForemanReturnDialog from '../components/ForemanReturnDialog.vue'
import IdentifyPanel from '../components/IdentifyPanel.vue'
import TimeclockPanel from '../components/TimeclockPanel.vue'
import { useCart } from '../composables/useCart'
import { useCartEvents } from '../composables/useCartEvents'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import { useSessionStore } from '../stores/session'
import { useToast } from '../composables/useToast'
import { api, ApiError } from '../lib/api'
import type {
  Cart,
  CartAction,
  CartLine,
  CommitResult,
  InstanceMatch,
  Item,
  OpenCheckoutDetail,
  User,
} from '../types'

const session = useSessionStore()
const { cart } = storeToRefs(session)
const toast = useToast()
// All kiosk-side toasts use the top-center position to stay visible across a
// wide touchscreen; admin views default to bottom-right.
const TOP = { position: 'top' } as const
const c = useCart()
const { identity } = useKioskIdentity()

// Optional per-terminal attribution: each physical screen/door is configured
// with a ?door= URL param. Read once (the kiosk never mutates it) and passed
// through to commit so the transaction records where it was finished. Absent
// on single-kiosk installs, which is fully supported (door_id is optional).
const route = useRoute()
const doorId = computed(() => {
  const d = route.query.door
  return (Array.isArray(d) ? d[0] : d) || null
})

const splashLogoBroken = ref(false)
const splashLogoUrl = computed(() =>
  !splashLogoBroken.value && identity.value?.branding?.logo_url
    ? identity.value.branding.logo_url
    : null,
)
const splashTagline = computed(() => identity.value?.branding?.tagline ?? '')

// Receipt auto-dismisses so the kiosk is ready for the next worker. Long
// enough to actually read a multi-line receipt; the explicit "Done" button
// lets impatient users skip the wait.
const SUCCESS_SCREEN_MS = 8000

interface Receipt {
  result: CommitResult
  lines: CartLine[]
  userName: string
}

const success = ref<Receipt | null>(null)
const committing = ref(false)
const browseOpen = ref(false)
const browsePending = ref(false)
const foremanReturnOpen = ref(false)
const crossUserConfirmOpen = ref(false)
const cancelConfirmOpen = ref(false)

// Timeclock splash mode: while open, badge scans route into the panel
// (clock in/out) instead of starting a cart. Gated on the identity flag.
// timeclockOnly goes further: the panel IS the splash (dedicated punch
// station — no carts, no checkout); the epoch key remounts it on close so
// every reset starts from a fresh waiting state.
const timeclockButtonVisible = computed(() => !!identity.value?.timeclock_enabled)
const timeclockOnly = computed(() => !!identity.value?.timeclock_only)
const timeclockOpen = ref(false)
const timeclockUserCode = ref<string | null>(null)
const timeclockEpoch = ref(0)
function closeTimeclock() {
  timeclockOpen.value = false
  timeclockUserCode.value = null
  timeclockEpoch.value++
}
// Routes a badge scan into the panel. The null-then-set dance forces the
// panel's userCode watcher to fire even when the same worker rescans —
// a rescan should refresh their status, not be silently ignored.
async function routeBadgeToTimeclock(code: string) {
  if (timeclockUserCode.value === code) {
    timeclockUserCode.value = null
    await nextTick()
  }
  timeclockUserCode.value = code
}
// Golden path: commit returned 409 not_clocked_in → offer a one-tap
// clock-in + retry instead of bouncing the worker to the splash button.
const clockInPromptOpen = ref(false)

// Receipt countdown: a normalized 0..1 ref that drives the progress bar at
// the top of the success view. Each tick recomputes from the dismiss
// deadline; the deadline gets pushed forward when the worker interacts
// with the receipt (scroll, click), so reading the receipt doesn't trip
// the auto-dismiss out from under them.
const receiptProgress = ref(1)
let receiptDismissAt = 0
let receiptTickHandle: ReturnType<typeof setInterval> | null = null
function startReceiptCountdown() {
  receiptDismissAt = Date.now() + SUCCESS_SCREEN_MS
  receiptProgress.value = 1
  if (receiptTickHandle) clearInterval(receiptTickHandle)
  receiptTickHandle = setInterval(() => {
    const remaining = receiptDismissAt - Date.now()
    if (remaining <= 0) {
      dismissReceipt()
      return
    }
    receiptProgress.value = remaining / SUCCESS_SCREEN_MS
  }, 100)
}
function extendReceiptCountdown() {
  if (!success.value) return
  receiptDismissAt = Date.now() + SUCCESS_SCREEN_MS
  receiptProgress.value = 1
}

// identify holds the result of an item scan that landed before a badge —
// the splash promises "scan an item code to identify it" and this drives
// the panel that delivers on that. Cleared on next badge / item-with-cart
// scan or via the panel's Dismiss button. Auto-dismisses so a curious
// peek doesn't lock the splash forever.
const identify = ref<{ item: Item | null; instance: InstanceMatch | null } | null>(null)
const IDENTIFY_TIMEOUT_MS = 15000
let identifyDismissHandle: ReturnType<typeof setTimeout> | null = null

// outstanding holds the worker's currently-checked-out items, captured from
// the /api/kiosk/scan response when they badge in. Cleared on cart end (the
// next scan re-fetches via the scan endpoint). Read-only — workers see it
// as a glance, they don't act on rows from here.
const outstanding = ref<OpenCheckoutDetail[]>([])
const outstandingExpanded = ref(false)

// relativeAge renders a short "Nd / Nh / Nm" string for the "What you have
// out" panel. We avoid Intl.RelativeTimeFormat because the panel only needs
// a small, deterministic surface and the kiosk's Vite build doesn't ship
// any other relative-time helpers.
function relativeAge(iso: string): string {
  const t = new Date(iso).getTime()
  if (!Number.isFinite(t)) return ''
  const diffMs = Date.now() - t
  if (diffMs < 60_000) return 'just now'
  const minutes = Math.floor(diffMs / 60_000)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}
function showIdentify(payload: { item: Item | null; instance: InstanceMatch | null }) {
  identify.value = payload
  if (identifyDismissHandle) clearTimeout(identifyDismissHandle)
  identifyDismissHandle = setTimeout(() => { identify.value = null }, IDENTIFY_TIMEOUT_MS)
}
function dismissIdentify() {
  if (identifyDismissHandle) {
    clearTimeout(identifyDismissHandle)
    identifyDismissHandle = null
  }
  identify.value = null
}

// Ref to the scrollable cart-lines container so we can snap to the latest
// addition. Called explicitly from the add handlers (scan + browse) rather
// than via a length watcher — that way qty +/- changes and removals don't
// trigger an unwanted scroll.
const cartScroller = ref<HTMLElement | null>(null)
async function scrollCartToBottom() {
  await nextTick()
  const el = cartScroller.value
  if (el) {
    el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' })
  }
}

// True when the API error means "your cart is gone" (idle timeout or process
// restart). Surfaces a friendly toast and resets local state so the next scan
// starts fresh.
function isCartExpiredError(e: unknown): boolean {
  if (!(e instanceof ApiError)) return false
  if (e.status !== 404) return false
  const m = e.message.toLowerCase()
  return m.includes('cart not found') || m.includes('cart expired')
}

function handleApiError(e: unknown, fallbackPrefix?: string) {
  if (isCartExpiredError(e)) {
    session.setCart(null)
    toast.warn('Your session has expired. Scan your badge to begin again.', {
      position: 'top',
      duration: 6000,
    })
    return
  }
  const msg = (e as Error).message
  toast.error(fallbackPrefix ? `${fallbackPrefix}: ${msg}` : msg, TOP)
}

// Cart lines created via the explicit foreman-return endpoint carry the
// target worker's id. The commit confirm uses this to remind the foreman
// what they're about to close on someone else's behalf.
const crossUserLines = computed<CartLine[]>(() => {
  if (!cart.value) return []
  return cart.value.lines.filter((l) => !!l.original_checkout_user_id)
})

const isForeman = computed(() => cart.value?.user_role === 'foreman')

// SSE subscription: while a cart is active we listen for server-side
// tickles (other writers, RFID reads from inside the same kiosk in
// future multi-window scenarios, Phase 4's NATS-driven cart.start /
// read.trigger commands in enclosure_diff). On every tickle we
// refetch via GET cart so the store always reflects the latest
// canonical state — "push the signal, pull the data."
const activeCartId = computed(() => cart.value?.id ?? null)
useCartEvents(activeCartId, {
  onUpdated: () => {
    void c.refresh()
  },
  onGone: () => {
    // Server told us the cart is committed/cancelled. The corresponding
    // local flow (onCommit / onCancel) already cleared session.cart, so
    // this is a defensive sync for the case where another writer
    // (a controller-driven admin close in future, an idle timeout we
    // surface later) terminated the cart out from under us.
    session.setCart(null)
  },
})

// Two RFID button modes, mutually exclusive on the cart toolbar:
//   - counter_scan: "RFID scan" — operator-initiated read that folds
//     observed tags into the cart (one tag = one cart line).
//   - enclosure_diff: "Re-read" — operator manually re-triggers the
//     diff path that the NATS-driven read.trigger normally fires
//     when door-occupancy ends. Same backend window, same countdown
//     style, different semantics (diff vs scan).
const rfidScanButtonVisible = computed(
  () => identity.value?.rfid_enabled && identity.value?.rfid_mode === 'counter_scan',
)
const rfidReReadButtonVisible = computed(
  () => identity.value?.rfid_enabled && identity.value?.rfid_mode === 'enclosure_diff',
)

// rfidReadWindowMs is what we draw the "Reading… 3s" countdown over.
// The server's actual read window comes from kiosk.yaml; we mirror the
// docs/rfid.md default here so the UI's countdown matches by default
// without us shipping read_window over the identity payload. The
// button's `disabled` while scanning prevents the operator from
// double-clicking and getting out of sync if a deployment used a
// non-default window — they'll just see the spinner past 3s, which
// is fine.
const RFID_READ_WINDOW_MS = 3000
const rfidScanning = ref(false)
const rfidProgress = ref(0)
let rfidTickHandle: ReturnType<typeof setInterval> | null = null

async function onRFIDScan() {
  if (!cart.value || rfidScanning.value) return
  rfidScanning.value = true
  rfidProgress.value = 0
  const startedAt = Date.now()
  rfidTickHandle = setInterval(() => {
    const elapsed = Date.now() - startedAt
    rfidProgress.value = Math.min(1, elapsed / RFID_READ_WINDOW_MS)
  }, 50)
  try {
    const result = await c.rfidScan()
    const added = result.added_lines.length
    const observed = result.observed_epcs.length
    const unresolved = result.unresolved_epcs.length
    if (added > 0) {
      toast.success(
        `Added ${added} ${added === 1 ? 'item' : 'items'} from ${observed} observed`,
        TOP,
      )
    } else if (observed > 0) {
      // The reader saw something but nothing landed — typically
      // duplicates already in the cart or unresolved tags.
      const detail =
        unresolved > 0
          ? `${unresolved} unknown ${unresolved === 1 ? 'tag' : 'tags'}`
          : 'all tags already in cart'
      toast.warn(`Observed ${observed} ${observed === 1 ? 'tag' : 'tags'} — ${detail}`, TOP)
    } else {
      toast.warn('No tags observed — check antenna placement and try again', TOP)
    }
  } catch (e) {
    handleApiError(e, 'RFID scan')
  } finally {
    if (rfidTickHandle) {
      clearInterval(rfidTickHandle)
      rfidTickHandle = null
    }
    rfidScanning.value = false
    rfidProgress.value = 0
  }
}

// onReReadEnclosure is the enclosure_diff variant. Same countdown
// state (only one read can be in flight at a time per kiosk), but
// the toast summarizes diff outcomes rather than per-tag adds —
// checkouts/returns derived from the observed vs expected state,
// plus the skipped-cross-user count if any tags belonged to another
// worker.
async function onReReadEnclosure() {
  if (!cart.value || rfidScanning.value) return
  rfidScanning.value = true
  rfidProgress.value = 0
  const startedAt = Date.now()
  rfidTickHandle = setInterval(() => {
    const elapsed = Date.now() - startedAt
    rfidProgress.value = Math.min(1, elapsed / RFID_READ_WINDOW_MS)
  }, 50)
  try {
    const result = await c.readTrigger()
    const added = result.added_lines.length
    const observed = result.observed_epcs.length
    const skipped = result.skipped_cross_user_count
    if (added > 0) {
      let msg = `Diff: ${added} cart line${added === 1 ? '' : 's'} from ${observed} observed`
      if (skipped > 0) {
        msg += ` (${skipped} skipped — held by another worker)`
      }
      toast.success(msg, TOP)
    } else if (observed > 0) {
      const skippedSuffix = skipped > 0 ? ` (${skipped} held by another worker)` : ''
      toast.warn(`Observed ${observed} tag${observed === 1 ? '' : 's'} — no diff changes${skippedSuffix}`, TOP)
    } else {
      toast.warn('No tags observed in the enclosure', TOP)
    }
  } catch (e) {
    handleApiError(e, 'Re-read')
  } finally {
    if (rfidTickHandle) {
      clearInterval(rfidTickHandle)
      rfidTickHandle = null
    }
    rfidScanning.value = false
    rfidProgress.value = 0
  }
}

const ACTION_LABEL: Record<CartAction, string> = {
  checkout: 'Checked out',
  return: 'Returned',
  consume: 'Consumed',
}
const ACTION_TONE: Record<CartAction, string> = {
  checkout: 'text-emerald-400',
  return: 'text-amber-400',
  consume: 'text-sky-400',
}

function dismissReceipt() {
  if (receiptTickHandle) {
    clearInterval(receiptTickHandle)
    receiptTickHandle = null
  }
  success.value = null
}

// Clear any live interval/timeout on unmount (e.g. operator navigates to
// /admin/login mid-countdown). Vue tolerates writes to orphaned refs, so this
// is hygiene rather than a crash fix — but it stops stray timers firing
// against a torn-down component, matching the cleanup the admin views do.
onScopeDispose(() => {
  if (receiptTickHandle) clearInterval(receiptTickHandle)
  if (rfidTickHandle) clearInterval(rfidTickHandle)
  if (identifyDismissHandle) clearTimeout(identifyDismissHandle)
})

// addChain serializes item-add round-trips. A USB HID scanner can emit two
// barcodes faster than one /cart/add completes; without this, overlapping
// addItem calls can resolve out of order and leave the store reflecting an
// earlier server snapshot (a line appears to "not register" until the next
// SSE tickle). Chaining applies adds in scan order, one at a time.
let addChain: Promise<void> = Promise.resolve()

async function onScan(raw: string) {
  // A scan during the success receipt means the next worker (or the same one)
  // is moving on. Dismiss it first: otherwise a badge scan would start a new
  // cart that stays hidden behind the previous worker's receipt until the
  // 8s countdown fires — a shared-kiosk session-bleed footgun.
  if (success.value) dismissReceipt()

  let result
  try {
    result = await c.scanDispatch(raw)
  } catch (e) {
    handleApiError(e, 'Scan failed')
    return
  }

  if (result.type === 'unknown') {
    toast.warn(`Unknown code: ${result.value ?? raw}`, TOP)
    return
  }

  if (result.type === 'user') {
    const u = result.record as User
    // Timeclock mode owns badge scans while the panel is open (splash only —
    // the panel isn't rendered once a cart exists). On a timeclock-only
    // kiosk every badge scan is a punch flow — there is no cart to start.
    if (timeclockOnly.value || (!cart.value && timeclockOpen.value)) {
      await routeBadgeToTimeclock(u.code)
      return
    }
    if (cart.value && cart.value.user_id !== u.id) {
      toast.warn(`${cart.value.user_name} is still active. Cancel or commit first.`, TOP)
      return
    }
    try {
      await c.start(u.code)
      dismissIdentify()
      outstanding.value = u.open_checkouts ?? []
      outstandingExpanded.value = false
      const welcome = u.open_count > 0
        ? `Welcome, ${u.name} — ${u.open_count} item${u.open_count === 1 ? '' : 's'} out`
        : `Welcome, ${u.name}`
      toast.info(welcome, TOP)
    } catch (e) {
      handleApiError(e)
    }
    return
  }

  if (result.type === 'item' || result.type === 'item_instance') {
    if (timeclockOnly.value) {
      toast.warn('This kiosk is a time clock — item checkout is not available here.', TOP)
      return
    }
    if (!cart.value && timeclockOpen.value) {
      toast.warn('Time clock is open — close it to scan items.', TOP)
      return
    }
    if (!cart.value) {
      // Splash identify: render the scanned item's info instead of nagging
      // for a badge. The promise on the splash is "scan an item code to
      // identify it"; this is where that lands.
      if (result.type === 'item_instance') {
        showIdentify({ item: null, instance: result.record as InstanceMatch })
      } else {
        showIdentify({ item: result.record as Item, instance: null })
      }
      return
    }
    // For an instance scan, we pass the instance's own code — the backend
    // resolves instances before items (same precedence as /scan), so this
    // ends up on the correct cart line with the instance FK populated.
    const code = result.type === 'item_instance'
      ? (result.record as InstanceMatch).instance.code
      : (result.record as Item).code
    // Enqueue rather than await directly so concurrent scans serialize in
    // order. Errors are handled inside the chain so one failed add can't
    // poison the chain for the next scan.
    addChain = addChain.then(async () => {
      try {
        await c.addItem(code)
        await scrollCartToBottom()
      } catch (e) {
        handleApiError(e)
      }
    })
  }
}

async function onUpdate(id: string, patch: { qty?: number; action?: CartAction; request_maintenance?: boolean }) {
  try {
    await c.updateLine(id, patch)
  } catch (e) {
    handleApiError(e)
  }
}

async function onRemove(id: string) {
  try {
    await c.deleteLine(id)
  } catch (e) {
    handleApiError(e)
  }
}

async function onCancel() {
  // Empty carts cancel without a prompt — there's nothing to lose. Non-empty
  // carts surface the line count so a fat-finger doesn't discard work.
  if (cart.value && cart.value.lines.length > 0) {
    cancelConfirmOpen.value = true
    return
  }
  await doCancel()
}

async function doCancel() {
  try {
    await c.cancel()
    outstanding.value = []
    outstandingExpanded.value = false
    toast.info('Session ended', TOP)
  } catch (e) {
    handleApiError(e)
  }
}

function onConfirmCancel() {
  cancelConfirmOpen.value = false
  void doCancel()
}

async function onBrowsePick(code: string) {
  if (!cart.value || browsePending.value) return
  browsePending.value = true
  try {
    const line = await c.addItem(code)
    toast.info(`Added ${line.item_name}`, TOP)
    await scrollCartToBottom()
  } catch (e) {
    handleApiError(e)
  } finally {
    browsePending.value = false
  }
}

function onForemanReturnAdded(payload: { cart: Cart; line: CartLine }) {
  session.setCart(payload.cart)
  toast.warn(
    `Foreman return queued: ${payload.line.original_checkout_user_name ?? 'worker'} · ${payload.line.item_name}`,
    TOP,
  )
  void scrollCartToBottom()
}

function onCommit() {
  if (committing.value || !cart.value) return
  if (crossUserLines.value.length > 0) {
    crossUserConfirmOpen.value = true
    return
  }
  void doCommit()
}

async function doCommit() {
  if (committing.value || !cart.value) return
  committing.value = true
  // Snapshot lines + user before commit — useCart.commit() clears the store
  // on success, so this is the only chance to read them for the receipt.
  const snapshotLines = cart.value.lines.map((l) => ({ ...l }))
  const snapshotUser = cart.value.user_name
  try {
    const result = await c.commit(doorId.value)
    success.value = { result, lines: snapshotLines, userName: snapshotUser }
    outstanding.value = []
    outstandingExpanded.value = false
    startReceiptCountdown()
  } catch (e) {
    // Timeclock interlock: an expected, worker-fixable conflict — offer the
    // one-tap clock-in instead of a red error.
    if (
      e instanceof ApiError &&
      e.status === 409 &&
      (e.data as { error?: string } | null)?.error === 'not_clocked_in'
    ) {
      clockInPromptOpen.value = true
    } else {
      handleApiError(e)
    }
  } finally {
    committing.value = false
  }
}

async function onConfirmClockIn() {
  clockInPromptOpen.value = false
  if (!cart.value) return
  try {
    await api.post('/api/kiosk/timeclock/punch', {
      user_code: cart.value.user_code,
      direction: 'in',
    })
    toast.success('Clocked in', TOP)
    await doCommit()
  } catch (e) {
    handleApiError(e, 'Clock in')
  }
}

function onConfirmCrossUser() {
  crossUserConfirmOpen.value = false
  void doCommit()
}

const crossUserSummary = computed(() =>
  crossUserLines.value
    .map((l) => `${l.item_code} — ${l.original_checkout_user_name ?? 'another worker'}`)
    .join('\n'),
)

</script>

<template>
  <!-- Single flex-col root so RouterView's flex-1 class can apply and the
       inner mains can use flex-1 to fill the viewport area between the
       app-level header and footer. min-h-0 lets the inner cart-lines area
       overflow without pushing the chrome and footer off-screen. -->
  <div class="flex-1 flex flex-col min-h-0">
  <ScanInput @scan="onScan" />

  <!-- Chrome header for the active cart and the post-commit success screen.
       The splash has its own centered logo treatment so we skip the chrome
       header there to avoid showing the logo twice. The user identity chip
       on the right only appears when a cart is active — success already
       prints "Thanks, {name}" prominently. -->
  <header
    v-if="cart || success"
    class="flex items-start justify-between gap-6 px-6 pt-6 pb-2 shrink-0"
  >
    <img
      v-if="splashLogoUrl"
      :src="splashLogoUrl"
      alt="logo"
      class="h-20 md:h-24 w-auto object-contain shrink-0"
      @error="splashLogoBroken = true"
    />
    <span v-else class="text-3xl md:text-5xl font-semibold tracking-wide shrink-0">Kiosk</span>
    <div v-if="cart" class="text-right shrink-0">
      <div class="flex items-center justify-end gap-2">
        <span
          v-if="cart.lines.length > 0"
          class="inline-block px-2 py-0.5 rounded-full bg-slate-800 text-slate-300 text-sm tabular-nums"
        >
          {{ cart.lines.length }} {{ cart.lines.length === 1 ? 'item' : 'items' }}
        </span>
        <p class="text-2xl text-slate-100">{{ cart.user_name }}</p>
      </div>
      <p class="text-sm text-slate-500 font-mono">{{ cart.user_code }}</p>
    </div>
  </header>

  <!-- Receipt scrolls within the viewport area when the transaction has
       more lines than fit. Unlike the cart, the scroll position stays at
       the top so the worker sees "Done / Thanks, {name}" and the first
       few lines first; they can scroll down to read the rest or hit the
       Done button at the bottom. The countdown bar drains over
       SUCCESS_SCREEN_MS; any scroll/click inside the main pushes it back
       to full so a worker reading the receipt doesn't get bumped. -->
  <div
    v-if="success"
    class="h-1 bg-emerald-500 transition-[width] duration-100 ease-linear shrink-0"
    :style="{ width: `${Math.round(receiptProgress * 100)}%` }"
    aria-hidden="true"
  ></div>
  <main
    v-if="success"
    class="flex-1 min-h-0 overflow-y-auto flex flex-col items-center px-6 py-10"
    @scroll.passive="extendReceiptCountdown"
    @click="extendReceiptCountdown"
  >
    <div class="w-full max-w-2xl">
      <div class="text-center mb-6">
        <p class="text-5xl font-bold text-emerald-400 mb-2">Done</p>
        <p class="text-lg text-slate-400">Thanks, {{ success.userName }}</p>
      </div>

      <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
        <ul class="divide-y divide-slate-800">
          <li
            v-for="line in success.lines"
            :key="line.id"
            class="flex items-center gap-4 px-5 py-4"
          >
            <span
              class="text-sm font-medium uppercase tracking-wide w-28 shrink-0"
              :class="ACTION_TONE[line.action]"
            >
              {{ ACTION_LABEL[line.action] }}
            </span>
            <div class="min-w-0 flex-1">
              <p class="text-lg font-medium text-slate-100 truncate">{{ line.item_name }}</p>
              <p class="text-xs text-slate-500 truncate">
                {{ line.item_code }}<span v-if="line.serial"> · SN {{ line.serial }}</span>
              </p>
            </div>
            <span
              v-if="line.tracking_mode !== 'serialized'"
              class="text-2xl font-semibold tabular-nums text-slate-200 shrink-0"
            >×{{ line.qty }}</span>
          </li>
        </ul>
        <div class="flex justify-between items-center px-5 py-3 bg-slate-950/40 text-sm text-slate-400">
          <span>
            {{ success.result.lines_count }} line<span v-if="success.result.lines_count !== 1">s</span>
          </span>
          <div class="flex gap-4">
            <span v-if="success.result.checked_out > 0">
              <span class="text-emerald-400 font-semibold">{{ success.result.checked_out }}</span> out
            </span>
            <span v-if="success.result.returned > 0">
              <span class="text-amber-400 font-semibold">{{ success.result.returned }}</span> back
            </span>
            <span v-if="success.result.consumed > 0">
              <span class="text-sky-400 font-semibold">{{ success.result.consumed }}</span> consumed
            </span>
          </div>
        </div>
      </div>

      <p class="text-center text-xs text-slate-600 font-mono mt-4 select-all">
        Tx {{ success.result.transaction_id }}
      </p>

      <div class="flex justify-center mt-6">
        <button
          type="button"
          class="px-8 py-3 rounded-xl bg-slate-800 hover:bg-slate-700 text-slate-200 text-base transition-transform active:scale-95"
          @click="dismissReceipt"
        >
          Done
        </button>
      </div>
    </div>
  </main>

  <main v-else-if="!cart" class="flex-1 flex flex-col items-center justify-center px-8 py-16 text-center gap-10">
    <!-- Dedicated punch station: the panel is the whole splash. Keyed on
         the epoch so closeTimeclock remounts it back to a fresh waiting
         state instead of unmounting it. -->
    <template v-if="timeclockOnly">
      <div v-if="splashLogoUrl || splashTagline" class="flex flex-col items-center gap-4">
        <img
          v-if="splashLogoUrl"
          :src="splashLogoUrl"
          alt="logo"
          class="h-24 md:h-32 w-auto object-contain"
          @error="splashLogoBroken = true"
        />
        <p v-if="splashTagline" class="text-xl text-slate-400 max-w-2xl">
          {{ splashTagline }}
        </p>
      </div>
      <TimeclockPanel
        :key="timeclockEpoch"
        standalone
        :user-code="timeclockUserCode"
        @close="closeTimeclock"
      />
    </template>
    <IdentifyPanel
      v-else-if="identify"
      :item="identify.item"
      :instance="identify.instance"
      @dismiss="dismissIdentify"
    />
    <TimeclockPanel
      v-else-if="timeclockOpen"
      :user-code="timeclockUserCode"
      @close="closeTimeclock"
    />
    <template v-else>
      <div v-if="splashLogoUrl || splashTagline" class="flex flex-col items-center gap-4">
        <img
          v-if="splashLogoUrl"
          :src="splashLogoUrl"
          alt="logo"
          class="h-24 md:h-32 w-auto object-contain"
          @error="splashLogoBroken = true"
        />
        <p v-if="splashTagline" class="text-xl text-slate-400 max-w-2xl">
          {{ splashTagline }}
        </p>
      </div>
      <div class="max-w-2xl">
        <p class="text-5xl font-bold tracking-tight mb-4">Scan your badge to begin</p>
        <p class="text-xl text-slate-400">Or scan an item code to identify it.</p>
      </div>
      <!-- Front and center on purpose: checkout starts with a badge scan
           (no touch needed), so this is the only tappable thing on the
           splash — it competes with nothing, and a corner placement would
           just make the kiosk's one button easy to miss. -->
      <button
        v-if="timeclockButtonVisible"
        type="button"
        class="flex items-center gap-3 px-10 py-5 rounded-2xl bg-slate-800 hover:bg-slate-700 border border-slate-700 text-slate-100 text-2xl font-medium transition-transform active:scale-95"
        @click="timeclockOpen = true"
      >
        <svg class="w-8 h-8 text-slate-400" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
          <path d="M10 2a8 8 0 1 0 0 16 8 8 0 0 0 0-16Zm0 1.5a6.5 6.5 0 1 1 0 13 6.5 6.5 0 0 1 0-13Zm-.75 2.75a.75.75 0 0 1 1.5 0v3.31l2.49 1.49a.75.75 0 1 1-.77 1.29l-3.22-1.93V6.25Z" />
        </svg>
        Time clock
      </button>
    </template>
  </main>

  <main v-else class="flex-1 min-h-0 flex flex-col px-6 py-6 max-w-4xl mx-auto w-full">
    <!-- Scroll container: grows to fill the main, scrolls when lines exceed
         the available height. min-h-0 lets it shrink below content size so
         the action row below stays in view and the logo/footer don't move.
         pr-3 + cart-scroll class give the scrollbar its own gutter so it
         doesn't sit on top of the cart rows. -->
    <div
      ref="cartScroller"
      class="flex-1 min-h-0 overflow-y-auto cart-scroll pr-3"
    >
      <!-- Worker self-service: "What you have out" panel. Read-only,
           collapsed by default. Folds away on empty so a worker who's never
           checked anything out doesn't see noise. -->
      <section
        v-if="outstanding.length > 0"
        class="mb-4 rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden"
      >
        <button
          type="button"
          class="w-full flex items-center justify-between gap-3 px-4 py-3 text-left transition-colors hover:bg-slate-800/40 active:bg-slate-800/60"
          :aria-expanded="outstandingExpanded"
          @click="outstandingExpanded = !outstandingExpanded"
        >
          <div class="flex items-center gap-3">
            <span class="text-base font-medium text-slate-200">
              What you have out
            </span>
            <span
              class="inline-flex items-center justify-center min-w-7 h-7 px-2 rounded-full bg-amber-900/40 text-amber-200 text-sm font-semibold tabular-nums"
            >
              {{ outstanding.length }}
            </span>
          </div>
          <span class="text-slate-400 text-sm" aria-hidden="true">
            {{ outstandingExpanded ? '▾' : '▸' }}
          </span>
        </button>
        <ul
          v-if="outstandingExpanded"
          class="divide-y divide-slate-800 border-t border-slate-800"
        >
          <li
            v-for="o in outstanding"
            :key="o.id"
            class="flex items-center justify-between gap-4 px-4 py-2.5"
          >
            <div class="flex-1 min-w-0">
              <div class="text-slate-100 truncate">{{ o.item_name }}</div>
              <div class="text-xs text-slate-500 font-mono">
                {{ o.item_code }}<span v-if="o.instance_serial"> · {{ o.instance_serial }}</span>
              </div>
            </div>
            <div
              class="text-xs text-slate-400 tabular-nums whitespace-nowrap"
              :title="o.checked_out_at"
            >
              {{ relativeAge(o.checked_out_at) }}
            </div>
          </li>
        </ul>
      </section>

      <p
        v-if="cart.lines.length === 0"
        class="rounded-2xl bg-slate-900 border border-slate-800 border-dashed text-slate-500 text-center py-12 text-lg"
      >
        Scan items to add them to your cart.
      </p>

      <CartTable v-else :lines="cart.lines" @update="onUpdate" @remove="onRemove" />
    </div>

    <!-- Pinned to the bottom of the main so the commit button sits in a
         predictable place regardless of how many lines are in the cart. -->
    <div class="mt-8 flex gap-3 justify-end shrink-0 flex-wrap">
      <button
        type="button"
        class="px-6 py-4 rounded-xl bg-slate-800 hover:bg-slate-700 text-lg mr-auto transition-transform active:scale-95"
        @click="browseOpen = true"
      >
        Browse items
      </button>
      <button
        v-if="rfidScanButtonVisible"
        type="button"
        class="relative overflow-hidden px-6 py-4 rounded-xl bg-sky-700/80 hover:bg-sky-700 disabled:bg-slate-700 disabled:text-slate-500 text-white text-lg transition-transform active:scale-95"
        :disabled="rfidScanning"
        @click="onRFIDScan"
      >
        <!-- Countdown drains left-to-right, same shape as the receipt
             countdown bar. We render it as a thin overlay rather than
             a separate component so the visual stays anchored to the
             button it belongs to. -->
        <span
          v-if="rfidScanning"
          class="absolute left-0 top-0 h-1 bg-sky-300/80 transition-[width] duration-100"
          :style="{ width: `${rfidProgress * 100}%` }"
        ></span>
        <template v-if="rfidScanning">Reading… {{ Math.max(0, Math.ceil((1 - rfidProgress) * (RFID_READ_WINDOW_MS / 1000))) }}s</template>
        <template v-else>RFID scan</template>
      </button>
      <button
        v-if="rfidReReadButtonVisible"
        type="button"
        class="relative overflow-hidden px-6 py-4 rounded-xl bg-sky-700/80 hover:bg-sky-700 disabled:bg-slate-700 disabled:text-slate-500 text-white text-lg transition-transform active:scale-95"
        :disabled="rfidScanning"
        @click="onReReadEnclosure"
      >
        <span
          v-if="rfidScanning"
          class="absolute left-0 top-0 h-1 bg-sky-300/80 transition-[width] duration-100"
          :style="{ width: `${rfidProgress * 100}%` }"
        ></span>
        <template v-if="rfidScanning">Reading… {{ Math.max(0, Math.ceil((1 - rfidProgress) * (RFID_READ_WINDOW_MS / 1000))) }}s</template>
        <template v-else>Re-read enclosure</template>
      </button>
      <button
        v-if="isForeman"
        type="button"
        class="px-6 py-4 rounded-xl bg-amber-700/80 hover:bg-amber-700 text-white text-lg transition-transform active:scale-95"
        @click="foremanReturnOpen = true"
      >
        Return on behalf of…
      </button>
      <button
        type="button"
        class="px-6 py-4 rounded-xl bg-slate-800 hover:bg-slate-700 text-lg transition-transform active:scale-95"
        @click="onCancel"
      >
        Cancel
      </button>
      <button
        type="button"
        class="px-8 py-4 rounded-xl bg-brand-primary hover:bg-brand-primary-hover disabled:bg-slate-700 disabled:text-slate-500 text-white text-lg font-semibold transition-transform active:scale-95"
        :disabled="cart.lines.length === 0 || committing"
        @click="onCommit"
      >
        <template v-if="committing">Finishing…</template>
        <template v-else-if="cart.lines.length === 0">Finish</template>
        <template v-else>
          Finish · {{ cart.lines.length }} {{ cart.lines.length === 1 ? 'item' : 'items' }}
        </template>
      </button>
    </div>
  </main>

  <ItemBrowseDialog
    :open="browseOpen"
    :pending="browsePending"
    @update:open="browseOpen = $event"
    @pick="onBrowsePick"
    @error="toast.error($event, TOP)"
  />

  <ForemanReturnDialog
    v-if="cart"
    :open="foremanReturnOpen"
    :cart-id="cart.id"
    @update:open="foremanReturnOpen = $event"
    @added="onForemanReturnAdded"
  />

  <ConfirmDialog
    :open="crossUserConfirmOpen"
    title="Return on someone else's behalf?"
    :message="`These items are checked out to other workers. Returning them on their behalf will close their open checkouts.\n\n${crossUserSummary}`"
    confirm-label="Confirm return"
    @update:open="crossUserConfirmOpen = $event"
    @confirm="onConfirmCrossUser"
  />

  <ConfirmDialog
    :open="clockInPromptOpen"
    title="You're not clocked in"
    message="Checking out items requires clocking in first. Clock in now and finish this checkout?"
    confirm-label="Clock in & finish"
    @update:open="clockInPromptOpen = $event"
    @confirm="onConfirmClockIn"
  />

  <ConfirmDialog
    :open="cancelConfirmOpen"
    title="Discard cart?"
    :message="cart ? `You have ${cart.lines.length} item${cart.lines.length === 1 ? '' : 's'} in this cart. Cancelling will discard everything without committing.` : ''"
    confirm-label="Discard cart"
    destructive
    @update:open="cancelConfirmOpen = $event"
    @confirm="onConfirmCancel"
  />
  </div>
</template>
