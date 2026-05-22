<script setup lang="ts">
import { computed, reactive, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import type { WorkerRecord } from '../types'

const props = withDefaults(
  defineProps<{
    open: boolean
    user: Partial<WorkerRecord> | null
    managed?: boolean
  }>(),
  { managed: false },
)

const emit = defineEmits<{
  'update:open': [value: boolean]
  save: [data: Partial<WorkerRecord>]
}>()

const form = reactive<Partial<WorkerRecord>>({
  code: '',
  name: '',
  email: '',
  role: 'worker',
  active: true,
})

watch(
  () => props.open,
  (open) => {
    if (!open) return
    Object.assign(form, {
      code: '',
      name: '',
      email: '',
      role: 'worker',
      active: true,
      ...(props.user ?? {}),
    })
  },
  { immediate: true },
)

const isEdit = computed(() => !!props.user?.id)

function onSubmit() {
  emit('save', { ...form })
}
</script>

<template>
  <AppDialog
    :open="open"
    :title="isEdit ? 'Edit worker' : 'New worker'"
    :description="isEdit ? undefined : 'Workers identify by badge scan; passwords are auto-generated and unused in v1.'"
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
      </div>

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
          {{ isEdit ? 'Save changes' : 'Create worker' }}
        </button>
      </div>
    </form>
  </AppDialog>
</template>
