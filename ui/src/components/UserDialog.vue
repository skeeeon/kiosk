<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import type { GroupRecord, WorkerRecord } from '../types'

const props = withDefaults(
  defineProps<{
    open: boolean
    user: Partial<WorkerRecord> | null
    managed?: boolean
    groups?: GroupRecord[]
  }>(),
  { managed: false, groups: () => [] },
)

const emit = defineEmits<{
  'update:open': [value: boolean]
  save: [data: Partial<WorkerRecord>]
  'save-and-add-another': [data: Partial<WorkerRecord>]
  'create-group': []
  // Edit mode only. The host closes this sheet and runs its own delete
  // confirmation (which preserves the friendly FK-constraint message).
  delete: []
}>()

const form = reactive<Partial<WorkerRecord>>({
  code: '',
  name: '',
  email: '',
  phone: '',
  role: 'worker',
  group: '',
  active: true,
})

const initialSnapshot = ref('')

watch(
  () => [props.open, props.user] as const,
  ([open]) => {
    if (!open) return
    Object.assign(form, {
      code: '',
      name: '',
      email: '',
      phone: '',
      role: 'worker',
      group: '',
      active: true,
      ...(props.user ?? {}),
    })
    initialSnapshot.value = JSON.stringify(form)
  },
  { immediate: true },
)

const isEdit = computed(() => !!props.user?.id)
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
    :title="isEdit ? 'Edit worker' : 'New worker'"
    :description="isEdit ? undefined : 'Workers identify by badge scan; passwords are auto-generated and unused in v1.'"
    confirm-discard
    :dirty="dirty && !managed"
    @update:open="emit('update:open', $event)"
  >
    <form class="flex flex-col gap-4" @submit.prevent="onSubmit">
      <div class="grid grid-cols-2 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Badge code</span>
          <input
            v-model="form.code"
            type="text"
            required
            placeholder="EMP-4042"
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
          <span class="text-sm text-slate-400">Email</span>
          <input
            v-model="form.email"
            type="email"
            required
            :disabled="managed"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Phone <span class="text-slate-500">(optional)</span></span>
          <input
            v-model="form.phone"
            type="tel"
            placeholder="+1-555-0100"
            :disabled="managed"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
          />
        </label>
      </div>

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Role</span>
        <select
          v-model="form.role"
          :disabled="managed"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
        >
          <option value="worker">Worker</option>
          <option value="foreman">Foreman</option>
        </select>
      </label>

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Group <span class="text-slate-500">(optional)</span></span>
        <div class="flex gap-2">
          <select
            v-model="form.group"
            :disabled="managed"
            class="flex-1 rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 disabled:opacity-60 disabled:cursor-not-allowed"
          >
            <option value="">— Ungrouped —</option>
            <option v-for="g in groups" :key="g.id" :value="g.id">
              {{ g.code }}{{ g.name && g.name !== g.code ? ` — ${g.name}` : '' }}
            </option>
          </select>
          <button
            v-if="!managed"
            type="button"
            class="px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
            @click="emit('create-group')"
          >
            + New
          </button>
        </div>
        <span class="text-xs text-slate-500">
          Foremen can only return tools across users within their own group. Leave ungrouped for the strictest setting.
        </span>
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
          {{ isEdit ? 'Save changes' : 'Create worker' }}
        </button>
      </div>
    </form>
  </AppDialog>
</template>
