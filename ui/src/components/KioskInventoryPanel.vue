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
import DataTable, { type ColumnDef } from './DataTable.vue'
import { api, isKioskOfflineError as isOfflineError } from '../lib/api'
import { availableFor, isLowStock } from '../lib/inventory'
import { useToast } from '../composables/useToast'
import type {
  InventoryAdjustResponse,
  InventorySnapshotItem,
  InventorySnapshotResponse,
  InventoryThresholdResponse,
} from '../types'

const props = defineProps<{ kioskCode: string }>()
const toast = useToast()

const items = ref<InventorySnapshotItem[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const offline = ref(false)
const page = ref(1)
const perPage = ref(25)

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

// Reorder threshold. Separate from the adjust dialog because it is a
// different kind of change: adjust moves a physical count and demands a
// reason for the audit ledger; a threshold is a policy knob that moves
// nothing and audits nothing. It is also available for serialized SKUs,
// which have no Adjust button at all.
const thresholdOpen = ref(false)
const thresholdForm = ref<{ item_code: string; item_name: string; current: number; value: number }>({
  item_code: '',
  item_name: '',
  current: 0,
  value: 0,
})
const thresholdSubmitting = ref(false)

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

function openThreshold(it: InventorySnapshotItem) {
  thresholdForm.value = {
    item_code: it.item_code,
    item_name: it.item_name,
    current: it.reorder_threshold,
    value: it.reorder_threshold,
  }
  thresholdOpen.value = true
}

async function submitThreshold() {
  const f = thresholdForm.value
  if (f.value < 0) {
    toast.error('Threshold must be zero or greater')
    return
  }
  thresholdSubmitting.value = true
  try {
    const res = await api.post<InventoryThresholdResponse>(
      `/api/controller/kiosks/${encodeURIComponent(props.kioskCode)}/inventory/threshold`,
      { item_code: f.item_code, value: f.value },
    )
    // Patch the in-memory row from the reply, sparing a full refetch —
    // same trick submitAdjust uses.
    const idx = items.value.findIndex((x) => x.item_code === res.item_code)
    if (idx >= 0) {
      items.value[idx] = { ...items.value[idx], reorder_threshold: res.reorder_threshold }
    }
    thresholdOpen.value = false
    toast.success(
      res.reorder_threshold === 0
        ? `${res.item_code}: low-stock alert off`
        : `${res.item_code}: alert at ${res.reorder_threshold} or fewer`,
    )
  } catch (e) {
    if (isOfflineError(e)) {
      offline.value = true
      thresholdOpen.value = false
      toast.error('Kiosk is offline — threshold not applied')
    } else {
      toast.error((e as Error).message)
    }
  } finally {
    thresholdSubmitting.value = false
  }
}

const columns: ColumnDef[] = [
  { key: 'item_code', label: 'Code' },
  { key: 'item_name', label: 'Name' },
  { key: 'quantity_on_hand', label: 'On hand', align: 'right' },
  { key: 'out', label: 'Out', align: 'right' },
  { key: 'available', label: 'Available', align: 'right' },
  { key: 'reorder_threshold', label: 'Reorder ≤', align: 'right' },
  { key: '__actions', align: 'right' },
]

// available + low-stock use the shared helper so this panel matches the local
// kiosk Items view exactly. `out` is supplied by the controller (derived from
// its projected ledger); `type` distinguishes tools from consumables;
// `maintenance` (serialized units parked at the bench) is carried from the
// kiosk's snapshot and subtracted alongside out.
function availableOf(it: InventorySnapshotItem): number {
  return availableFor(it.quantity_on_hand, it.out ?? 0, it.type, it.maintenance ?? 0)
}

function isLow(it: InventorySnapshotItem): boolean {
  return isLowStock(availableOf(it), it.reorder_threshold)
}

const pagedItems = computed(() => {
  const start = (page.value - 1) * perPage.value
  return items.value.slice(start, start + perPage.value)
})
</script>

<template>
  <section class="space-y-3">
    <header class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2">
      <div>
        <h3 class="text-sm font-medium text-slate-200">Live inventory</h3>
        <p class="text-xs text-slate-500">
          Snapshot fetched from the kiosk over NATS. Adjustments apply on the kiosk and
          publish an audit event. Reorder thresholds are per-kiosk — click one to set it.
          Refresh to re-poll.
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
      class="rounded-lg bg-amber-900/40 border border-amber-800 text-amber-100 text-sm px-3 py-2"
    >
      This kiosk hasn't sent a heartbeat recently. Inventory snapshot and remote
      adjustments are unavailable until it reconnects.
    </div>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 text-sm px-3 py-2">
      {{ error }}
    </p>

    <DataTable
      :columns="columns"
      :rows="pagedItems"
      :row-key="(it) => it.item_code"
      :loading="loading"
      empty-text="No items stocked at this kiosk."
      :page="page"
      :per-page="perPage"
      :total="items.length"
      @update:page="(p) => page = p"
      @update:per-page="(n) => { perPage = n; page = 1 }"
    >
      <template #cell-item_code="{ row }">
        <span class="font-mono text-slate-200">{{ row.item_code }}</span>
      </template>
      <template #cell-item_name="{ row }">
        <span class="text-slate-300">{{ row.item_name }}</span>
      </template>
      <template #cell-quantity_on_hand="{ row }">
        <span class="tabular-nums font-mono text-slate-200">{{ row.quantity_on_hand }}</span>
      </template>
      <template #cell-out="{ row }">
        <span class="tabular-nums font-mono text-slate-400">{{ row.out ?? 0 }}</span>
      </template>
      <template #cell-available="{ row }">
        <span
          class="tabular-nums font-mono"
          :class="isLow(row) ? 'text-amber-300' : 'text-slate-200'"
          :title="isLow(row) ? 'At or below reorder threshold' : undefined"
        >{{ availableOf(row) }}</span>
      </template>
      <!-- Editable in place: the threshold is the one inventory field a
           controller admin can set for ANY SKU here, serialized included,
           so it doesn't belong behind the quantity-only Adjust button. -->
      <template #cell-reorder_threshold="{ row }">
        <button
          type="button"
          class="tabular-nums font-mono text-slate-400 hover:text-slate-100 underline decoration-dotted underline-offset-4 disabled:opacity-50 disabled:no-underline disabled:hover:text-slate-400"
          :disabled="offline"
          :title="offline ? 'Kiosk is offline' : 'Set the low-stock alert level for this kiosk'"
          @click="openThreshold(row)"
        >
          {{ row.reorder_threshold > 0 ? row.reorder_threshold : '—' }}
        </button>
      </template>
      <template #cell-__actions="{ row }">
        <!-- Serialized items derive their count from active instances, so a
             direct quantity adjust is rejected server-side. Manage their units
             on the Instances tab instead. -->
        <span
          v-if="row.tracking_mode === 'serialized'"
          class="text-xs text-slate-500"
          title="Serialized — change the count on the Instances tab by adding or retiring units"
        >
          serialized
        </span>
        <button
          v-else
          type="button"
          class="px-3 py-1.5 rounded-md bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm border border-slate-700 whitespace-nowrap disabled:opacity-50"
          :disabled="offline"
          @click="openAdjust(row)"
        >
          Adjust
        </button>
      </template>
    </DataTable>

    <AppDialog
      :open="adjustOpen"
      title="Adjust quantity"
      :description="`${adjustForm.item_code} — ${adjustForm.item_name}`"
      size="sm"
      @update:open="(v) => { if (!v) adjustOpen = false }"
    >
      <form class="flex flex-col gap-4" @submit.prevent="submitAdjust">
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
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

    <AppDialog
      :open="thresholdOpen"
      title="Reorder threshold"
      :description="`${thresholdForm.item_code} — ${thresholdForm.item_name}`"
      size="sm"
      @update:open="(v) => { if (!v) thresholdOpen = false }"
    >
      <form class="flex flex-col gap-4" @submit.prevent="submitThreshold">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Alert when available is at or below</span>
          <input
            v-model.number="thresholdForm.value"
            type="number"
            min="0"
            step="1"
            required
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 font-mono"
          />
          <span class="text-xs text-slate-500">0 turns the low-stock alert off for this SKU.</span>
        </label>

        <p class="text-xs text-slate-400 bg-slate-950 border border-slate-800 rounded-lg px-3 py-2">
          This applies to <span class="font-mono text-slate-200">{{ kioskCode }}</span> only.
          Thresholds are per-kiosk, so a busy crib and a quiet cross-dock can hold
          different levels for the same SKU.
        </p>

        <div class="flex justify-end gap-3 mt-1">
          <button
            type="button"
            class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
            @click="thresholdOpen = false"
          >
            Cancel
          </button>
          <button
            type="submit"
            class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium disabled:opacity-50"
            :disabled="thresholdSubmitting"
          >
            {{ thresholdSubmitting ? 'Submitting…' : 'Save' }}
          </button>
        </div>
      </form>
    </AppDialog>
  </section>
</template>
