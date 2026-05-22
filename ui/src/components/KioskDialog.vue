<script setup lang="ts">
import { computed, reactive, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import type { KioskRecord } from '../types'

const props = defineProps<{
  open: boolean
  kiosk: Partial<KioskRecord> | null
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  save: [data: Partial<KioskRecord>]
}>()

const form = reactive<Partial<KioskRecord>>({
  kiosk_code: '',
  location_code: '',
  status: 'unknown',
  notes: '',
})

watch(
  () => props.open,
  (open) => {
    if (!open) return
    Object.assign(form, {
      kiosk_code: '',
      location_code: '',
      status: 'unknown',
      notes: '',
      ...(props.kiosk ?? {}),
    })
  },
  { immediate: true },
)

const lastSeenDisplay = computed(() => {
  const v = props.kiosk?.last_seen
  if (!v) return 'never'
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return v
  return d.toLocaleString()
})

function onSubmit() {
  // kiosk_code is identity — never edit. Send only the editable subset back
  // so PB doesn't see a unique-index update with the same value.
  emit('save', {
    id: form.id,
    location_code: form.location_code,
    status: form.status,
    notes: form.notes,
  })
}
</script>

<template>
  <AppDialog
    :open="open"
    :title="`Kiosk ${form.kiosk_code ?? ''}`"
    description="Kiosks register themselves automatically the first time they publish an event. Editing here updates the central record; the kiosk's own config (kiosk.yaml) is independent."
    @update:open="emit('update:open', $event)"
  >
    <form class="flex flex-col gap-4" @submit.prevent="onSubmit">
      <div class="grid grid-cols-2 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Kiosk code</span>
          <span class="rounded-lg bg-slate-950 border border-slate-800 px-3 py-2 text-slate-100 font-mono text-sm">
            {{ form.kiosk_code || '—' }}
          </span>
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Last seen</span>
          <span class="rounded-lg bg-slate-950 border border-slate-800 px-3 py-2 text-slate-300 text-sm">
            {{ lastSeenDisplay }}
          </span>
        </label>
      </div>

      <div class="grid grid-cols-2 gap-3">
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
          type="submit"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        >
          Save changes
        </button>
      </div>
    </form>
  </AppDialog>
</template>
