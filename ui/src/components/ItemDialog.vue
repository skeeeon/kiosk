<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import AppDialog from './AppDialog.vue'
import ItemInstancesPanel from './ItemInstancesPanel.vue'
import StockAdjustDialog from './StockAdjustDialog.vue'
import StockAdjustmentHistoryDialog from './StockAdjustmentHistoryDialog.vue'
import type { ItemRecord, KioskRecord, StockAdjustmentResult } from '../types'

const props = withDefaults(
  defineProps<{
    open: boolean
    item: Partial<ItemRecord> | null
    // Managed mode (kiosk only): catalog fields read-only; stock adjust still
    // available locally.
    managed?: boolean
    // Controller mode: full edit access, but the quantity / threshold / stock
    // adjust / instances affordances are hidden because they're kiosk-local
    // concepts.
    isController?: boolean
  }>(),
  { managed: false, isController: false },
)

const emit = defineEmits<{
  'update:open': [value: boolean]
  save: [data: Partial<ItemRecord>]
}>()

// `kind` collapses the two underlying axes (type, tracking_mode) into one
// picker. consumable+serialized isn't a real combination, so we don't expose
// it. Mapping is one-way deterministic on save; on load we project the two
// fields back into the right kind.
type Kind = 'tool-qty' | 'tool-serial' | 'consumable'

function kindFromItem(it: Partial<ItemRecord> | null): Kind {
  if (!it) return 'tool-qty'
  if (it.type === 'consumable') return 'consumable'
  return it.tracking_mode === 'serialized' ? 'tool-serial' : 'tool-qty'
}

function applyKindToForm(k: Kind) {
  if (k === 'consumable') {
    form.type = 'consumable'
    form.tracking_mode = 'quantity'
  } else if (k === 'tool-serial') {
    form.type = 'tool'
    form.tracking_mode = 'serialized'
  } else {
    form.type = 'tool'
    form.tracking_mode = 'quantity'
  }
}

const form = reactive<Partial<ItemRecord>>({
  code: '',
  name: '',
  type: 'tool',
  unit: 'each',
  tracking_mode: 'quantity',
  category: '',
  active: true,
  notes: '',
  quantity_on_hand: 0,
  reorder_threshold: 0,
})

const kind = ref<Kind>('tool-qty')

watch(
  () => props.open,
  (open) => {
    if (!open) return
    Object.assign(form, {
      code: '',
      name: '',
      type: 'tool',
      unit: 'each',
      tracking_mode: 'quantity',
      category: '',
      active: true,
      notes: '',
      quantity_on_hand: 0,
      reorder_threshold: 0,
      ...(props.item ?? {}),
    })
    kind.value = kindFromItem(props.item)
  },
  { immediate: true },
)

watch(kind, (k) => applyKindToForm(k))

const isEdit = computed(() => !!props.item?.id)
const isSerialized = computed(() => kind.value === 'tool-serial')
const isConsumable = computed(() => kind.value === 'consumable')

const adjustOpen = ref(false)
const historyOpen = ref(false)

// "Stocked at" — controller-only read-out of the kiosks that carry this SKU.
// Source of truth is the per-kiosk kiosk_items panel; this view is just the
// inverse projection so admins can answer "where does this item live?"
// without flipping back to the kiosks tab.
const stockedAt = ref<KioskRecord[]>([])
const stockedAtLoading = ref(false)

async function loadStockedAt(itemId: string) {
  stockedAtLoading.value = true
  try {
    const rows = await pb.collection('kiosk_items').getFullList<{
      expand?: { kiosk?: KioskRecord }
    }>({
      filter: `item = "${itemId}"`,
      expand: 'kiosk',
      sort: '+created',
    })
    stockedAt.value = rows
      .map((r) => r.expand?.kiosk)
      .filter((k): k is KioskRecord => !!k)
  } catch {
    stockedAt.value = []
  } finally {
    stockedAtLoading.value = false
  }
}

watch(
  () => [props.open, props.isController, props.item?.id],
  ([open, isCtrl, id]) => {
    if (open && isCtrl && typeof id === 'string' && id) {
      void loadStockedAt(id)
    } else {
      stockedAt.value = []
    }
  },
  { immediate: true },
)

function onSubmit() {
  emit('save', { ...form })
}

function onAdjusted(result: StockAdjustmentResult) {
  form.quantity_on_hand = result.new_quantity
}
</script>

<template>
  <AppDialog
    :open="open"
    variant="sheet"
    :title="isEdit ? 'Edit item' : 'New item'"
    :size="isSerialized && isEdit ? 'lg' : 'md'"
    @update:open="emit('update:open', $event)"
  >
    <form class="flex flex-col gap-4" @submit.prevent="onSubmit">
      <div class="grid grid-cols-2 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Code</span>
          <input
            v-model="form.code"
            type="text"
            required
            :disabled="managed"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Name</span>
          <input
            v-model="form.name"
            type="text"
            required
            :disabled="managed"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
          />
        </label>
      </div>

      <div class="grid grid-cols-2 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Kind</span>
          <select
            v-model="kind"
            :disabled="managed"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
          >
            <option value="tool-qty">Tool — quantity tracked</option>
            <option value="tool-serial">Tool — serialized</option>
            <option value="consumable">Consumable</option>
          </select>
          <span class="text-xs text-slate-500">
            {{ isSerialized
              ? 'Each unit has its own identity (per-unit code, serial, RFID).'
              : isConsumable
                ? 'Depletes when consumed — no checkout/return.'
                : 'A pool of interchangeable units, tracked by quantity.' }}
          </span>
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Unit</span>
          <input
            v-model="form.unit"
            type="text"
            :disabled="managed"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
          />
        </label>
      </div>

      <!-- For serialized items, serial and RFID live on each instance instead
           of the SKU itself. Hide the dialog-level inputs and surface the
           instances panel below once the item exists. -->
      <label v-if="isSerialized && !isEdit" class="rounded-lg bg-slate-950/40 border border-dashed border-slate-700 px-3 py-2 text-xs text-slate-400">
        Save the item first, then add its serialized instances (per-unit
        code, serial, RFID) in the panel that appears below.
      </label>

      <div v-if="!isController" class="grid grid-cols-2 gap-3">
        <div class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">
            {{ form.type === 'tool' ? 'Fleet quantity' : 'Quantity on hand' }}
          </span>
          <!-- In edit mode the quantity is read-only; use the Adjust button
               to change it so the audit log captures who / why. New items
               still take a free-form number for the initial seed. -->
          <div v-if="isEdit" class="flex items-center gap-2">
            <span class="rounded-lg bg-slate-950 border border-slate-800 px-3 py-2 text-slate-100 font-medium tabular-nums flex-1">
              {{ form.quantity_on_hand ?? 0 }}
            </span>
            <button
              type="button"
              class="px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm whitespace-nowrap"
              @click="adjustOpen = true"
            >
              Adjust…
            </button>
          </div>
          <input
            v-else
            v-model.number="form.quantity_on_hand"
            type="number"
            step="1"
            min="0"
            :disabled="managed"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
          />
          <span class="text-xs text-slate-500">
            {{ form.type === 'tool'
              ? 'Total units owned. Does not change on checkout/return.'
              : 'Current stock. Decrements automatically when consumed.' }}
            <button
              v-if="isEdit"
              type="button"
              class="ml-1 text-sky-400 hover:text-sky-300 underline-offset-2 hover:underline"
              @click="historyOpen = true"
            >
              View adjustment history
            </button>
          </span>
        </div>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Reorder threshold</span>
          <input
            v-model.number="form.reorder_threshold"
            type="number"
            step="1"
            min="0"
            :disabled="managed"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
          />
          <span class="text-xs text-slate-500">Alert when available at or below this. 0 = no alert.</span>
        </label>
      </div>

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Category</span>
        <input
          v-model="form.category"
          type="text"
          placeholder="e.g. Power Tools"
          :disabled="managed"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
        />
      </label>

      <ItemInstancesPanel
        v-if="!isController && isSerialized && isEdit && form.id"
        :item-id="form.id"
      />

      <section
        v-if="isController && isEdit && form.id"
        class="rounded-xl bg-slate-950/40 border border-slate-800 p-4"
      >
        <header class="mb-2">
          <h3 class="text-sm font-medium text-slate-200">Stocked at</h3>
          <p class="text-xs text-slate-500">
            Kiosks carrying this SKU. Edit a kiosk's "Stocked items" panel to
            add or remove.
          </p>
        </header>
        <p v-if="stockedAtLoading" class="text-xs text-slate-500">Loading…</p>
        <p v-else-if="stockedAt.length === 0" class="text-xs text-slate-500">
          Not assigned to any kiosk yet.
        </p>
        <ul v-else class="flex flex-wrap gap-1.5">
          <li
            v-for="k in stockedAt"
            :key="k.id"
            class="font-mono text-xs px-2 py-0.5 rounded bg-slate-800 text-slate-200"
            :title="k.location_code ? `${k.kiosk_code} — ${k.location_code}` : k.kiosk_code"
          >
            {{ k.kiosk_code }}
          </li>
        </ul>
      </section>

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Notes</span>
        <textarea
          v-model="form.notes"
          rows="2"
          :disabled="managed"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 resize-none disabled:opacity-60 disabled:cursor-not-allowed"
        ></textarea>
      </label>

      <label class="flex items-center gap-2 text-slate-300">
        <input v-model="form.active" type="checkbox" :disabled="managed" class="w-4 h-4 disabled:opacity-60" />
        Active
      </label>

      <div class="flex justify-end gap-3 mt-2">
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          @click="emit('update:open', false)"
        >
          {{ managed ? 'Close' : 'Cancel' }}
        </button>
        <button
          v-if="!managed"
          type="submit"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        >
          {{ isEdit ? 'Save changes' : 'Create item' }}
        </button>
      </div>
    </form>
  </AppDialog>

  <StockAdjustDialog
    v-if="!isController && isEdit && form.id"
    :open="adjustOpen"
    :item-id="form.id"
    :item-code="form.code ?? ''"
    :item-name="form.name ?? ''"
    :current-qty="form.quantity_on_hand ?? 0"
    @update:open="adjustOpen = $event"
    @applied="onAdjusted"
  />
  <StockAdjustmentHistoryDialog
    v-if="!isController && isEdit && form.id"
    :open="historyOpen"
    :item-id="form.id"
    :item-code="form.code ?? ''"
    @update:open="historyOpen = $event"
  />
</template>
