<script setup lang="ts">
import { computed } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '../stores/auth'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import AdminToast from '../components/AdminToast.vue'

const auth = useAuthStore()
const router = useRouter()
const { identity } = useKioskIdentity()

const isController = computed(() => identity.value?.role === 'controller')

function onLogout() {
  auth.logout()
  router.push(isController.value ? { name: 'admin-login' } : '/')
}
</script>

<template>
  <div class="flex flex-col flex-1 min-h-0">
    <nav class="flex items-center gap-1 px-6 py-3 border-b border-slate-800 bg-slate-900/50">
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
      <RouterLink
        v-if="!isController && !identity?.managed"
        :to="{ name: 'admin-import' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Import
      </RouterLink>
      <RouterLink
        v-if="!isController"
        :to="{ name: 'admin-reports' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Reports
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
    <div
      v-if="identity?.managed"
      class="px-6 py-2 bg-sky-950/60 border-b border-sky-800 text-sky-200 text-sm flex items-center gap-2"
    >
      <span>Catalog managed by controller — items and workers are edited centrally and synced down to this kiosk.</span>
    </div>
    <div class="flex-1 overflow-auto">
      <RouterView />
    </div>
    <AdminToast />
  </div>
</template>
