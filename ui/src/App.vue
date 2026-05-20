<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute } from 'vue-router'
import { storeToRefs } from 'pinia'
import { useKioskIdentity } from './composables/useKioskIdentity'
import { useSessionStore } from './stores/session'

const { identity } = useKioskIdentity()
const route = useRoute()
const session = useSessionStore()
const { cart } = storeToRefs(session)

// Layout modes:
//   - admin: any /admin/* route. App.vue stays out of the way; AdminLayout
//     (or AdminLoginView) draws its own chrome.
//   - active: kiosk route with an active cart. Slim header on top.
//   - idle:   kiosk route with no cart. Branded splash content from the view
//     itself fills the screen; we render a thin footer with operator chrome.
const layoutMode = computed<'admin' | 'active' | 'idle'>(() => {
  if (route.path.startsWith('/admin')) return 'admin'
  if (cart.value) return 'active'
  return 'idle'
})

// Track logo image failures so a misconfigured path doesn't leave a broken-
// image icon in the header. Resets when identity changes.
const logoBroken = ref(false)
const logoUrl = computed(() =>
  !logoBroken.value && identity.value?.branding?.logo_url
    ? identity.value.branding.logo_url
    : null,
)
</script>

<template>
  <div class="min-h-screen flex flex-col bg-slate-950 text-slate-100">
    <header
      v-if="layoutMode === 'active'"
      class="px-6 py-4 border-b border-slate-800 flex items-center gap-6"
    >
      <img
        v-if="logoUrl"
        :src="logoUrl"
        alt="logo"
        class="h-12 w-auto object-contain shrink-0"
        @error="logoBroken = true"
      />
      <span v-else class="text-slate-200 font-semibold tracking-wide text-xl">Kiosk</span>

      <span class="text-slate-200 font-mono text-lg">
        {{ identity?.kiosk_code ?? '…' }}
      </span>
      <span class="text-slate-400 font-mono text-lg">
        {{ identity?.location_code ?? '' }}
      </span>
      <RouterLink
        to="/admin/login"
        class="ml-auto text-slate-400 hover:text-slate-200 text-base"
      >
        Admin
      </RouterLink>
    </header>

    <RouterView class="flex-1" />

    <footer
      v-if="layoutMode === 'idle'"
      class="px-6 py-5 border-t border-slate-800 flex items-center justify-between text-lg text-slate-300"
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
