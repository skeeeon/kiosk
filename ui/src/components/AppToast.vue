<!-- AppToast is the singleton renderer for useToast. Mounted once in App.vue
     so any view (admin or kiosk) can fire toasts via the composable without
     prop-drilling. Position is per-call: 'bottom-right' for admin UX (default),
     'top' for kiosk touchscreen visibility. -->
<script setup lang="ts">
import { computed } from 'vue'
import { useToast } from '../composables/useToast'

const { current, dismiss } = useToast()

const TONE = {
  success: 'bg-emerald-900/80 border-emerald-700 text-emerald-100',
  error: 'bg-red-900/80 border-red-700 text-red-100',
  warn: 'bg-amber-900/80 border-amber-700 text-amber-100',
  info: 'bg-sky-900/80 border-sky-700 text-sky-100',
} as const

const POSITION = {
  'bottom-right': 'bottom-6 right-6',
  // Mirrors the previous CheckoutView flash placement so kiosk toasts stay
  // where operators already expect to look.
  top: 'top-16 left-1/2 -translate-x-1/2',
} as const

const positionClass = computed(() =>
  current.value ? POSITION[current.value.position] : '',
)
const toneClass = computed(() => (current.value ? TONE[current.value.kind] : ''))
// Kiosk toasts (top position) get the larger text the touchscreen UI was
// already using; admin toasts stay at the compact size.
const sizeClass = computed(() =>
  current.value?.position === 'top' ? 'px-6 py-4 text-lg' : 'px-5 py-3 text-sm',
)
</script>

<template>
  <Transition
    enter-active-class="transition duration-200 ease-out"
    enter-from-class="opacity-0 translate-y-2"
    enter-to-class="opacity-100 translate-y-0"
    leave-active-class="transition duration-150 ease-in"
    leave-from-class="opacity-100"
    leave-to-class="opacity-0"
  >
    <button
      v-if="current"
      :key="current.seq"
      type="button"
      class="fixed z-[60] rounded-xl border shadow-lg cursor-pointer transition-transform active:scale-95"
      :class="[positionClass, toneClass, sizeClass]"
      @click="dismiss"
    >
      {{ current.text }}
    </button>
  </Transition>
</template>
