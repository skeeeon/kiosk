<!-- KioskInventoryPanel surfaces a managed kiosk's live inventory from the
     controller. On mount it fetches a snapshot via the
     inventory.snapshot NATS command (proxied by GET
     /api/controller/kiosks/:code/inventory) and lets an admin issue an
     adjustment from the same screen via POST .../inventory/adjust.

     503 with body {error: "kiosk_offline"} renders an inline banner; any
     other error surfaces in the usual red box. -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import { api, ApiError } from '../lib/api'
import { useAdminToast } from '../composables/useAdminToast'
import type {
  InventoryAdjustResponse,
  InventorySnapshotItem,
  InventorySnapshotResponse,
  KioskOfflineError,
} from '../types'

const props = defineProps<{ kioskCode: string }>()
const toast = useAdminToast()

const items = ref<InventorySnapshotItem[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const offline = ref(false)

const adjustOpen = ref(false)
const adjustForm = ref<{
  item_code: string
  item_name: string
  current: number
  mode: 'delta' | 'absolute'
  value: number
  reason: string
}>({
  item_code: '',
  item_name: '',
  current: 0,
  mode: 'delta',
  value: 0,
  reason: '',
})
const adjustSubmitting = ref(false)

async function loadSnapshot() {
  if (!props.kioskCode) return
  loading.value = true
  error.value = null
  offline.value = false
  try {
    const res = await api.get<InventorySnapshotResponse>(
      `/api/controller/kiosks/${encodeURIComponent(props.kioskCode)}/inventory`,
    )
    items.value = res.items
  } catch (e) {
    if (isOfflineError(e)) {
      offline.value = true
      items.value = []
    } else {
      error.value = (e as Error).message
    }
  } finally {
    loading.value = false
  }
}

// isOfflineError identifies the 503 + kiosk_offline body so the panel can
// render a banner rather than a red error box. Any other error stays a
// generic failure.
function isOfflineError(e: unknown): boolean {
  if (e instanceof ApiError && e.status === 503) {
    const data = e.data as KioskOfflineError | null
    return data?.error === 'kiosk_offline'
  }
  return false
}

watch(() => props.kioskCode, (c) => { if (c) void loadSnapshot() }, { immediate: true })

function openAdjust(it: InventorySnapshotItem) {
  adjustForm.value = {
    item_code: it.item_code,
    item_name: it.item_name,
    current: it.quantity_on_hand,
    mode: 'delta',
    value: 0,
    reason: '',
  }
  adjustOpen.value = true
}

// previewNewQty is what the new quantity_on_hand would be if the user
// submitted right now. Cosmetic — the server is authoritative — but useful
// feedback when entering deltas or absolute values.
const previewNewQty = computed(() => {
  if (adjustForm.value.mode === 'delta') {
    return adjustForm.value.current + adjustForm.value.value
  }
  return adjustForm.value.value
})

async function submitAdjust() {
  const f = adjustForm.value
  if (!f.reason.trim()) {
    toast.error('Reason is required')
    return
  }
  adjustSubmitting.value = true
  try {
    const res = await api.post<InventoryAdjustResponse>(
      `/api/controller/kiosks/${encodeURIComponent(props.kioskCode)}/inventory/adjust`,
      {
        item_code: f.item_code,
        mode: f.mode,
        value: f.value,
        reason: f.reason.trim(),
      },
    )
    // Patch the in-memory row from the reply, sparing a full refetch.
    const idx = items.value.findIndex((x) => x.item_code === res.item_code)
    if (idx >= 0) {
      items.value[idx] = { ...items.value[idx], quantity_on_hand: res.new_quantity }
    }
    adjustOpen.value = false
    toast.success(`${res.item_code}: ${res.prev_quantity} → ${res.new_quantity}`)
  } catch (e) {
    if (isOfflineError(e)) {
      offline.value = true
      adjustOpen.value = false
      toast.error('Kiosk is offline — adjustment not applied')
    } else {
      toast.error((e as Error).message)
    }
  } finally {
    adjustSubmitting.value = false
  }
}
</script>

<template>
  <section class="rounded-xl bg-slate-950/40 border border-slate-800 p-4">
    <header class="flex items-center justify-between mb-3">
      <div>
        <h3 class="text-sm font-medium text-slate-200">Live inventory</h3>
        <p class="text-xs text-slate-500">
          Snapshot fetched from the kiosk over NATS. Adjustments apply on the kiosk and
          publish an audit event. Refresh to re-poll.
        </p>
      </div>
      <button
        type="button"
        class="text-sm px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 disabled:opacity-50"
        :disabled="loading"
        @click="loadSnapshot"
      >
        {{ loading ? 'Loading…' : 'Refresh' }}
      </button>
    </header>

    <div
      v-if="offline"
      class="rounded-lg bg-amber-900/40 border border-amber-800 text-amber-100 text-sm px-3 py-2 mb-3"
    >
      This kiosk hasn't sent a heartbeat recently. Inventory snapshot and remote
      adjustments are unavailable until it reconnects.
    </div>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 text-sm px-3 py-2 mb-3">
      {{ error }}
    </p>

    <table class="w-full text-left text-xs">
      <thead class="text-slate-500">
        <tr>
          <th class="px-2 py-2 font-medium">Code</th>
          <th class="px-2 py-2 font-medium">Name</th>
          <th class="px-2 py-2 font-medium text-right">On hand</th>
          <th class="px-2 py-2 font-medium text-right">Reorder ≤</th>
          <th class="px-2 py-2"></th>
        </tr>
      </thead>
      <tbody class="divide-y divide-slate-800">
        <tr v-if="loading">
          <td colspan="5" class="text-center text-slate-500 py-3">Loading…</td>
        </tr>
        <tr v-else-if="!offline && items.length === 0">
          <td colspan="5" class="text-center text-slate-500 py-3">
            No items stocked at this kiosk.
          </td>
        </tr>
        <tr v-for="it in items" :key="it.item_code" class="hover:bg-slate-900/50">
          <td class="px-2 py-2 font-mono text-slate-200">{{ it.item_code }}</td>
          <td class="px-2 py-2 text-slate-300 truncate max-w-xs">{{ it.item_name }}</td>
          <td
            class="px-2 py-2 text-right font-mono"
            :class="it.reorder_threshold > 0 && it.quantity_on_hand <= it.reorder_threshold ? 'text-amber-300' : 'text-slate-200'"
          >
            {{ it.quantity_on_hand }}
          </td>
          <td class="px-2 py-2 text-right text-slate-500 font-mono">
            {{ it.reorder_threshold > 0 ? it.reorder_threshold : '—' }}
          </td>
          <td class="px-2 py-2 text-right whitespace-nowrap">
            <button
              type="button"
              class="text-sm text-brand-primary hover:underline disabled:opacity-50"
              :disabled="offline"
              @click="openAdjust(it)"
            >
              Adjust
            </button>
          </td>
        </tr>
      </tbody>
    </table>

    <AppDialog
      :open="adjustOpen"
      title="Adjust quantity"
      :description="`${adjustForm.item_code} — ${adjustForm.item_name}`"
      size="sm"
      @update:open="(v) => { if (!v) adjustOpen = false }"
    >
      <form class="flex flex-col gap-4" @submit.prevent="submitAdjust">
        <div class="grid grid-cols-2 gap-3">
          <label class="flex flex-col gap-1">
            <span class="text-sm text-slate-400">Mode</span>
            <select
              v-model="adjustForm.mode"
              class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
            >
              <option value="delta">Delta (+/-)</option>
              <option value="absolute">Set absolute</option>
            </select>
          </label>
          <label class="flex flex-col gap-1">
            <span class="text-sm text-slate-400">Value</span>
            <input
              v-model.number="adjustForm.value"
              type="number"
              required
              class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 font-mono"
            />
          </label>
        </div>

        <div class="text-xs text-slate-400 bg-slate-950 border border-slate-800 rounded-lg px-3 py-2">
          Current on hand:
          <span class="font-mono text-slate-200">{{ adjustForm.current }}</span>
          → New:
          <span class="font-mono text-slate-200">{{ previewNewQty }}</span>
        </div>

        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Reason</span>
          <textarea
            v-model="adjustForm.reason"
            rows="2"
            required
            placeholder="e.g. physical count, restock from PO-42"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 resize-none"
          ></textarea>
        </label>

        <div class="flex justify-end gap-3 mt-1">
          <button
            type="button"
            class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
            @click="adjustOpen = false"
          >
            Cancel
          </button>
          <button
            type="submit"
            class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium disabled:opacity-50"
            :disabled="adjustSubmitting"
          >
            {{ adjustSubmitting ? 'Submitting…' : 'Apply' }}
          </button>
        </div>
      </form>
    </AppDialog>
  </section>
</template>
