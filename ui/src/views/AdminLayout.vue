<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  DialogRoot,
  DialogPortal,
  DialogOverlay,
  DialogContent,
  TooltipProvider,
  TooltipRoot,
  TooltipTrigger,
  TooltipPortal,
  TooltipContent,
} from 'reka-ui'
import { useAuthStore } from '../stores/auth'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import { useToast } from '../composables/useToast'
import { api } from '../lib/api'
import SidebarNavIcon from '../components/SidebarNavIcon.vue'
import type { LedgerRepublishResult } from '../types'

const auth = useAuthStore()
const router = useRouter()
const toast = useToast()
const { identity } = useKioskIdentity()

const isController = computed(() => identity.value?.role === 'controller')
const managed = computed(() => identity.value?.managed ?? false)
// The virtual timeclock terminal has no checkout flow, so checkout-world admin
// surfaces (Items, the cart/RFID Metrics dashboard) are hidden — see the
// navItems visibility gates below.
const isTimeclockVirtual = computed(() => identity.value?.timeclock_virtual ?? false)
const logoBroken = ref(false)
const logoUrl = computed(() =>
  !logoBroken.value && identity.value?.branding?.logo_url
    ? identity.value.branding.logo_url
    : null,
)

// Sidebar collapse state. Persists via localStorage so the layout doesn't
// flip on refresh. Defaults to expanded for both roles — operators can pin
// to rail-only when they want more horizontal table room.
const COLLAPSED_KEY = 'admin.sidebar.collapsed'
const collapsed = ref(localStorage.getItem(COLLAPSED_KEY) === '1')
function toggleCollapsed() {
  collapsed.value = !collapsed.value
  localStorage.setItem(COLLAPSED_KEY, collapsed.value ? '1' : '0')
}

// Mobile drawer (below md). Desktop sidebar lays out in flow; the drawer
// is an overlay so it doesn't push content on narrow viewports.
const drawerOpen = ref(false)

const resyncing = ref(false)

async function onResync() {
  if (resyncing.value) return
  resyncing.value = true
  try {
    const r = await api.post<LedgerRepublishResult>('/api/kiosk/ledger/republish', {})
    toast.success(
      `Resync complete: republished ${r.transactions_published} transactions, ${r.lines_published} lines` +
        (r.skipped > 0 ? ` (${r.skipped} skipped)` : ''),
    )
  } catch (e) {
    toast.error(`Resync failed: ${(e as Error).message}`)
  } finally {
    resyncing.value = false
  }
}

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

type NavSection = 'catalog' | 'ops' | 'access'
type NavItem = {
  name: string
  label: string
  section: NavSection
  visible?: () => boolean
}

const navItems = computed<NavItem[]>(() => [
  { name: 'admin-kiosks', label: 'Kiosks', section: 'catalog', visible: () => isController.value },
  { name: 'admin-items', label: 'Items', section: 'catalog', visible: () => !isTimeclockVirtual.value },
  { name: 'admin-users', label: 'Workers', section: 'catalog' },
  { name: 'admin-groups', label: 'Groups', section: 'catalog' },
  { name: 'admin-reports', label: 'Reports', section: 'ops' },
  // Controller-only — an operational ledger drill-down, grouped with Reports.
  { name: 'admin-transactions', label: 'Transactions', section: 'ops', visible: () => isController.value },
  { name: 'admin-metrics', label: 'Metrics', section: 'ops', visible: () => !isController.value && !isTimeclockVirtual.value },
  { name: 'admin-notifications', label: 'Notifications', section: 'ops' },
  { name: 'admin-catalog-sync', label: 'Catalog sync', section: 'ops', visible: () => isController.value },
  { name: 'admin-import', label: 'Import', section: 'ops', visible: () => isController.value || !managed.value },
  { name: 'admin-admins', label: 'Admins', section: 'access' },
])

const sections: { id: NavSection; label: string }[] = [
  { id: 'catalog', label: 'Catalog' },
  { id: 'ops', label: 'Operations' },
  { id: 'access', label: 'Access' },
]

const visibleSections = computed(() =>
  sections.flatMap((s) => {
    const items = navItems.value.filter((i) => i.section === s.id && (i.visible?.() ?? true))
    return items.length ? [{ ...s, items }] : []
  }),
)

function onLogout() {
  auth.logout()
  router.push(isController.value ? { name: 'admin-login' } : '/')
  drawerOpen.value = false
}

function closeDrawer() {
  drawerOpen.value = false
}
</script>

<template>
  <TooltipProvider :delay-duration="150" :skip-delay-duration="200">
  <div class="flex flex-1 min-h-0">
    <!-- Desktop sidebar (md+). -->
    <aside
      :class="[
        'hidden md:flex flex-col border-r border-slate-800 bg-slate-900/50 transition-[width] duration-150',
        collapsed ? 'w-16' : 'w-60',
      ]"
    >
      <!-- Brand area. Two-row layout in expanded mode (logo + collapse on
           row 1, full-width identity chip on row 2) so the chip has room
           for any kiosk_code + location_code length. Collapsed mode stacks
           the expand toggle on top and the logo below; the chip is hidden
           since it wouldn't fit in 64px. -->
      <div v-if="collapsed" class="flex flex-col items-center gap-3 px-2 py-3 border-b border-slate-800">
        <TooltipRoot>
          <TooltipTrigger as-child>
            <button
              type="button"
              class="flex items-center justify-center h-7 w-7 rounded-md text-slate-400 hover:bg-slate-800 hover:text-slate-200"
              aria-label="Expand sidebar"
              @click="toggleCollapsed"
            >
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="h-4 w-4">
                <path fill-rule="evenodd" d="M7.21 14.77a.75.75 0 0 1 .02-1.06L10.44 10 7.23 6.29a.75.75 0 1 1 1.06-1.06l3.75 4.25a.75.75 0 0 1 0 1.04l-3.75 4.25a.75.75 0 0 1-1.06.02Z" clip-rule="evenodd" />
              </svg>
            </button>
          </TooltipTrigger>
          <TooltipPortal>
            <TooltipContent
              side="right"
              :side-offset="8"
              class="z-50 rounded-md bg-slate-800 border border-slate-700 px-2 py-1 text-xs text-slate-100 shadow-lg"
            >
              Expand sidebar
            </TooltipContent>
          </TooltipPortal>
        </TooltipRoot>
        <img
          v-if="logoUrl"
          :src="logoUrl"
          alt="Logo"
          class="h-10 w-10 object-contain shrink-0"
          @error="logoBroken = true"
        />
      </div>
      <div v-else class="border-b border-slate-800">
        <div class="flex items-center justify-between gap-3 px-4 py-3">
          <img
            v-if="logoUrl"
            :src="logoUrl"
            alt="Logo"
            class="h-10 w-auto shrink-0"
            @error="logoBroken = true"
          />
          <span v-else class="text-sm font-semibold text-slate-200">Admin</span>
          <TooltipRoot>
            <TooltipTrigger as-child>
              <button
                type="button"
                class="flex items-center justify-center h-7 w-7 rounded-md text-slate-400 hover:bg-slate-800 hover:text-slate-200 shrink-0"
                aria-label="Collapse sidebar"
                @click="toggleCollapsed"
              >
                <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="h-4 w-4">
                  <path fill-rule="evenodd" d="M12.79 14.77a.75.75 0 0 0-.02-1.06L9.56 10l3.21-3.71a.75.75 0 1 0-1.06-1.06L7.96 9.48a.75.75 0 0 0 0 1.04l3.75 4.25a.75.75 0 0 0 1.08.02Z" clip-rule="evenodd" />
                </svg>
              </button>
            </TooltipTrigger>
            <TooltipPortal>
              <TooltipContent
                side="bottom"
                :side-offset="6"
                class="z-50 rounded-md bg-slate-800 border border-slate-700 px-2 py-1 text-xs text-slate-100 shadow-lg"
              >
                Collapse sidebar
              </TooltipContent>
            </TooltipPortal>
          </TooltipRoot>
        </div>
        <div class="px-4 pb-3">
          <span
            :class="[
              'flex items-center justify-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium',
              chipClass,
            ]"
            :title="isController ? 'Central controller — fleet management' : 'Local kiosk'"
          >
            <svg
              v-if="isController"
              xmlns="http://www.w3.org/2000/svg"
              viewBox="0 0 20 20"
              fill="currentColor"
              class="h-3.5 w-3.5 shrink-0"
            >
              <path d="M3 4a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v3H3V4Zm0 4.5h14V12a2 2 0 0 1-2 2h-3v2h2a.5.5 0 0 1 0 1H6a.5.5 0 0 1 0-1h2v-2H5a2 2 0 0 1-2-2V8.5Z" />
            </svg>
            <span class="truncate">{{ chipText }}</span>
          </span>
        </div>
      </div>

      <!-- Nav. In collapsed/rail mode each link is wrapped with a Reka Tooltip
           that surfaces the label on hover/focus; expanded mode is plain. -->
      <nav class="flex-1 overflow-y-auto py-3 px-2">
        <template v-for="(section, sIdx) in visibleSections" :key="section.id">
          <div v-if="!collapsed" class="px-2 pt-3 pb-1 text-[10px] font-semibold uppercase tracking-wider text-slate-500">
            {{ section.label }}
          </div>
          <div v-else-if="sIdx > 0" class="my-2 mx-3 border-t border-slate-800" aria-hidden="true" />
          <template v-if="collapsed">
            <TooltipRoot v-for="item in section.items" :key="item.name">
              <TooltipTrigger as-child>
                <RouterLink
                  :to="{ name: item.name }"
                  class="flex items-center justify-center h-10 mx-1 rounded-lg border-l-2 border-transparent text-slate-300 hover:bg-slate-800/60 hover:text-slate-100"
                  active-class="bg-brand-primary/15 text-slate-50 border-brand-primary"
                  :aria-label="item.label"
                >
                  <SidebarNavIcon :name="item.name" class="h-5 w-5 shrink-0" />
                </RouterLink>
              </TooltipTrigger>
              <TooltipPortal>
                <TooltipContent
                  side="right"
                  :side-offset="8"
                  class="z-50 rounded-md bg-slate-800 border border-slate-700 px-2 py-1 text-xs text-slate-100 shadow-lg"
                >
                  {{ item.label }}
                </TooltipContent>
              </TooltipPortal>
            </TooltipRoot>
          </template>
          <RouterLink
            v-else
            v-for="item in section.items"
            :key="item.name"
            :to="{ name: item.name }"
            class="flex items-center gap-3 px-3 py-2 rounded-lg border-l-2 border-transparent text-slate-300 hover:bg-slate-800/60 hover:text-slate-100"
            active-class="bg-brand-primary/15 text-slate-50 border-brand-primary"
          >
            <SidebarNavIcon :name="item.name" class="h-5 w-5 shrink-0" />
            <span class="truncate">{{ item.label }}</span>
          </RouterLink>
        </template>
      </nav>

      <!-- Footer: email + logout. Collapse toggle moved to the brand area
           so it isn't adjacent to the destructive Logout action. -->
      <div class="border-t border-slate-800 p-2">
        <div v-if="!collapsed" class="px-2 py-1 text-xs text-slate-400 truncate" :title="auth.admin?.email">
          {{ auth.admin?.email }}
        </div>
        <TooltipRoot v-if="collapsed">
          <TooltipTrigger as-child>
            <button
              type="button"
              class="mt-1 flex items-center justify-center h-9 w-9 mx-auto rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
              aria-label="Logout"
              @click="onLogout"
            >
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="h-4 w-4">
                <path d="M3 4a2 2 0 0 1 2-2h6a2 2 0 0 1 2 2v1a.5.5 0 0 1-1 0V4a1 1 0 0 0-1-1H5a1 1 0 0 0-1 1v12a1 1 0 0 0 1 1h6a1 1 0 0 0 1-1v-1a.5.5 0 0 1 1 0v1a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V4Zm11.85 2.65a.5.5 0 0 1 .7 0l2.8 2.8a.5.5 0 0 1 0 .7l-2.8 2.8a.5.5 0 0 1-.7-.7L16.79 10.5H8a.5.5 0 0 1 0-1h8.79l-1.94-1.95a.5.5 0 0 1 0-.7Z" />
              </svg>
            </button>
          </TooltipTrigger>
          <TooltipPortal>
            <TooltipContent
              side="right"
              :side-offset="8"
              class="z-50 rounded-md bg-slate-800 border border-slate-700 px-2 py-1 text-xs text-slate-100 shadow-lg"
            >
              Logout
            </TooltipContent>
          </TooltipPortal>
        </TooltipRoot>
        <button
          v-else
          type="button"
          class="mt-1 w-full flex items-center justify-center gap-1.5 px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
          @click="onLogout"
        >
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="h-4 w-4">
            <path d="M3 4a2 2 0 0 1 2-2h6a2 2 0 0 1 2 2v1a.5.5 0 0 1-1 0V4a1 1 0 0 0-1-1H5a1 1 0 0 0-1 1v12a1 1 0 0 0 1 1h6a1 1 0 0 0 1-1v-1a.5.5 0 0 1 1 0v1a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V4Zm11.85 2.65a.5.5 0 0 1 .7 0l2.8 2.8a.5.5 0 0 1 0 .7l-2.8 2.8a.5.5 0 0 1-.7-.7L16.79 10.5H8a.5.5 0 0 1 0-1h8.79l-1.94-1.95a.5.5 0 0 1 0-.7Z" />
          </svg>
          <span>Logout</span>
        </button>
      </div>
    </aside>

    <!-- Mobile drawer (below md). Mirrors the desktop sidebar's expanded layout. -->
    <DialogRoot v-model:open="drawerOpen">
      <DialogPortal>
        <DialogOverlay class="md:hidden fixed inset-0 bg-black/70 backdrop-blur-sm z-30 data-[state=open]:animate-in data-[state=closed]:animate-out" />
        <DialogContent
          class="md:hidden fixed inset-y-0 left-0 w-72 max-w-[85vw] flex flex-col bg-slate-900 border-r border-slate-800 z-40 shadow-2xl focus:outline-none"
        >
          <div class="flex items-center gap-3 px-4 py-3 border-b border-slate-800">
            <img
              v-if="logoUrl"
              :src="logoUrl"
              alt="Logo"
              class="h-10 w-auto"
              @error="logoBroken = true"
            />
            <span
              :class="[
                'inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium whitespace-nowrap truncate',
                chipClass,
              ]"
            >
              <svg
                v-if="isController"
                xmlns="http://www.w3.org/2000/svg"
                viewBox="0 0 20 20"
                fill="currentColor"
                class="h-3.5 w-3.5"
              >
                <path d="M3 4a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v3H3V4Zm0 4.5h14V12a2 2 0 0 1-2 2h-3v2h2a.5.5 0 0 1 0 1H6a.5.5 0 0 1 0-1h2v-2H5a2 2 0 0 1-2-2V8.5Z" />
              </svg>
              {{ chipText }}
            </span>
          </div>
          <nav class="flex-1 overflow-y-auto py-3 px-2">
            <template v-for="section in visibleSections" :key="section.id">
              <div class="px-2 pt-3 pb-1 text-[10px] font-semibold uppercase tracking-wider text-slate-500">
                {{ section.label }}
              </div>
              <RouterLink
                v-for="item in section.items"
                :key="item.name"
                :to="{ name: item.name }"
                class="flex items-center gap-3 px-3 py-2 rounded-lg border-l-2 border-transparent text-slate-300 hover:bg-slate-800/60 hover:text-slate-100"
                active-class="bg-brand-primary/15 text-slate-50 border-brand-primary"
                @click="closeDrawer"
              >
                <SidebarNavIcon :name="item.name" class="h-5 w-5 shrink-0" />
                <span class="truncate">{{ item.label }}</span>
              </RouterLink>
            </template>
          </nav>
          <div class="border-t border-slate-800 p-3">
            <div class="px-1 py-1 text-xs text-slate-400 truncate" :title="auth.admin?.email">
              {{ auth.admin?.email }}
            </div>
            <button
              type="button"
              class="mt-2 w-full flex items-center justify-center gap-1.5 px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
              @click="onLogout"
            >
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="h-4 w-4">
                <path d="M3 4a2 2 0 0 1 2-2h6a2 2 0 0 1 2 2v1a.5.5 0 0 1-1 0V4a1 1 0 0 0-1-1H5a1 1 0 0 0-1 1v12a1 1 0 0 0 1 1h6a1 1 0 0 0 1-1v-1a.5.5 0 0 1 1 0v1a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V4Zm11.85 2.65a.5.5 0 0 1 .7 0l2.8 2.8a.5.5 0 0 1 0 .7l-2.8 2.8a.5.5 0 0 1-.7-.7L16.79 10.5H8a.5.5 0 0 1 0-1h8.79l-1.94-1.95a.5.5 0 0 1 0-.7Z" />
              </svg>
              Logout
            </button>
          </div>
        </DialogContent>
      </DialogPortal>
    </DialogRoot>

    <!-- Main area -->
    <div class="flex flex-col flex-1 min-w-0">
      <!-- Mobile top bar (below md). Hamburger + identity chip; desktop hides this. -->
      <div class="md:hidden flex items-center gap-3 px-4 py-2 border-b border-slate-800 bg-slate-900/50">
        <button
          type="button"
          class="flex items-center justify-center h-9 w-9 rounded-lg text-slate-300 hover:bg-slate-800"
          aria-label="Open navigation"
          @click="drawerOpen = true"
        >
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="h-5 w-5">
            <path fill-rule="evenodd" d="M3 5.5A.75.75 0 0 1 3.75 4.75h12.5a.75.75 0 0 1 0 1.5H3.75A.75.75 0 0 1 3 5.5Zm0 4.5A.75.75 0 0 1 3.75 9.25h12.5a.75.75 0 0 1 0 1.5H3.75A.75.75 0 0 1 3 10Zm0 4.5a.75.75 0 0 1 .75-.75h12.5a.75.75 0 0 1 0 1.5H3.75a.75.75 0 0 1-.75-.75Z" clip-rule="evenodd" />
          </svg>
        </button>
        <span
          :class="[
            'inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium whitespace-nowrap truncate',
            chipClass,
          ]"
        >
          {{ chipText }}
        </span>
      </div>

      <!-- Managed banner (kiosk + managed only). Carries the Resync action too —
           single affordance, no sidebar duplicate. -->
      <div
        v-if="managed"
        class="px-6 py-2 bg-sky-950/60 border-b border-sky-800 text-sky-200 text-sm flex items-center gap-3"
      >
        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="h-4 w-4 shrink-0">
          <path d="M8.5 4.5a3.5 3.5 0 0 1 4.95 0l2.05 2.05a3.5 3.5 0 0 1-4.95 4.95l-.6-.6 1.06-1.06.6.6a2 2 0 1 0 2.83-2.83L12.39 5.56a2 2 0 0 0-2.83 0l-.6.6L7.9 5.1l.6-.6Zm-3 3a3.5 3.5 0 0 1 4.95 0l.6.6L9.99 9.16l-.6-.6a2 2 0 1 0-2.83 2.83l2.05 2.05a2 2 0 0 0 2.83 0l.6-.6 1.06 1.06-.6.6a3.5 3.5 0 0 1-4.95-4.95L5.5 7.5Z" />
        </svg>
        <span class="flex-1 min-w-0">Catalog managed by controller — items, workers, and groups are edited centrally and synced down to this kiosk.</span>
        <button
          type="button"
          class="shrink-0 px-3 py-1 rounded-md bg-sky-800/70 hover:bg-sky-700 text-sky-100 text-xs font-medium disabled:opacity-50"
          :disabled="resyncing"
          title="Re-emit every completed transaction's events. Safe to run any time — the controller deduplicates by source transaction id. Use after a suspected NATS outage to recover dropped events."
          @click="onResync"
        >
          {{ resyncing ? 'Resyncing…' : 'Resync ledger' }}
        </button>
      </div>

      <div class="flex-1 overflow-auto">
        <RouterView />
      </div>
    </div>
  </div>
  </TooltipProvider>
</template>
