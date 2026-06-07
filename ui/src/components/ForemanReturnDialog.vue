<!-- ForemanReturnDialog is the picker for the "Return on behalf of…" flow.
     Two paths, both visible at once:

       1. Scan input at the top — for the case the foreman has the
          physical (serialized) tool in hand. Server resolves the
          instance, derives the holder from its open_checkouts row,
          validates same-group. Zero typing, zero picking.
       2. Worker list below — for the case the absent worker's tool is
          on the floor / not yet returned. Each row shows a group member
          with at least one open checkout; expanding it reveals their
          items; clicking an item adds it as a foreman return.

     The endpoint pre-flights all the same rules commit re-enforces
     (foreman + same group), so failures here surface immediately
     instead of five scans later. The dialog is also the only writer of
     original_checkout_user_id on cart lines (server-side invariant
     documented in CLAUDE.md). -->
<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import { api, ApiError } from '../lib/api'
import type {
  Cart,
  CartLine,
  ForemanReturnOptions,
  ForemanReturnWorker,
  OpenCheckoutDetail,
} from '../types'

const props = defineProps<{
  open: boolean
  cartId: string
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  added: [payload: { cart: Cart; line: CartLine }]
}>()

const options = ref<ForemanReturnOptions | null>(null)
const loading = ref(false)
const errorMsg = ref('')
const scanCode = ref('')
const scanInput = ref<HTMLInputElement | null>(null)
const expandedWorker = ref<string | null>(null)
const submitting = ref<string | null>(null) // key of row being added

watch(
  () => props.open,
  async (open) => {
    if (!open) return
    scanCode.value = ''
    errorMsg.value = ''
    expandedWorker.value = null
    submitting.value = null
    await loadOptions()
    await nextTick()
    scanInput.value?.focus()
  },
)

async function loadOptions() {
  loading.value = true
  errorMsg.value = ''
  try {
    options.value = await api.get<ForemanReturnOptions>(
      `/api/kiosk/cart/foreman-return/options?cart_id=${encodeURIComponent(props.cartId)}`,
    )
  } catch (e) {
    options.value = null
    errorMsg.value = e instanceof ApiError ? e.message : (e as Error).message
  } finally {
    loading.value = false
  }
}

async function submitForemanReturn(body: { item_code: string; target_user_code?: string }, rowKey: string) {
  if (submitting.value) return
  submitting.value = rowKey
  errorMsg.value = ''
  try {
    const payload = await api.post<{ cart: Cart; line: CartLine }>(
      '/api/kiosk/cart/foreman-return',
      { cart_id: props.cartId, ...body },
    )
    emit('added', payload)
    emit('update:open', false)
  } catch (e) {
    errorMsg.value = e instanceof ApiError ? e.message : (e as Error).message
    // Reload options on failure — the picker state may be stale (the row
    // could have been returned by someone else between fetch and click).
    void loadOptions()
  } finally {
    submitting.value = null
  }
}

async function onScanSubmit() {
  const code = scanCode.value.trim()
  if (!code) return
  await submitForemanReturn({ item_code: code }, `scan:${code}`)
}

async function onPickItem(worker: ForemanReturnWorker, row: OpenCheckoutDetail) {
  // For serialized rows the resolver expects the instance code (what's
  // printed on the scannable label, not the human-readable serial).
  // Fall back to the SKU code for non-serialized rows.
  const itemCode = row.item_instance_code || row.item_code
  await submitForemanReturn(
    { item_code: itemCode, target_user_code: worker.user_code },
    row.id,
  )
}

function toggleWorker(userID: string) {
  expandedWorker.value = expandedWorker.value === userID ? null : userID
}

function onCancel() {
  emit('update:open', false)
}

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

const hasWorkers = computed(() => (options.value?.workers.length ?? 0) > 0)
</script>

<template>
  <AppDialog
    :open="open"
    title="Return on behalf of"
    :description="
      options
        ? `Workers in ${options.group_code} with open checkouts. Or scan a tool you have in hand.`
        : 'Workers in your group with open checkouts. Or scan a tool you have in hand.'
    "
    size="md"
    @update:open="emit('update:open', $event)"
  >
    <div class="flex flex-col gap-5">
      <!-- Scan shortcut. Submitting from this input does not require
           picking a worker — the server derives the holder from the
           instance's open_checkouts row. Errors (no open checkout,
           wrong group) surface inline. -->
      <form class="flex flex-col gap-1" @submit.prevent="onScanSubmit">
        <label class="text-sm text-slate-300">Scan a tool you have in hand</label>
        <div class="flex gap-2">
          <input
            ref="scanInput"
            v-model="scanCode"
            type="text"
            class="flex-1 px-3 py-2 rounded-lg bg-slate-800 border border-slate-700 text-slate-100 font-mono focus:outline-none focus:border-brand-primary"
            placeholder="Instance code or RFID"
            autocomplete="off"
            spellcheck="false"
          />
          <button
            type="submit"
            class="px-5 py-3 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium transition-transform active:scale-95 disabled:bg-slate-700 disabled:text-slate-500 disabled:active:scale-100"
            :disabled="!scanCode.trim() || submitting !== null"
          >
            <template v-if="submitting === `scan:${scanCode.trim()}`">Adding…</template>
            <template v-else>Add</template>
          </button>
        </div>
      </form>

      <p
        v-if="errorMsg"
        class="rounded-lg bg-red-900/40 border border-red-700/60 text-red-200 text-sm px-3 py-2"
      >
        {{ errorMsg }}
      </p>

      <div class="border-t border-slate-800"></div>

      <!-- Worker picker. Each worker is expandable; clicking an item
           inside their list submits a foreman-return with both the
           worker code (target) and the item's scannable code. -->
      <div class="flex flex-col gap-2 max-h-[50vh] overflow-y-auto">
        <p v-if="loading" class="text-slate-400 text-sm">Loading…</p>
        <p
          v-else-if="!hasWorkers"
          class="rounded-xl bg-slate-800/60 border border-slate-700/60 text-slate-400 text-sm px-4 py-6 text-center"
        >
          No one in your group currently has anything out.
        </p>

        <div
          v-for="worker in options?.workers ?? []"
          :key="worker.user_id"
          class="rounded-xl bg-slate-800/60 border border-slate-700/60 overflow-hidden"
        >
          <button
            type="button"
            class="w-full flex items-center justify-between gap-3 px-4 py-3 text-left transition-colors hover:bg-slate-800 active:bg-slate-700"
            :aria-expanded="expandedWorker === worker.user_id"
            @click="toggleWorker(worker.user_id)"
          >
            <div class="min-w-0">
              <p class="text-slate-100 font-medium truncate">{{ worker.user_name }}</p>
              <p class="text-xs text-slate-500 font-mono truncate">{{ worker.user_code }}</p>
            </div>
            <div class="flex items-center gap-3">
              <span
                class="inline-flex items-center justify-center min-w-7 h-7 px-2 rounded-full bg-amber-900/40 text-amber-200 text-sm font-semibold tabular-nums"
              >
                {{ worker.open_checkouts.length }}
              </span>
              <span class="text-slate-400 text-sm" aria-hidden="true">
                {{ expandedWorker === worker.user_id ? '▾' : '▸' }}
              </span>
            </div>
          </button>

          <ul
            v-if="expandedWorker === worker.user_id"
            class="divide-y divide-slate-800 border-t border-slate-800"
          >
            <li
              v-for="row in worker.open_checkouts"
              :key="row.id"
            >
              <button
                type="button"
                class="w-full flex items-center gap-3 px-4 py-3 text-left transition-colors hover:bg-slate-800/60 active:bg-slate-800 disabled:opacity-50"
                :disabled="submitting !== null"
                @click="onPickItem(worker, row)"
              >
                <div class="min-w-0 flex-1">
                  <div class="text-slate-100 truncate">{{ row.item_name }}</div>
                  <div class="text-xs text-slate-500 font-mono truncate">
                    {{ row.item_instance_code || row.item_code }}<span
                      v-if="row.instance_serial"
                    > · SN {{ row.instance_serial }}</span>
                  </div>
                </div>
                <div
                  class="text-xs text-slate-400 tabular-nums whitespace-nowrap"
                  :title="row.checked_out_at"
                >
                  {{ relativeAge(row.checked_out_at) }}
                </div>
                <span
                  v-if="submitting === row.id"
                  class="text-xs text-slate-400"
                >Adding…</span>
              </button>
            </li>
          </ul>
        </div>
      </div>

      <div class="flex justify-end gap-3 pt-2">
        <button
          type="button"
          class="px-6 py-3 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 transition-transform active:scale-95"
          @click="onCancel"
        >
          Close
        </button>
      </div>
    </div>
  </AppDialog>
</template>
