<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { pb } from '../lib/pb'
import { api } from '../lib/api'
import KioskDialog from '../components/KioskDialog.vue'
import { useAdminToast } from '../composables/useAdminToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import type { HeartbeatsResponse, KioskRecord } from '../types'

const toast = useAdminToast()
const router = useRouter()
const { identity } = useKioskIdentity()
const isController = computed(() => identity.value?.role === 'controller')

const kiosks = ref<KioskRecord[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const search = ref('')

const editing = ref<Partial<KioskRecord> | null>(null)

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

// onlineStatusFor mirrors the AdminKioskDetailView logic. Same thresholds
// across both views so the badge stays consistent when the operator
// drills in.
function onlineStatusFor(code: string): 'online' | 'stale' | 'offline' | 'unknown' {
  const ts = heartbeats.value[code]
  if (!ts) {
    if (!controllerStartedAt.value) return 'unknown'
    const sinceRestart = Date.now() - new Date(controllerStartedAt.value).getTime()
    return sinceRestart < 90_000 ? 'unknown' : 'offline'
  }
  const age = Date.now() - new Date(ts).getTime()
  if (age < 90_000) return 'online'
  if (age < 5 * 60_000) return 'stale'
  return 'offline'
}

function onlineBadgeClass(s: ReturnType<typeof onlineStatusFor>): string {
  switch (s) {
    case 'online':
      return 'bg-emerald-900/60 text-emerald-200'
    case 'stale':
      return 'bg-amber-900/60 text-amber-200'
    case 'offline':
      return 'bg-red-900/60 text-red-200'
    default:
      return 'bg-slate-800 text-slate-400'
  }
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

function openDetail(kiosk: KioskRecord) {
  router.push({ name: 'admin-kiosk-detail', params: { code: kiosk.kiosk_code } })
}

function openNew() {
  editing.value = {}
}

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
</script>

<template>
  <main class="p-6 max-w-7xl mx-auto w-full">
    <header class="flex items-baseline justify-between mb-4">
      <div>
        <h1 class="text-2xl font-semibold">Kiosks</h1>
        <p class="text-sm text-slate-400">{{ kiosks.length }} registered</p>
      </div>
      <button
        v-if="isController"
        type="button"
        class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        @click="openNew"
      >
        New kiosk
      </button>
    </header>

    <input
      v-model="search"
      type="search"
      placeholder="Search code, location, notes…"
      class="w-full rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100 mb-4"
    />

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 mb-3">
      {{ error }}
    </p>

    <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
      <table class="w-full text-left text-sm">
        <thead class="bg-slate-950/70 text-slate-400">
          <tr>
            <th class="px-4 py-3 font-medium">Kiosk code</th>
            <th class="px-4 py-3 font-medium">Location</th>
            <th class="px-4 py-3 font-medium">Online</th>
            <th class="px-4 py-3 font-medium">Status</th>
            <th class="px-4 py-3 font-medium">Last transaction</th>
            <th class="px-4 py-3 font-medium">Notes</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-slate-800">
          <tr v-if="loading">
            <td colspan="6" class="text-center text-slate-500 py-8">Loading…</td>
          </tr>
          <tr v-else-if="filtered.length === 0">
            <td colspan="6" class="text-center text-slate-500 py-8">
              {{ kiosks.length === 0
                ? 'No kiosks yet. A kiosk auto-registers the first time it publishes a transaction event or heartbeat.'
                : 'No kiosks match your filter.' }}
            </td>
          </tr>
          <tr
            v-for="kiosk in filtered"
            :key="kiosk.id"
            class="hover:bg-slate-800/50 cursor-pointer"
            @click="openDetail(kiosk)"
          >
            <td class="px-4 py-3 font-mono text-slate-200">{{ kiosk.kiosk_code }}</td>
            <td class="px-4 py-3 text-slate-300">{{ kiosk.location_code || '—' }}</td>
            <td class="px-4 py-3">
              <span
                class="inline-block px-2 py-0.5 rounded text-xs capitalize"
                :class="onlineBadgeClass(onlineStatusFor(kiosk.kiosk_code))"
              >
                {{ onlineStatusFor(kiosk.kiosk_code) }}
              </span>
            </td>
            <td class="px-4 py-3">
              <span
                class="inline-block px-2 py-0.5 rounded text-xs"
                :class="statusBadgeClass(kiosk.status)"
              >
                {{ kiosk.status }}
              </span>
            </td>
            <td class="px-4 py-3 text-slate-400">
              {{ lastSeenDisplay(kiosk.last_transaction_at ?? kiosk.last_seen) }}
            </td>
            <td class="px-4 py-3 text-slate-400 truncate max-w-xs">{{ kiosk.notes || '—' }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <KioskDialog
      :open="editing !== null"
      :kiosk="editing"
      @update:open="(v) => { if (!v) editing = null }"
      @save="onSave"
    />
  </main>
</template>
