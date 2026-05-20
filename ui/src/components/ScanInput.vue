<!-- ScanInput is intentionally invisible. It owns the global keydown listener
     via useScan and emits 'scan' on each completed (Enter-terminated) read.
     A barcode scanner emulating a keyboard drives this; a developer can also
     just type at the focused window and hit Enter. -->
<script setup lang="ts">
import { useScan } from '../composables/useScan'

const emit = defineEmits<{ scan: [value: string] }>()
useScan((value) => emit('scan', value))
</script>

<template>
  <span class="sr-only" aria-hidden="true">scan input active</span>
</template>
