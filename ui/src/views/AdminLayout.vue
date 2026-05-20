<script setup lang="ts">
import { useRouter } from 'vue-router'
import { useAuthStore } from '../stores/auth'
import AdminToast from '../components/AdminToast.vue'

const auth = useAuthStore()
const router = useRouter()

function onLogout() {
  auth.logout()
  router.push('/')
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
        :to="{ name: 'admin-import' }"
        class="px-3 py-2 rounded-lg text-slate-300 hover:bg-slate-800"
        active-class="bg-slate-800 text-slate-100"
      >
        Import
      </RouterLink>
      <RouterLink
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
    <div class="flex-1 overflow-auto">
      <RouterView />
    </div>
    <AdminToast />
  </div>
</template>
