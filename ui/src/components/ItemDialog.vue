<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import ItemInstancesPanel from './ItemInstancesPanel.vue'
import StockAdjustDialog from './StockAdjustDialog.vue'
import StockAdjustmentHistoryDialog from './StockAdjustmentHistoryDialog.vue'
import type { ItemRecord, StockAdjustmentResult } from '../types'

const props = defineProps<{
  open: boolean
  item: Partial<ItemRecord> | null
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  save: [data: Partial<ItemRecord>]
}>()

const form = reactive<Partial<ItemRecord>>({
  code: '',
  name: '',
  type: 'tool',
  unit: 'each',
  tracking_mode: 'quantity',
  serial: '',
  category: '',
  rfid_epc: '',
  active: true,
  notes: '',
  quantity_on_hand: 0,
  reorder_threshold: 0,
})

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
      serial: '',
      category: '',
      rfid_epc: '',
      active: true,
      notes: '',
      quantity_on_hand: 0,
      reorder_threshold: 0,
      ...(props.item ?? {}),
    })
  },
  { immediate: true },
)

const isEdit = computed(() => !!props.item?.id)
const isSerialized = computed(() => form.tracking_mode === 'serialized')

const adjustOpen = ref(false)
const historyOpen = ref(false)

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
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Name</span>
          <input
            v-model="form.name"
            type="text"
            required
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
      </div>

      <div class="grid grid-cols-3 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Type</span>
          <select
            v-model="form.type"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          >
            <option value="tool">Tool</option>
            <option value="consumable">Consumable</option>
          </select>
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Tracking</span>
          <select
            v-model="form.tracking_mode"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          >
            <option value="quantity">Quantity</option>
            <option value="serialized">Serialized</option>
          </select>
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Unit</span>
          <input
            v-model="form.unit"
            type="text"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
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

      <div class="grid grid-cols-2 gap-3">
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
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
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
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
          <span class="text-xs text-slate-500">Alert when available at or below this. 0 = no alert.</span>
        </label>
      </div>

      <div class="grid grid-cols-2 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Category</span>
          <input
            v-model="form.category"
            type="text"
            placeholder="e.g. Power Tools"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
        <label v-if="!isSerialized" class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">RFID EPC</span>
          <input
            v-model="form.rfid_epc"
            type="text"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
      </div>

      <ItemInstancesPanel
        v-if="isSerialized && isEdit && form.id"
        :item-id="form.id"
      />

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Notes</span>
        <textarea
          v-model="form.notes"
          rows="2"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 resize-none"
        ></textarea>
      </label>

      <label class="flex items-center gap-2 text-slate-300">
        <input v-model="form.active" type="checkbox" class="w-4 h-4" />
        Active
      </label>

      <div class="flex justify-end gap-3 mt-2">
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          @click="emit('update:open', false)"
        >
          Cancel
        </button>
        <button
          type="submit"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        >
          {{ isEdit ? 'Save changes' : 'Create item' }}
        </button>
      </div>
    </form>
  </AppDialog>

  <StockAdjustDialog
    v-if="isEdit && form.id"
    :open="adjustOpen"
    :item-id="form.id"
    :item-code="form.code ?? ''"
    :item-name="form.name ?? ''"
    :current-qty="form.quantity_on_hand ?? 0"
    @update:open="adjustOpen = $event"
    @applied="onAdjusted"
  />
  <StockAdjustmentHistoryDialog
    v-if="isEdit && form.id"
    :open="historyOpen"
    :item-id="form.id"
    :item-code="form.code ?? ''"
    @update:open="historyOpen = $event"
  />
</template>
