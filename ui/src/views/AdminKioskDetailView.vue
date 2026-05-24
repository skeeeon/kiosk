<!-- AdminKioskDetailView is the controller's per-kiosk admin surface. Lifted
     out of KioskDialog so each tab gets real estate to grow. Four tabs:

       Overview  — fields + heartbeat-derived online indicator
       Items     — KioskItemsPanel (catalog membership)
       Inventory — KioskInventoryPanel (live qty, remote adjust)
       Instances — KioskInstancesPanel (serialized-unit roster + remote
                   create / edit / decommission / reactivate)

     Path: /admin/kiosks/:code. The :code param is the kiosk_code, not the
     PB record ID, so deep-links are stable across re-registrations. -->
<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { pb } from '../lib/pb'
import { api } from '../lib/api'
import { useAdminToast } from '../composables/useAdminToast'
import KioskItemsPanel from '../components/KioskItemsPanel.vue'
import KioskInventoryPanel from '../components/KioskInventoryPanel.vue'
import KioskInstancesPanel from '../components/KioskInstancesPanel.vue'
import type { HeartbeatsResponse, KioskRecord } from '../types'

const props = defineProps<{ code: string }>()
const router = useRouter()
const toast = useAdminToast()

const kiosk = ref<KioskRecord | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)
const saving = ref(false)

type TabId = 'overview' | 'items' | 'inventory' | 'instances'
const activeTab = ref<TabId>('overview')

// editable mirror of kiosk fields. kiosk_code is identity once persisted —
// never edit. Status and notes can change from this view; location_code is
// editable here for the same reason it was in the old dialog.
const form = ref<Pick<KioskRecord, 'location_code' | 'status' | 'notes'>>({
  location_code: '',
  status: 'unknown',
  notes: '',
})

// Heartbeat polling: every 10s while the page is visible. Cleared on
// unmount. Pause when the tab is backgrounded (visibilitychange) to spare
// the controller from idle traffic.
const heartbeatTs = ref<string | null>(null)
const controllerStartedAt = ref<string | null>(null)
let pollTimer: ReturnType<typeof setInterval> | null = null

async function loadKiosk() {
  loading.value = true
  error.value = null
  try {
    const list = await pb.collection('kiosks').getFullList<KioskRecord>({
      filter: `kiosk_code = "${props.code}"`,
    })
    if (list.length === 0) {
      error.value = `No kiosk with code ${props.code}`
      kiosk.value = null
      return
    }
    kiosk.value = list[0]
    form.value = {
      location_code: kiosk.value.location_code ?? '',
      status: kiosk.value.status,
      notes: kiosk.value.notes ?? '',
    }
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

async function pollHeartbeats() {
  try {
    const res = await api.get<HeartbeatsResponse>('/api/controller/kiosks/heartbeats')
    controllerStartedAt.value = res.controller_started_at
    heartbeatTs.value = res.kiosks[props.code] ?? null
  } catch {
    // Best-effort — a transient failure here just means the badge stays
    // on its last-known value. Don't toast.
  }
}

onMounted(async () => {
  await loadKiosk()
  await pollHeartbeats()
  pollTimer = setInterval(pollHeartbeats, 10_000)
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

// Three-state online indicator. Mirrors the threshold constants on the
// controller side (heartbeatFreshness=90s in internal/controller/inventory.go).
// "Unknown" suppresses "offline" briefly after a controller restart so the
// SPA doesn't paint a freshly-booted fleet red.
const onlineStatus = computed<'online' | 'stale' | 'offline' | 'unknown'>(() => {
  if (!heartbeatTs.value) {
    if (!controllerStartedAt.value) return 'unknown'
    const sinceRestart = Date.now() - new Date(controllerStartedAt.value).getTime()
    return sinceRestart < 90_000 ? 'unknown' : 'offline'
  }
  const age = Date.now() - new Date(heartbeatTs.value).getTime()
  if (age < 90_000) return 'online'
  if (age < 5 * 60_000) return 'stale'
  return 'offline'
})

const onlineLabel = computed(() => {
  switch (onlineStatus.value) {
    case 'online':
      return 'Online'
    case 'stale':
      return 'Stale'
    case 'offline':
      return 'Offline'
    default:
      return 'Unknown'
  }
})

const onlineBadgeClass = computed(() => {
  switch (onlineStatus.value) {
    case 'online':
      return 'bg-emerald-900/60 text-emerald-200'
    case 'stale':
      return 'bg-amber-900/60 text-amber-200'
    case 'offline':
      return 'bg-red-900/60 text-red-200'
    default:
      return 'bg-slate-800 text-slate-400'
  }
})

function lastTransactionDisplay(): string {
  const v = kiosk.value?.last_transaction_at ?? kiosk.value?.last_seen
  if (!v) return 'never'
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return v
  return d.toLocaleString()
}

async function save() {
  if (!kiosk.value) return
  saving.value = true
  try {
    const updated = await pb.collection('kiosks').update<KioskRecord>(kiosk.value.id, {
      location_code: form.value.location_code,
      status: form.value.status,
      notes: form.value.notes,
    })
    kiosk.value = updated
    toast.success('Kiosk updated')
  } catch (e) {
    toast.error((e as Error).message)
  } finally {
    saving.value = false
  }
}

function back() {
  router.push({ name: 'admin-kiosks' })
}
</script>

<template>
  <main class="p-6 max-w-6xl mx-auto w-full">
    <button
      type="button"
      class="text-sm text-slate-400 hover:text-slate-200 mb-3"
      @click="back"
    >
      ← Back to kiosks
    </button>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 mb-3">
      {{ error }}
    </p>

    <header v-if="kiosk" class="flex items-baseline justify-between mb-4">
      <div>
        <h1 class="text-2xl font-semibold font-mono">{{ kiosk.kiosk_code }}</h1>
        <p class="text-sm text-slate-400">
          {{ kiosk.location_code || 'no location' }}
          · last transaction {{ lastTransactionDisplay() }}
        </p>
      </div>
      <span
        class="inline-block px-2.5 py-1 rounded text-xs font-medium"
        :class="onlineBadgeClass"
      >
        {{ onlineLabel }}
      </span>
    </header>

    <div v-if="loading && !kiosk" class="text-slate-500 text-center py-8">Loading…</div>

    <div v-if="kiosk" class="flex gap-1 mb-4 border-b border-slate-800">
      <button
        v-for="t in (['overview','items','inventory','instances'] as TabId[])"
        :key="t"
        type="button"
        class="px-4 py-2 text-sm border-b-2 -mb-px"
        :class="activeTab === t
          ? 'border-brand-primary text-slate-100'
          : 'border-transparent text-slate-400 hover:text-slate-200'"
        @click="activeTab = t"
      >
        {{ t === 'overview' ? 'Overview' : t === 'items' ? 'Items' : t === 'inventory' ? 'Inventory' : 'Instances' }}
      </button>
    </div>

    <div v-if="kiosk && activeTab === 'overview'" class="rounded-2xl bg-slate-900 border border-slate-800 p-5">
      <div class="grid grid-cols-2 gap-4">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Kiosk code</span>
          <span class="rounded-lg bg-slate-950 border border-slate-800 px-3 py-2 text-slate-100 font-mono text-sm">
            {{ kiosk.kiosk_code }}
          </span>
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Last transaction</span>
          <span class="rounded-lg bg-slate-950 border border-slate-800 px-3 py-2 text-slate-300 text-sm">
            {{ lastTransactionDisplay() }}
          </span>
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Location code</span>
          <input
            v-model="form.location_code"
            type="text"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Status</span>
          <select
            v-model="form.status"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          >
            <option value="unknown">Unknown</option>
            <option value="active">Active</option>
            <option value="disabled">Disabled</option>
          </select>
        </label>
      </div>
      <label class="flex flex-col gap-1 mt-4">
        <span class="text-sm text-slate-400">Notes</span>
        <textarea
          v-model="form.notes"
          rows="3"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 resize-none"
        ></textarea>
      </label>
      <div class="flex justify-end mt-4">
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium disabled:opacity-50"
          :disabled="saving"
          @click="save"
        >
          {{ saving ? 'Saving…' : 'Save changes' }}
        </button>
      </div>
    </div>

    <div v-if="kiosk && activeTab === 'items'">
      <KioskItemsPanel :kiosk-id="kiosk.id" />
    </div>

    <div v-if="kiosk && activeTab === 'inventory'">
      <KioskInventoryPanel :kiosk-code="kiosk.kiosk_code" />
    </div>

    <div v-if="kiosk && activeTab === 'instances'">
      <KioskInstancesPanel :kiosk-code="kiosk.kiosk_code" />
    </div>
  </main>
</template>
