<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { pb } from '../lib/pb'
import { api } from '../lib/api'
import KioskDialog from '../components/KioskDialog.vue'
import DataTable, { type ColumnDef } from '../components/DataTable.vue'
import { useToast } from '../composables/useToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import { useListShortcuts } from '../composables/useListShortcuts'
import { useUrlQuerySync } from '../composables/useUrlQuerySync'
import { onlineStatusFor as sharedOnlineStatus, onlineBadgeClass } from '../lib/onlineStatus'
import type { HeartbeatsResponse, KioskRecord } from '../types'

const toast = useToast()
const router = useRouter()
const { identity } = useKioskIdentity()
const isController = computed(() => identity.value?.role === 'controller')

const kiosks = ref<KioskRecord[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const search = ref('')
const page = ref(1)
const perPage = ref(25)

const editing = ref<Partial<KioskRecord> | null>(null)
const searchInput = ref<HTMLInputElement | null>(null)

useUrlQuerySync({
  page: { ref: page, default: 1, parse: (v) => Number(v) || 1 },
  q: { ref: search, default: '' },
})

// Heartbeat polling state. The controller's in-memory map is authoritative
// for "is this kiosk online right now"; we poll every 10s while the page is
// visible. The map is empty for kiosks that have never beat since the
// controller restarted, which is why we also surface controller_started_at.
const heartbeats = ref<Record<string, string>>({})
const controllerStartedAt = ref<string | null>(null)
let heartbeatTimer: ReturnType<typeof setInterval> | null = null

async function load() {
  loading.value = true
  error.value = null
  try {
    // Sort newest-first by last_transaction_at so the active fleet floats
    // to the top. PB puts empty dates last regardless of direction, which
    // lines up with "never-transacted kiosks at the bottom."
    kiosks.value = await pb.collection('kiosks').getFullList<KioskRecord>({
      sort: '-last_transaction_at',
    })
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

async function pollHeartbeats() {
  if (!isController.value) return
  try {
    const res = await api.get<HeartbeatsResponse>('/api/controller/kiosks/heartbeats')
    heartbeats.value = res.kiosks
    controllerStartedAt.value = res.controller_started_at
  } catch {
    // Best-effort — last-known state stays on screen during a transient
    // controller blip. Don't toast.
  }
}

onMounted(async () => {
  await load()
  await pollHeartbeats()
  heartbeatTimer = setInterval(pollHeartbeats, 10_000)
})

onUnmounted(() => {
  if (heartbeatTimer) clearInterval(heartbeatTimer)
})

// Thin wrapper over the shared helper — same thresholds as
// AdminKioskDetailView so the badge stays consistent when the operator
// drills in.
function onlineStatusFor(code: string) {
  return sharedOnlineStatus(heartbeats.value[code], controllerStartedAt.value)
}

const filtered = computed(() => {
  const q = search.value.trim().toLowerCase()
  if (!q) return kiosks.value
  return kiosks.value.filter(
    (k) =>
      k.kiosk_code.toLowerCase().includes(q) ||
      k.location_code.toLowerCase().includes(q) ||
      (k.notes ?? '').toLowerCase().includes(q),
  )
})

watch(search, () => { page.value = 1 })

const pagedRows = computed(() => {
  const start = (page.value - 1) * perPage.value
  return filtered.value.slice(start, start + perPage.value)
})

function openDetail(kiosk: KioskRecord) {
  router.push({ name: 'admin-kiosk-detail', params: { code: kiosk.kiosk_code } })
}

function openNew() {
  editing.value = {}
}

useListShortcuts({ searchInput, onNew: openNew, canCreate: isController })

function lastSeenDisplay(v?: string): string {
  if (!v) return 'never'
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return v
  // Relative time for fresh kiosks, absolute date once stale (>24h).
  const ageMs = Date.now() - d.getTime()
  const min = Math.floor(ageMs / 60_000)
  if (min < 1) return 'just now'
  if (min < 60) return `${min}m ago`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr}h ago`
  return d.toLocaleDateString()
}

function statusBadgeClass(status: string): string {
  switch (status) {
    case 'active':
      return 'bg-emerald-900/60 text-emerald-200'
    case 'disabled':
      return 'bg-slate-800 text-slate-400'
    default:
      return 'bg-amber-900/60 text-amber-200'
  }
}

const columns: ColumnDef[] = [
  { key: 'kiosk_code', label: 'Kiosk code' },
  { key: 'location_code', label: 'Location' },
  { key: 'online', label: 'Online' },
  { key: 'status', label: 'Status' },
  { key: 'last_transaction', label: 'Last transaction' },
  { key: 'notes', label: 'Notes' },
]

const emptyText = computed(() =>
  kiosks.value.length === 0
    ? 'No kiosks yet. A kiosk auto-registers the first time it publishes a transaction event or heartbeat.'
    : 'No kiosks match your filter.',
)

async function onSave(data: Partial<KioskRecord>) {
  error.value = null
  // KioskDialog is now create-only — editing lives on AdminKioskDetailView.
  try {
    const created = await pb.collection('kiosks').create<KioskRecord>(data)
    editing.value = null
    await load()
    toast.success(`Created ${created.kiosk_code}`)
    // Jump straight into the new kiosk's detail page so the operator can
    // assign items, set status, etc. without a second navigation.
    router.push({ name: 'admin-kiosk-detail', params: { code: created.kiosk_code } })
  } catch (e) {
    const msg = (e as Error).message
    error.value = msg
    toast.error(msg)
  }
}

// Bulk-pre-register flow: stay on the list, reseed the dialog with a blank
// draft. Operator can fast-create several kiosks in a row, then drill into
// whichever ones need item assignment afterwards.
async function onSaveAndAdd(data: Partial<KioskRecord>) {
  error.value = null
  try {
    const created = await pb.collection('kiosks').create<KioskRecord>(data)
    editing.value = {}
    await load()
    toast.success(`Created ${created.kiosk_code} — ready for next`)
  } catch (e) {
    const msg = (e as Error).message
    error.value = msg
    toast.error(msg)
  }
}
</script>

<template>
  <main class="p-4 sm:p-6 max-w-7xl mx-auto w-full">
    <header class="flex items-center justify-between gap-3 mb-4">
      <div>
        <h1 class="text-2xl font-semibold">Kiosks</h1>
        <p class="text-sm text-slate-400">{{ kiosks.length }} registered</p>
      </div>
      <div class="flex items-center gap-2 shrink-0">
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 disabled:opacity-50"
          :disabled="loading"
          @click="load"
        >
          {{ loading ? 'Loading…' : 'Refresh' }}
        </button>
        <button
          v-if="isController"
          type="button"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
          @click="openNew"
        >
          New kiosk
        </button>
      </div>
    </header>

    <input
      ref="searchInput"
      v-model="search"
      type="search"
      placeholder="Search code, location, notes… (press / to focus)"
      class="w-full rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100 mb-4"
    />

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 mb-3">
      {{ error }}
    </p>

    <DataTable
      :columns="columns"
      :rows="pagedRows"
      :row-key="(k) => k.id"
      :loading="loading"
      :empty-text="emptyText"
      row-clickable
      :page="page"
      :per-page="perPage"
      :total="filtered.length"
      @row-click="openDetail"
      @update:page="(p) => page = p"
      @update:per-page="(n) => { perPage = n; page = 1 }"
    >
      <template #cell-kiosk_code="{ row }">
        <span class="font-mono text-slate-200">{{ row.kiosk_code }}</span>
      </template>
      <template #cell-location_code="{ row }">
        <span class="text-slate-300">{{ row.location_code || '—' }}</span>
      </template>
      <template #cell-online="{ row }">
        <span
          class="inline-block px-2 py-0.5 rounded text-xs capitalize"
          :class="onlineBadgeClass(onlineStatusFor(row.kiosk_code))"
        >
          {{ onlineStatusFor(row.kiosk_code) }}
        </span>
      </template>
      <template #cell-status="{ row }">
        <span class="inline-block px-2 py-0.5 rounded text-xs" :class="statusBadgeClass(row.status)">
          {{ row.status }}
        </span>
      </template>
      <template #cell-last_transaction="{ row }">
        <span class="text-slate-400">
          {{ lastSeenDisplay(row.last_transaction_at) }}
        </span>
      </template>
      <template #cell-notes="{ row }">
        <span class="text-slate-400 truncate inline-block max-w-[14rem] align-middle">
          {{ row.notes || '—' }}
        </span>
      </template>
    </DataTable>

    <KioskDialog
      :open="editing !== null"
      :kiosk="editing"
      @update:open="(v) => { if (!v) editing = null }"
      @save="onSave"
      @save-and-add-another="onSaveAndAdd"
    />
  </main>
</template>
