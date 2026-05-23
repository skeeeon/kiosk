<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '../stores/auth'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import AdminToast from '../components/AdminToast.vue'

const auth = useAuthStore()
const router = useRouter()
const { identity } = useKioskIdentity()

const isController = computed(() => identity.value?.role === 'controller')
const managed = computed(() => identity.value?.managed ?? false)
const logoBroken = ref(false)
const logoUrl = computed(() =>
  !logoBroken.value && identity.value?.branding?.logo_url
    ? identity.value.branding.logo_url
    : null,
)

// Mode chip: a single labelled badge that tells the operator at a glance
// which surface they're on. Three flavors share one component:
//   - Controller   → indigo, no kiosk code
//   - Kiosk        → slate, kiosk code + location
//   - Managed kiosk → same as kiosk plus a chain glyph
const chipText = computed(() => {
  if (isController.value) return 'Controller'
  const code = identity.value?.kiosk_code
  const loc = identity.value?.location_code
  if (code && loc) return `${code} · ${loc}`
  if (code) return code
  return 'Kiosk'
})

const chipClass = computed(() =>
  isController.value
    ? 'bg-indigo-900/60 border-indigo-700 text-indigo-200'
    : 'bg-slate-800 border-slate-700 text-slate-200',
)

function onLogout() {
  auth.logout()
  router.push(isController.value ? { name: 'admin-login' } : '/')
}
</script>

<template>
  <div class="flex flex-col flex-1 min-h-0">
    <nav class="flex items-center gap-1 px-6 py-3 border-b border-slate-800 bg-slate-900/50">
      <div class="flex items-center gap-3 mr-3 pr-3 border-r border-slate-800">
        <img
          v-if="logoUrl"
          :src="logoUrl"
          alt="Logo"
          class="h-8 w-auto"
          @error="logoBroken = true"
        />
        <span
          :class="['inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium whitespace-nowrap', chipClass]"
          :title="isController ? 'Central controller — fleet management' : managed ? 'Catalog managed by the controller' : 'Local kiosk'"
        >
          <svg v-if="isController" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="h-3.5 w-3.5">
            <path d="M3 4a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v3H3V4Zm0 4.5h14V12a2 2 0 0 1-2 2h-3v2h2a.5.5 0 0 1 0 1H6a.5.5 0 0 1 0-1h2v-2H5a2 2 0 0 1-2-2V8.5Z" />
          </svg>
          <svg v-else-if="managed" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="h-3.5 w-3.5">
            <path d="M8.5 4.5a3.5 3.5 0 0 1 4.95 0l2.05 2.05a3.5 3.5 0 0 1-4.95 4.95l-.6-.6 1.06-1.06.6.6a2 2 0 1 0 2.83-2.83L12.39 5.56a2 2 0 0 0-2.83 0l-.6.6L7.9 5.1l.6-.6Zm-3 3a3.5 3.5 0 0 1 4.95 0l.6.6L9.99 9.16l-.6-.6a2 2 0 1 0-2.83 2.83l2.05 2.05a2 2 0 0 0 2.83 0l.6-.6 1.06 1.06-.6.6a3.5 3.5 0 0 1-4.95-4.95L5.5 7.5Z" />
          </svg>
          {{ chipText }}
        </span>
      </div>

      <!-- Group 1: catalog data -->
      <RouterLink
        v-if="isController"
        :to="{ name: 'admin-kiosks' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Kiosks
      </RouterLink>
      <RouterLink
        v-if="isController"
        :to="{ name: 'admin-transactions' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Transactions
      </RouterLink>
      <span v-if="isController" class="h-6 w-px bg-slate-800 mx-2" aria-hidden="true" />

      <RouterLink
        :to="{ name: 'admin-items' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Items
      </RouterLink>
      <RouterLink
        :to="{ name: 'admin-users' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Workers
      </RouterLink>
      <RouterLink
        :to="{ name: 'admin-groups' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Groups
      </RouterLink>
      <span class="h-6 w-px bg-slate-800 mx-2" aria-hidden="true" />

      <!-- Group 2: ops / read-side -->
      <RouterLink
        :to="{ name: 'admin-reports' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Reports
      </RouterLink>
      <RouterLink
        v-if="!isController"
        :to="{ name: 'admin-notifications' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Notifications
      </RouterLink>
      <RouterLink
        v-if="isController"
        :to="{ name: 'admin-catalog-sync' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Catalog sync
      </RouterLink>
      <RouterLink
        v-if="!isController && !identity?.managed"
        :to="{ name: 'admin-import' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Import
      </RouterLink>
      <span class="h-6 w-px bg-slate-800 mx-2" aria-hidden="true" />

      <!-- Group 3: access -->
      <RouterLink
        :to="{ name: 'admin-admins' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Admins
      </RouterLink>

      <span class="ml-auto flex items-center gap-3 text-sm">
        <span class="text-slate-400">{{ auth.admin?.email }}</span>
        <button
          type="button"
          class="px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          @click="onLogout"
        >
          Logout
        </button>
      </span>
    </nav>
    <div class="flex-1 overflow-auto">
      <RouterView />
    </div>
    <AdminToast />
  </div>
</template>
