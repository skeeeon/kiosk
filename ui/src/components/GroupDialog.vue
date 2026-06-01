<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import type { GroupRecord } from '../types'

const props = withDefaults(
  defineProps<{
    open: boolean
    group: Partial<GroupRecord> | null
    managed?: boolean
  }>(),
  { managed: false },
)

const emit = defineEmits<{
  'update:open': [value: boolean]
  save: [data: Partial<GroupRecord>]
  'save-and-add-another': [data: Partial<GroupRecord>]
  // Edit mode only. The host closes this sheet and runs its own delete
  // confirmation.
  delete: []
}>()

const form = reactive<Partial<GroupRecord>>({
  code: '',
  name: '',
  contact_email: '',
  contact_phone: '',
  notes: '',
  active: true,
})

const initialSnapshot = ref('')

watch(
  () => [props.open, props.group] as const,
  ([open]) => {
    if (!open) return
    Object.assign(form, {
      code: '',
      name: '',
      contact_email: '',
      contact_phone: '',
      notes: '',
      active: true,
      ...(props.group ?? {}),
    })
    initialSnapshot.value = JSON.stringify(form)
  },
  { immediate: true },
)

const isEdit = computed(() => !!props.group?.id)
const dirty = computed(() => JSON.stringify(form) !== initialSnapshot.value)

function onSubmit() {
  emit('save', { ...form })
}

function onSubmitAndAdd() {
  emit('save-and-add-another', { ...form })
}
</script>

<template>
  <AppDialog
    :open="open"
    variant="sheet"
    :title="isEdit ? 'Edit group' : 'New group'"
    description="Groups associate workers with a sub-contractor or trade. The contact email receives copies of receipts and (later) digest emails for all workers in the group."
    confirm-discard
    :dirty="dirty && !managed"
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
            placeholder="ACME"
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
            placeholder="Acme Subcontracting"
            :disabled="managed"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
          />
        </label>
      </div>

      <div class="grid grid-cols-2 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Contact email <span class="text-slate-500">(optional)</span></span>
          <input
            v-model="form.contact_email"
            type="email"
            placeholder="foreman@acme.example"
            :disabled="managed"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Contact phone <span class="text-slate-500">(optional)</span></span>
          <input
            v-model="form.contact_phone"
            type="tel"
            placeholder="+1-555-0100"
            :disabled="managed"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
          />
        </label>
      </div>

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Notes <span class="text-slate-500">(optional)</span></span>
        <textarea
          v-model="form.notes"
          rows="3"
          placeholder="Contract reference, billing terms, anything operational."
          :disabled="managed"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
        />
      </label>

      <label class="flex items-center gap-2 text-slate-300">
        <input v-model="form.active" type="checkbox" :disabled="managed" class="w-4 h-4 disabled:opacity-60" />
        Active
      </label>

      <div class="flex justify-end gap-3 mt-2">
        <button
          v-if="!managed && isEdit"
          type="button"
          class="mr-auto px-4 py-2 rounded-lg bg-red-950/60 hover:bg-red-900/60 text-red-200 border border-red-800/70 text-sm font-medium"
          @click="emit('delete')"
        >
          Delete
        </button>
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          @click="emit('update:open', false)"
        >
          {{ managed ? 'Close' : 'Cancel' }}
        </button>
        <button
          v-if="!managed && !isEdit"
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-700 hover:bg-slate-600 text-slate-100 text-sm font-medium"
          @click="onSubmitAndAdd"
        >
          Save &amp; add another
        </button>
        <button
          v-if="!managed"
          type="submit"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        >
          {{ isEdit ? 'Save changes' : 'Create group' }}
        </button>
      </div>
    </form>
  </AppDialog>
</template>
