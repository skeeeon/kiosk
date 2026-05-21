<script setup lang="ts">
import { computed, nextTick, ref } from 'vue'
import { storeToRefs } from 'pinia'
import ScanInput from '../components/ScanInput.vue'
import CartTable from '../components/CartTable.vue'
import ItemBrowseDialog from '../components/ItemBrowseDialog.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import IdentifyPanel from '../components/IdentifyPanel.vue'
import { useCart } from '../composables/useCart'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import { useSessionStore } from '../stores/session'
import { ApiError } from '../lib/api'
import type { CartAction, CartLine, CommitResult, InstanceMatch, Item, User } from '../types'

const session = useSessionStore()
const { cart, flash } = storeToRefs(session)
const c = useCart()
const { identity } = useKioskIdentity()

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
const crossUserConfirmOpen = ref(false)
const cancelConfirmOpen = ref(false)
let dismissHandle: ReturnType<typeof setTimeout> | null = null

// identify holds the result of an item scan that landed before a badge —
// the splash promises "scan an item code to identify it" and this drives
// the panel that delivers on that. Cleared on next badge / item-with-cart
// scan or via the panel's Dismiss button. Auto-dismisses so a curious
// peek doesn't lock the splash forever.
const identify = ref<{ item: Item | null; instance: InstanceMatch | null } | null>(null)
const IDENTIFY_TIMEOUT_MS = 15000
let identifyDismissHandle: ReturnType<typeof setTimeout> | null = null
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
    session.setFlash('warn', 'Your session has expired. Scan your badge to begin again.', 6000)
    return
  }
  const msg = (e as Error).message
  session.setFlash('error', fallbackPrefix ? `${fallbackPrefix}: ${msg}` : msg)
}

const crossUserLines = computed<CartLine[]>(() => {
  if (!cart.value) return []
  return cart.value.lines.filter((l) =>
    (l.warnings ?? []).some((w) => w.startsWith('cross_user_return:')),
  )
})

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
  if (dismissHandle) {
    clearTimeout(dismissHandle)
    dismissHandle = null
  }
  success.value = null
}

async function onScan(raw: string) {
  let result
  try {
    result = await c.scanDispatch(raw)
  } catch (e) {
    handleApiError(e, 'Scan failed')
    return
  }

  if (result.type === 'unknown') {
    session.setFlash('warn', `Unknown code: ${result.value ?? raw}`)
    return
  }

  if (result.type === 'user') {
    const u = result.record as User
    if (cart.value && cart.value.user_id !== u.id) {
      session.setFlash(
        'warn',
        `${cart.value.user_name} is still active. Cancel or commit first.`,
      )
      return
    }
    try {
      await c.start(u.code)
      dismissIdentify()
      const welcome = u.open_count > 0
        ? `Welcome, ${u.name} — ${u.open_count} item${u.open_count === 1 ? '' : 's'} out`
        : `Welcome, ${u.name}`
      session.setFlash('info', welcome)
    } catch (e) {
      handleApiError(e)
    }
    return
  }

  if (result.type === 'item' || result.type === 'item_instance') {
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
    try {
      const line = await c.addItem(code)
      if (line.warnings && line.warnings.length > 0) {
        const first = line.warnings[0]
        if (first.startsWith('cross_user_return:')) {
          session.setFlash('warn', `Returning ${first.slice('cross_user_return:'.length)}'s ${line.item_name}`)
        }
      }
      await scrollCartToBottom()
    } catch (e) {
      handleApiError(e)
    }
  }
}

async function onUpdate(id: string, patch: { qty?: number; action?: CartAction }) {
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
    session.setFlash('info', 'Session ended')
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
    if (line.warnings && line.warnings.length > 0) {
      const first = line.warnings[0]
      if (first.startsWith('cross_user_return:')) {
        session.setFlash('warn', `Returning ${first.slice('cross_user_return:'.length)}'s ${line.item_name}`)
      }
    } else {
      session.setFlash('info', `Added ${line.item_name}`)
    }
    await scrollCartToBottom()
  } catch (e) {
    handleApiError(e)
  } finally {
    browsePending.value = false
  }
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
    const result = await c.commit()
    success.value = { result, lines: snapshotLines, userName: snapshotUser }
    if (dismissHandle) clearTimeout(dismissHandle)
    dismissHandle = setTimeout(dismissReceipt, SUCCESS_SCREEN_MS)
  } catch (e) {
    handleApiError(e)
  } finally {
    committing.value = false
  }
}

function onConfirmCrossUser() {
  crossUserConfirmOpen.value = false
  void doCommit()
}

const crossUserSummary = computed(() =>
  crossUserLines.value
    .map((l) => {
      const w = (l.warnings ?? []).find((x) => x.startsWith('cross_user_return:'))
      const who = w ? w.slice('cross_user_return:'.length) : 'someone else'
      return `${l.item_code} — ${who}`
    })
    .join('\n'),
)

const flashClasses = {
  info: 'bg-sky-900/60 border-sky-700/70 text-sky-100',
  warn: 'bg-amber-900/60 border-amber-700/70 text-amber-100',
  error: 'bg-red-900/60 border-red-700/70 text-red-100',
}
</script>

<template>
  <!-- Single flex-col root so RouterView's flex-1 class can apply and the
       inner mains can use flex-1 to fill the viewport area between the
       app-level header and footer. min-h-0 lets the inner cart-lines area
       overflow without pushing the chrome and footer off-screen. -->
  <div class="flex-1 flex flex-col min-h-0">
  <ScanInput @scan="onScan" />

  <Transition
    enter-active-class="transition duration-200 ease-out"
    enter-from-class="opacity-0 -translate-y-2"
    enter-to-class="opacity-100 translate-y-0"
    leave-active-class="transition duration-150 ease-in"
    leave-from-class="opacity-100"
    leave-to-class="opacity-0"
  >
    <div
      v-if="flash"
      class="fixed top-16 left-1/2 -translate-x-1/2 z-20 px-6 py-3 rounded-xl border text-lg shadow-lg"
      :class="flashClasses[flash.kind]"
    >
      {{ flash.text }}
    </div>
  </Transition>

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
      class="h-28 md:h-36 w-auto object-contain shrink-0"
      @error="splashLogoBroken = true"
    />
    <span v-else class="text-3xl md:text-5xl font-semibold tracking-wide shrink-0">Kiosk</span>
    <div v-if="cart" class="text-right shrink-0">
      <p class="text-2xl text-slate-100">{{ cart.user_name }}</p>
      <p class="text-sm text-slate-500 font-mono">{{ cart.user_code }}</p>
    </div>
  </header>

  <!-- Receipt scrolls within the viewport area when the transaction has
       more lines than fit. Unlike the cart, the scroll position stays at
       the top so the worker sees "Done / Thanks, {name}" and the first
       few lines first; they can scroll down to read the rest or hit the
       Done button at the bottom. -->
  <main
    v-if="success"
    class="flex-1 min-h-0 overflow-y-auto flex flex-col items-center px-6 py-10"
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

      <div class="flex justify-center mt-6">
        <button
          type="button"
          class="px-8 py-3 rounded-xl bg-slate-800 hover:bg-slate-700 text-slate-200 text-base"
          @click="dismissReceipt"
        >
          Done
        </button>
      </div>
    </div>
  </main>

  <main v-else-if="!cart" class="flex-1 flex flex-col items-center justify-center px-8 py-16 text-center gap-10">
    <IdentifyPanel
      v-if="identify"
      :item="identify.item"
      :instance="identify.instance"
      @dismiss="dismissIdentify"
    />
    <template v-else>
      <div v-if="splashLogoUrl || splashTagline" class="flex flex-col items-center gap-4">
        <img
          v-if="splashLogoUrl"
          :src="splashLogoUrl"
          alt="logo"
          class="h-28 md:h-36 w-auto object-contain"
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
    <div class="mt-8 flex gap-3 justify-end shrink-0">
      <button
        type="button"
        class="px-6 py-4 rounded-xl bg-slate-800 hover:bg-slate-700 text-lg mr-auto"
        @click="browseOpen = true"
      >
        Browse items
      </button>
      <button
        type="button"
        class="px-6 py-4 rounded-xl bg-slate-800 hover:bg-slate-700 text-lg"
        @click="onCancel"
      >
        Cancel
      </button>
      <button
        type="button"
        class="px-8 py-4 rounded-xl bg-brand-primary hover:bg-brand-primary-hover disabled:bg-slate-700 disabled:text-slate-500 text-white text-lg font-semibold"
        :disabled="cart.lines.length === 0 || committing"
        @click="onCommit"
      >
        {{ committing ? 'Committing…' : 'Commit' }}
      </button>
    </div>
  </main>

  <ItemBrowseDialog
    :open="browseOpen"
    :pending="browsePending"
    @update:open="browseOpen = $event"
    @pick="onBrowsePick"
    @error="session.setFlash('error', $event)"
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
