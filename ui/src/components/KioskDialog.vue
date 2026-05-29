<!-- KioskDialog is now a thin create-only modal. Editing (notes/location/status)
     and the stocked-items panel live on AdminKioskDetailView (/admin/kiosks/:code);
     this dialog is what AdminKiosksView's "New kiosk" button opens to
     pre-register a kiosk before it phones home. -->
<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import type { KioskRecord } from '../types'

const props = defineProps<{
  open: boolean
  kiosk: Partial<KioskRecord> | null
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  save: [data: Partial<KioskRecord>]
  'save-and-add-another': [data: Partial<KioskRecord>]
}>()

const form = reactive<Partial<KioskRecord>>({
  kiosk_code: '',
  location_code: '',
  status: 'unknown',
  notes: '',
})

const initialSnapshot = ref('')

watch(
  () => [props.open, props.kiosk] as const,
  ([open]) => {
    if (!open) return
    Object.assign(form, {
      kiosk_code: '',
      location_code: '',
      status: 'unknown',
      notes: '',
      ...(props.kiosk ?? {}),
    })
    initialSnapshot.value = JSON.stringify(form)
  },
  { immediate: true },
)

const dirty = computed(() => JSON.stringify(form) !== initialSnapshot.value)

function buildPayload(): Partial<KioskRecord> {
  return {
    kiosk_code: form.kiosk_code,
    location_code: form.location_code,
    status: form.status,
    notes: form.notes,
  }
}

function onSubmit() {
  // Create path only. Status defaults to unknown unless the admin
  // changed it. Editing of an existing kiosk now happens on the detail
  // view, which has room for the full set of tabs.
  emit('save', buildPayload())
}

function onSubmitAndAdd() {
  emit('save-and-add-another', buildPayload())
}
</script>

<template>
  <AppDialog
    :open="open"
    variant="sheet"
    title="New kiosk"
    size="md"
    description="Pre-register a kiosk so you can assign items to it before it phones home. Kiosks also self-register on first event; that path is a no-op when the row already exists."
    confirm-discard
    :dirty="dirty"
    @update:open="emit('update:open', $event)"
  >
    <form class="flex flex-col gap-4" @submit.prevent="onSubmit">
      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Kiosk code</span>
        <input
          v-model="form.kiosk_code"
          type="text"
          required
          placeholder="e.g. KIOSK01"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 font-mono text-sm"
        />
      </label>

      <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Location code</span>
          <input
            v-model="form.location_code"
            type="text"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Status</span>
          <select
            v-model="form.status"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          >
            <option value="unknown">Unknown</option>
            <option value="active">Active</option>
            <option value="disabled">Disabled</option>
          </select>
        </label>
      </div>

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Notes</span>
        <textarea
          v-model="form.notes"
          rows="3"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 resize-none"
        ></textarea>
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
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-700 hover:bg-slate-600 text-slate-100 text-sm font-medium"
          @click="onSubmitAndAdd"
        >
          Save &amp; add another
        </button>
        <button
          type="submit"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        >
          Create kiosk
        </button>
      </div>
    </form>
  </AppDialog>
</template>
