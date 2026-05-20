<!-- AppDialog wraps Reka's Dialog primitives with our base styling.
     Reka handles focus trap, ESC-to-close, and accessible labeling. -->
<script setup lang="ts">
import {
  DialogContent,
  DialogDescription,
  DialogOverlay,
  DialogPortal,
  DialogRoot,
  DialogTitle,
} from 'reka-ui'

defineProps<{
  open: boolean
  title?: string
  description?: string
}>()
const emit = defineEmits<{ 'update:open': [value: boolean] }>()
</script>

<template>
  <DialogRoot :open="open" @update:open="emit('update:open', $event)">
    <DialogPortal>
      <DialogOverlay
        class="fixed inset-0 bg-black/70 backdrop-blur-sm z-30 data-[state=open]:animate-in data-[state=closed]:animate-out"
      />
      <DialogContent
        class="fixed top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2 w-[92vw] max-w-xl max-h-[90vh] overflow-y-auto bg-slate-900 rounded-2xl border border-slate-800 p-6 z-40 shadow-2xl"
      >
        <DialogTitle v-if="title" class="text-2xl font-semibold text-slate-100">{{ title }}</DialogTitle>
        <DialogDescription v-if="description" class="text-slate-400 text-sm mt-1 mb-4">
          {{ description }}
        </DialogDescription>
        <slot />
      </DialogContent>
    </DialogPortal>
  </DialogRoot>
</template>
