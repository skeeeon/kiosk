<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { useKioskIdentity } from './composables/useKioskIdentity'

const { identity } = useKioskIdentity()
const route = useRoute()

// App.vue owns only the cross-route shell: a footer with kiosk identity
// shown on every non-admin route, and the RouterView in between. Admin
// routes draw their own chrome via AdminLayout. The kiosk's active-cart /
// idle / success states are managed inside CheckoutView, which renders
// its own upper-left logo and per-state header content as needed.
const isAdminRoute = computed(() => route.path.startsWith('/admin'))
</script>

<template>
  <!-- h-dvh + overflow-hidden anchors the chrome to the viewport: as cart
       lines (or any other route's content) grow, the inner overflow-auto
       containers scroll instead of pushing the document past 100vh and
       taking the footer with it. min-h-screen used to let the document
       grow past the viewport, which broke that contract. -->
  <div class="h-dvh overflow-hidden flex flex-col bg-slate-950 text-slate-100">
    <RouterView class="flex-1 min-h-0" />

    <footer
      v-if="!isAdminRoute"
      class="px-6 py-6 border-t border-slate-800 flex items-center justify-between text-xl text-slate-300"
    >
      <span class="font-mono">
        {{ identity?.kiosk_code ?? '…' }}
        <span v-if="identity?.location_code" class="text-slate-400"> · {{ identity.location_code }}</span>
      </span>
      <RouterLink
        to="/admin/login"
        class="text-slate-400 hover:text-slate-200"
      >
        Admin
      </RouterLink>
    </footer>
  </div>
</template>
