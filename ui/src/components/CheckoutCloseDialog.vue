<!-- CheckoutCloseDialog records an admin force-close of an outstanding
     open_checkouts row. Two routes depending on binary:

       - kiosk binary: POST /api/kiosk/checkouts/by-line/{line_id}/close
       - controller binary: POST /api/controller/kiosks/{code}/checkouts/{line_id}/close

     Both paths go through commit.AdminClose on the kiosk side; the only
     difference is whether the call rides a NATS request/reply (controller)
     or hits the local handler directly (kiosk).

     The DTO id from /api/kiosk/reports/open-checkouts is the kiosk-side
     transaction_lines.id with an optional "-N" suffix for non-serialized
     qty>N rows (synthesized so the SPA gets unique row keys). The line id
     itself is the natural key for the close — we strip the suffix here. -->
<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import { api, ApiError } from '../lib/api'
import { useToast } from '../composables/useToast'

const props = defineProps<{
  open: boolean
  // The DTO id from the Aging report (line_id or line_id-N). We strip the
  // suffix to recover the bare transaction_line_id.
  rowId: string
  // Display fields for the modal header.
  itemName: string
  itemCode: string
  userName: string
  userCode: string
  serial: string
  // When isController is true, the request rides NATS; the kiosk_code is
  // required to target the right kiosk. On the kiosk binary it's ignored.
  isController: boolean
  kioskCode?: string
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  closed: []
}>()

const toast = useToast()

type ClosureReason = 'lost' | 'returned_offline' | 'damaged' | 'other'

interface FormState {
  reason: ClosureReason | ''
  notes: string
}

const form = reactive<FormState>({ reason: '', notes: '' })
const submitting = ref(false)

watch(
  () => props.open,
  (open) => {
    if (open) {
      form.reason = ''
      form.notes = ''
    }
  },
)

// REASON_OPTIONS mirrors the validClosureReasons map + reasonAffectsInventory
// in commit.AdminClose. Keep these in lockstep — a typo here surfaces only
// at submit time as a 400, and a misaligned `affectsInventory` flag would
// mislead the admin about what they're about to do.
//
// affectsInventory drives the consequence text below each radio. Today
// lost + damaged both decrement stock by 1 (and, for serialized items,
// flip the instance to active=false). returned_offline + other do not.
const REASON_OPTIONS: {
  value: ClosureReason
  label: string
  hint: string
  affectsInventory: boolean
}[] = [
  {
    value: 'returned_offline',
    label: 'Returned off-system',
    hint: "Worker brought it back but didn't scan. Stock unchanged.",
    affectsInventory: false,
  },
  {
    value: 'lost',
    label: 'Lost',
    hint: 'Item is gone. Reduces stock by 1.',
    affectsInventory: true,
  },
  {
    value: 'damaged',
    label: 'Damaged / written off',
    hint: 'Item is unusable. Reduces stock by 1.',
    affectsInventory: true,
  },
  {
    value: 'other',
    label: 'Other',
    hint: 'Stock unchanged. Describe in notes.',
    affectsInventory: false,
  },
]

// Show a one-liner near the submit button summarising what will happen.
// Pulled out so the messaging lives in one place rather than scattered
// across radio hints + a separate banner.
const consequenceSummary = computed(() => {
  if (!form.reason) return ''
  const opt = REASON_OPTIONS.find((o) => o.value === form.reason)
  if (!opt) return ''
  if (opt.affectsInventory) {
    const base = `Closes this checkout and reduces ${props.itemCode} stock by 1.`
    return props.serial ? `${base} Instance ${props.serial} will be retired.` : base
  }
  return 'Closes this checkout. Stock and inventory unchanged.'
})

// transactionLineID strips the "-N" suffix that the Aging report DTO
// synthesizes for non-serialized qty>1 rows so each unit has a unique id
// in the table.
const transactionLineID = computed(() => props.rowId.replace(/-\d+$/, ''))

async function submit() {
  if (submitting.value) return
  if (!form.reason) {
    toast.error('Closure reason is required')
    return
  }
  if (props.isController && !props.kioskCode) {
    toast.error('Kiosk code is required for controller-driven close')
    return
  }
  submitting.value = true
  try {
    const url = props.isController
      ? `/api/controller/kiosks/${encodeURIComponent(props.kioskCode!)}/checkouts/${encodeURIComponent(transactionLineID.value)}/close`
      : `/api/kiosk/checkouts/by-line/${encodeURIComponent(transactionLineID.value)}/close`
    await api.post(url, { reason: form.reason, notes: form.notes.trim() })
    toast.success(`Closed ${props.itemCode}${props.serial ? ` (${props.serial})` : ''}`)
    emit('closed')
    emit('update:open', false)
  } catch (e) {
    if (e instanceof ApiError && e.status === 503) {
      toast.error('Kiosk is offline — try again when it reconnects.')
    } else {
      toast.error((e as Error).message)
    }
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <AppDialog
    :open="open"
    variant="sheet"
    title="Close checkout"
    :description="`${itemName} (${itemCode}) — held by ${userName} (${userCode})`"
    @update:open="emit('update:open', $event)"
  >
    <form class="flex flex-col gap-4" @submit.prevent="submit">
      <p v-if="serial" class="text-xs text-slate-500">
        Serial: <span class="font-mono text-slate-300">{{ serial }}</span>
      </p>

      <fieldset class="flex flex-col gap-2">
        <legend class="text-sm text-slate-400 mb-1">Closure reason</legend>
        <label
          v-for="opt in REASON_OPTIONS"
          :key="opt.value"
          class="flex items-start gap-3 rounded-lg border border-slate-700 bg-slate-800/40 px-3 py-2 cursor-pointer hover:bg-slate-800/70"
          :class="form.reason === opt.value ? 'ring-1 ring-sky-500 border-sky-700' : ''"
        >
          <input
            v-model="form.reason"
            type="radio"
            :value="opt.value"
            class="mt-1"
          />
          <span class="flex-1">
            <span class="block text-sm text-slate-200">{{ opt.label }}</span>
            <span class="block text-xs text-slate-500">{{ opt.hint }}</span>
          </span>
        </label>
      </fieldset>

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Notes (optional)</span>
        <textarea
          v-model="form.notes"
          rows="2"
          placeholder="Context for the audit log."
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 resize-none"
        ></textarea>
      </label>

      <p
        v-if="consequenceSummary"
        class="rounded-lg bg-amber-950/30 border border-amber-900/60 text-xs text-amber-100 px-3 py-2"
      >
        {{ consequenceSummary }}
      </p>

      <p
        v-if="isController"
        class="rounded-lg bg-slate-950/40 border border-slate-800 text-xs text-slate-400 px-3 py-2"
      >
        This sends a NATS command to <span class="font-mono text-slate-300">{{ kioskCode }}</span>.
        The kiosk must be online for the close to land.
      </p>

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
          :disabled="submitting || !form.reason"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium disabled:opacity-60 disabled:cursor-not-allowed"
        >
          {{ submitting ? 'Closing…' : 'Close checkout' }}
        </button>
      </div>
    </form>
  </AppDialog>
</template>
