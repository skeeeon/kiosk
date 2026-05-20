<script setup lang="ts">
import AppDialog from './AppDialog.vue'

defineProps<{
  open: boolean
  title: string
  message: string
  confirmLabel?: string
  cancelLabel?: string
  destructive?: boolean
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  confirm: []
}>()
</script>

<template>
  <AppDialog
    :open="open"
    :title="title"
    @update:open="emit('update:open', $event)"
  >
    <p class="text-slate-300 mb-6">{{ message }}</p>
    <div class="flex justify-end gap-3">
      <button
        type="button"
        class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
        @click="emit('update:open', false)"
      >
        {{ cancelLabel ?? 'Cancel' }}
      </button>
      <button
        type="button"
        class="px-4 py-2 rounded-lg font-medium text-white"
        :class="destructive ? 'bg-red-600 hover:bg-red-500' : 'bg-emerald-600 hover:bg-emerald-500'"
        @click="emit('confirm')"
      >
        {{ confirmLabel ?? 'Confirm' }}
      </button>
    </div>
  </AppDialog>
</template>
