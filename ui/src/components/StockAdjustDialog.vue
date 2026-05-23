<!-- StockAdjustDialog records an inventory adjustment with an audit trail.
     Two modes: delta (+/- N) and absolute (set on-hand to N). Both write
     through /api/kiosk/items/{id}/adjust which logs the change to
     stock_adjustments inside the same transaction as the item update. -->
<script setup lang="ts">
import { computed, reactive, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import { api } from '../lib/api'
import { useAdminToast } from '../composables/useAdminToast'
import type { StockAdjustmentResult } from '../types'

const props = defineProps<{
  open: boolean
  itemId: string
  itemCode: string
  itemName: string
  currentQty: number
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  applied: [result: StockAdjustmentResult]
}>()

const toast = useAdminToast()

const form = reactive<{
  mode: 'delta' | 'absolute'
  value: number
  reason: string
}>({
  mode: 'delta',
  value: 0,
  reason: '',
})

const submitting = computed(() => false) // kept for future async UX; useful as a guard
let inFlight = false

watch(
  () => props.open,
  (open) => {
    if (open) {
      form.mode = 'delta'
      form.value = 0
      form.reason = ''
    }
  },
)

// Show the resulting quantity inline so admin sees what they're about to commit.
const projected = computed(() =>
  form.mode === 'delta' ? props.currentQty + (form.value || 0) : (form.value || 0),
)

// Common reasons surface as one-tap chips above the textarea — most
// adjustments fall into a handful of buckets. Tapping a chip drops the
// label into the reason field; admin can append context (PO number,
// shelf location, etc.) before submitting.
const REASON_PRESETS = [
  'Restock from PO',
  'Physical count',
  'Damaged / broken',
  'Found extra',
] as const
function applyPreset(text: string) {
  form.reason = text
}

async function submit() {
  if (inFlight) return
  if (!form.reason.trim()) {
    toast.error('Reason is required')
    return
  }
  inFlight = true
  try {
    const result = await api.post<StockAdjustmentResult>(
      `/api/kiosk/items/${props.itemId}/adjust`,
      { mode: form.mode, value: form.value, reason: form.reason.trim() },
    )
    toast.success(
      `${props.itemCode}: ${result.prev_quantity} → ${result.new_quantity} (${result.delta >= 0 ? '+' : ''}${result.delta})`,
    )
    emit('applied', result)
    emit('update:open', false)
  } catch (e) {
    toast.error((e as Error).message)
  } finally {
    inFlight = false
  }
}
</script>

<template>
  <AppDialog
    :open="open"
    variant="sheet"
    title="Adjust stock"
    :description="`${itemName} (${itemCode}) — currently ${currentQty}`"
    @update:open="emit('update:open', $event)"
  >
    <form class="flex flex-col gap-4" @submit.prevent="submit">
      <div class="inline-flex rounded-xl overflow-hidden border border-slate-700 self-start">
        <button
          type="button"
          class="px-4 py-2 text-sm"
          :class="form.mode === 'delta' ? 'bg-sky-600 text-white' : 'bg-slate-800 text-slate-300 hover:bg-slate-700'"
          @click="form.mode = 'delta'"
        >
          Delta (+/−)
        </button>
        <button
          type="button"
          class="px-4 py-2 text-sm"
          :class="form.mode === 'absolute' ? 'bg-sky-600 text-white' : 'bg-slate-800 text-slate-300 hover:bg-slate-700'"
          @click="form.mode = 'absolute'"
        >
          Set absolute
        </button>
      </div>

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">
          {{ form.mode === 'delta' ? 'Change by' : 'Set quantity to' }}
        </span>
        <input
          v-model.number="form.value"
          type="number"
          step="1"
          required
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
        />
        <span class="text-xs text-slate-500">
          Result will be <span class="text-slate-300 font-medium">{{ projected }}</span>.
        </span>
      </label>

      <div class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Reason</span>
        <div class="flex flex-wrap gap-1.5">
          <button
            v-for="r in REASON_PRESETS"
            :key="r"
            type="button"
            class="px-2 py-1 rounded-md text-xs bg-slate-800 hover:bg-slate-700 text-slate-300 border border-slate-700"
            @click="applyPreset(r)"
          >
            {{ r }}
          </button>
        </div>
        <textarea
          v-model="form.reason"
          rows="2"
          required
          placeholder="e.g. restock from PO-42, found broken box, physical count"
          class="mt-1 rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 resize-none"
        ></textarea>
      </div>

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
          :disabled="submitting"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        >
          Apply adjustment
        </button>
      </div>
    </form>
  </AppDialog>
</template>
