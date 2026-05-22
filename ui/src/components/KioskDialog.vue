<script setup lang="ts">
import { computed, reactive, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import KioskItemsPanel from './KioskItemsPanel.vue'
import type { KioskRecord } from '../types'

const props = defineProps<{
  open: boolean
  kiosk: Partial<KioskRecord> | null
  // Controller-side only — gates the stocked-items panel.
  isController?: boolean
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

const isEdit = computed(() => !!props.kiosk?.id)

const lastSeenDisplay = computed(() => {
  const v = props.kiosk?.last_seen
  if (!v) return 'never'
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return v
  return d.toLocaleString()
})

function onSubmit() {
  if (isEdit.value) {
    // kiosk_code is identity once persisted — never edit. Send only the
    // editable subset so PB doesn't see a unique-index update with the
    // same value.
    emit('save', {
      id: form.id,
      location_code: form.location_code,
      status: form.status,
      notes: form.notes,
    })
  } else {
    // On create, kiosk_code is required and writable. Status defaults to
    // unknown unless the admin changed it.
    emit('save', {
      kiosk_code: form.kiosk_code,
      location_code: form.location_code,
      status: form.status,
      notes: form.notes,
    })
  }
}
</script>

<template>
  <AppDialog
    :open="open"
    :title="isEdit ? `Kiosk ${form.kiosk_code ?? ''}` : 'New kiosk'"
    :size="isController && isEdit ? 'lg' : 'md'"
    :description="isEdit
      ? 'Editing the central record. The kiosk\'s own kiosk.yaml is independent — change there for fields the kiosk owns (port, branding, etc.).'
      : 'Pre-register a kiosk so you can assign items to it before it phones home. Kiosks also self-register on first event; that path is a no-op when the row already exists.'"
    @update:open="emit('update:open', $event)"
  >
    <form class="flex flex-col gap-4" @submit.prevent="onSubmit">
      <div class="grid grid-cols-2 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Kiosk code</span>
          <input
            v-if="!isEdit"
            v-model="form.kiosk_code"
            type="text"
            required
            placeholder="e.g. KIOSK01"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 font-mono text-sm"
          />
          <span
            v-else
            class="rounded-lg bg-slate-950 border border-slate-800 px-3 py-2 text-slate-100 font-mono text-sm"
          >
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

      <KioskItemsPanel
        v-if="isController && isEdit && form.id"
        :kiosk-id="form.id"
      />

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
          {{ isEdit ? 'Save changes' : 'Create kiosk' }}
        </button>
      </div>
    </form>
  </AppDialog>
</template>
