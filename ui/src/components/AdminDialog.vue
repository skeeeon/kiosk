<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import type { AdminRecord } from '../types'

const props = defineProps<{
  open: boolean
  admin: Partial<AdminRecord> | null
  isSelf?: boolean
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  save: [data: Partial<AdminRecord>]
  'save-and-add-another': [data: Partial<AdminRecord>]
}>()

const form = reactive<Partial<AdminRecord>>({
  email: '',
  name: '',
  active: true,
})

const initialSnapshot = ref('')

watch(
  () => [props.open, props.admin] as const,
  ([open]) => {
    if (!open) return
    Object.assign(form, {
      email: '',
      name: '',
      active: true,
      ...(props.admin ?? {}),
    })
    initialSnapshot.value = JSON.stringify(form)
  },
  { immediate: true },
)

const isEdit = computed(() => !!props.admin?.id)
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
    :title="isEdit ? 'Edit admin' : 'New admin'"
    :description="isEdit
      ? 'Password changes go through the &quot;Forgot password&quot; flow on the admin login screen.'
      : 'A random password is generated on save and shown to you once — capture it before closing the confirmation.'"
    confirm-discard
    :dirty="dirty"
    @update:open="emit('update:open', $event)"
  >
    <form class="flex flex-col gap-4" @submit.prevent="onSubmit">
      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Email</span>
        <input
          v-model="form.email"
          type="email"
          required
          placeholder="ops@example.com"
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

      <label class="flex items-center gap-2 text-slate-300">
        <input v-model="form.active" type="checkbox" class="w-4 h-4" />
        Active
        <span v-if="isSelf && form.active === false" class="text-xs text-amber-400 ml-2">
          Deactivating yourself will log you out on next request.
        </span>
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
          v-if="!isEdit"
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
          {{ isEdit ? 'Save changes' : 'Create admin' }}
        </button>
      </div>
    </form>
  </AppDialog>
</template>
