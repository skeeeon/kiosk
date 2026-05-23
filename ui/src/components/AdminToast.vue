<!-- AdminToast is the renderer for the singleton admin toast queue from
     useAdminToast. Mount it once in the admin shell; any view can fire
     toasts via the composable without prop-drilling. -->
<script setup lang="ts">
import { useAdminToast } from '../composables/useAdminToast'

const { current, dismiss } = useAdminToast()

const TONE: Record<'success' | 'error', string> = {
  success: 'bg-emerald-900/80 border-emerald-700 text-emerald-100',
  error: 'bg-red-900/80 border-red-700 text-red-100',
}
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
      class="fixed bottom-6 right-6 z-[60] px-5 py-3 rounded-xl border shadow-lg text-sm cursor-pointer"
      :class="TONE[current.kind]"
      @click="dismiss"
    >
      {{ current.text }}
    </button>
  </Transition>
</template>
