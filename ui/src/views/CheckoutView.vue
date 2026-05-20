<script setup lang="ts">
import { computed, ref } from 'vue'
import { storeToRefs } from 'pinia'
import ScanInput from '../components/ScanInput.vue'
import CartTable from '../components/CartTable.vue'
import ItemBrowseDialog from '../components/ItemBrowseDialog.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import { useCart } from '../composables/useCart'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import { useSessionStore } from '../stores/session'
import { ApiError } from '../lib/api'
import type { CartAction, CartLine, CommitResult, Item, User } from '../types'

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
let dismissHandle: ReturnType<typeof setTimeout> | null = null

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
      session.setFlash('info', `Welcome, ${u.name}`)
    } catch (e) {
      handleApiError(e)
    }
    return
  }

  if (result.type === 'item') {
    if (!cart.value) {
      session.setFlash('warn', 'Scan your badge first')
      return
    }
    try {
      const line = await c.addItem((result.record as Item).code)
      if (line.warnings && line.warnings.length > 0) {
        const first = line.warnings[0]
        if (first.startsWith('cross_user_return:')) {
          session.setFlash('warn', `Returning ${first.slice('cross_user_return:'.length)}'s ${line.item_name}`)
        }
      }
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
  try {
    await c.cancel()
    session.setFlash('info', 'Session ended')
  } catch (e) {
    handleApiError(e)
  }
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

  <main
    v-if="success"
    class="flex flex-col items-center px-6 py-10"
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

  <main v-else-if="!cart" class="flex flex-col items-center justify-center px-8 py-16 text-center gap-10">
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
  </main>

  <main v-else class="px-6 py-8 max-w-4xl mx-auto w-full">
    <header class="flex items-baseline justify-between mb-6">
      <div>
        <p class="text-sm text-slate-400">Signed in as</p>
        <h1 class="text-3xl font-semibold">{{ cart.user_name }}</h1>
        <p class="text-sm text-slate-500">{{ cart.user_code }}</p>
      </div>
      <p class="text-slate-500 text-sm">
        Cart {{ cart.lines.length }} line<span v-if="cart.lines.length !== 1">s</span>
      </p>
    </header>

    <p
      v-if="cart.lines.length === 0"
      class="rounded-2xl bg-slate-900 border border-slate-800 border-dashed text-slate-500 text-center py-12 text-lg"
    >
      Scan items to add them to your cart.
    </p>

    <CartTable v-else :lines="cart.lines" @update="onUpdate" @remove="onRemove" />

    <div class="mt-8 flex gap-3 justify-end">
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
</template>
