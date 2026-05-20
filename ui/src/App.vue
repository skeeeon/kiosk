<script setup lang="ts">
import { useKioskIdentity } from './composables/useKioskIdentity'

const { identity } = useKioskIdentity()
</script>

<template>
  <div class="min-h-screen flex flex-col bg-slate-950 text-slate-100">
    <header class="px-6 py-3 border-b border-slate-800 flex items-baseline justify-between text-sm">
      <span class="text-slate-400">
        Kiosk
        <span class="text-slate-200 font-semibold">{{ identity?.kiosk_code ?? '…' }}</span>
      </span>
      <span class="flex items-center gap-4">
        <RouterLink
          to="/admin/login"
          class="text-slate-600 hover:text-slate-400 text-xs"
        >
          Admin
        </RouterLink>
        <span class="text-slate-500">{{ identity?.location_code ?? '' }}</span>
      </span>
    </header>
    <RouterView class="flex-1" />
  </div>
</template>
