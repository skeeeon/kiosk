<!-- AppDialog wraps Reka's Dialog primitives with our base styling.
     Reka handles focus trap, ESC-to-close, and accessible labeling.

     Two variants:
       - 'modal' (default): centered overlay, suitable for confirmations and
         one-shot prompts.
       - 'sheet': right-anchored side panel, used for edit forms and drilldown
         detail views. The list view stays visible behind it, which both keeps
         operators oriented and works well on tablet/phone.

     `size` controls width. Defaults to 'md'. For sheets, the values map to a
     fixed-width panel; for modals, to max-width caps. -->
<script setup lang="ts">
import { computed } from 'vue'
import {
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogOverlay,
  DialogPortal,
  DialogRoot,
  DialogTitle,
} from 'reka-ui'

const props = withDefaults(
  defineProps<{
    open: boolean
    title?: string
    description?: string
    size?: 'sm' | 'md' | 'lg'
    variant?: 'modal' | 'sheet'
  }>(),
  { size: 'md', variant: 'modal' },
)
const emit = defineEmits<{ 'update:open': [value: boolean] }>()

const modalSizeClass = computed(() => {
  switch (props.size) {
    case 'sm':
      return 'max-w-md'
    case 'lg':
      return 'max-w-4xl'
    default:
      return 'max-w-2xl'
  }
})

const sheetSizeClass = computed(() => {
  switch (props.size) {
    case 'sm':
      return 'w-[400px]'
    case 'lg':
      return 'w-[760px]'
    default:
      return 'w-[560px]'
  }
})
</script>

<template>
  <DialogRoot :open="open" @update:open="emit('update:open', $event)">
    <DialogPortal>
      <DialogOverlay
        class="fixed inset-0 bg-black/70 backdrop-blur-sm z-30 data-[state=open]:animate-in data-[state=closed]:animate-out"
      />

      <!-- Modal variant -->
      <DialogContent
        v-if="variant === 'modal'"
        :class="[
          'fixed top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2 w-[92vw] max-h-[90vh] flex flex-col overflow-y-auto bg-slate-900 rounded-2xl border border-slate-800 p-6 z-40 shadow-2xl',
          modalSizeClass,
        ]"
      >
        <DialogTitle v-if="title" class="text-2xl font-semibold text-slate-100">{{ title }}</DialogTitle>
        <DialogDescription v-if="description" class="text-slate-400 text-sm mt-1 mb-4">
          {{ description }}
        </DialogDescription>
        <slot />
      </DialogContent>

      <!-- Sheet variant -->
      <DialogContent
        v-else
        :class="[
          'fixed inset-y-0 right-0 h-full max-w-[92vw] flex flex-col bg-slate-900 border-l border-slate-800 z-40 shadow-2xl focus:outline-none',
          sheetSizeClass,
        ]"
      >
        <div
          v-if="title || description"
          class="flex items-start justify-between gap-3 px-6 py-4 border-b border-slate-800 shrink-0"
        >
          <div class="min-w-0">
            <DialogTitle v-if="title" class="text-xl font-semibold text-slate-100 truncate">
              {{ title }}
            </DialogTitle>
            <DialogDescription v-if="description" class="text-slate-400 text-sm mt-1">
              {{ description }}
            </DialogDescription>
          </div>
          <DialogClose
            class="shrink-0 flex items-center justify-center h-8 w-8 rounded-md text-slate-400 hover:bg-slate-800 hover:text-slate-200 focus:outline-none focus-visible:ring-2 focus-visible:ring-slate-500"
            aria-label="Close"
          >
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="h-4 w-4">
              <path d="M6.28 5.22a.75.75 0 0 0-1.06 1.06L8.94 10l-3.72 3.72a.75.75 0 1 0 1.06 1.06L10 11.06l3.72 3.72a.75.75 0 1 0 1.06-1.06L11.06 10l3.72-3.72a.75.75 0 0 0-1.06-1.06L10 8.94 6.28 5.22Z" />
            </svg>
          </DialogClose>
        </div>
        <div class="flex-1 overflow-y-auto px-6 py-5">
          <slot />
        </div>
      </DialogContent>
    </DialogPortal>
  </DialogRoot>
</template>
