<!-- AdminKioskDetailView is the controller's per-kiosk admin surface. Lifted
     out of KioskDialog so each tab gets real estate to grow. Tabs:

       Overview  — fields + heartbeat-derived online indicator
       Items     — KioskItemsPanel (catalog membership)
       Inventory — KioskInventoryPanel (live qty, remote adjust)
       Instances — KioskInstancesPanel (serialized-unit roster + remote
                   create / edit / status transitions)
       Metrics   — KioskMetricsPanel (live operational + activity snapshot)

     Path: /admin/kiosks/:code. The :code param is the kiosk_code, not the
     PB record ID, so deep-links are stable across re-registrations. -->
<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { pb } from '../lib/pb'
import { api } from '../lib/api'
import { useToast } from '../composables/useToast'
import KioskItemsPanel from '../components/KioskItemsPanel.vue'
import KioskInventoryPanel from '../components/KioskInventoryPanel.vue'
import KioskInstancesPanel from '../components/KioskInstancesPanel.vue'
import KioskMetricsPanel from '../components/KioskMetricsPanel.vue'
import AppDialog from '../components/AppDialog.vue'
import type { HeartbeatsResponse, KioskRecord } from '../types'

const props = defineProps<{ code: string }>()
const router = useRouter()
const toast = useToast()

const kiosk = ref<KioskRecord | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)
const saving = ref(false)

type TabId = 'overview' | 'items' | 'inventory' | 'instances' | 'metrics'
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
      filter: pb.filter('kiosk_code = {:code}', { code: props.code }),
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

// --- Maintenance actions: integrity rebuild + ledger republish. Both
// run on the remote kiosk via the command bus; the offline shape mirrors
// other command-driven endpoints (503 + kiosk_offline). Confirmations
// gate destructive-by-policy actions even though the operations are
// idempotent on their own.

const rebuildOpen = ref(false)
const rebuildSubmitting = ref(false)
const republishOpen = ref(false)
const republishSubmitting = ref(false)
const republishForm = ref({ from: '', to: '' })

async function confirmRebuild() {
  if (!kiosk.value) return
  rebuildSubmitting.value = true
  try {
    const res = await api.post<{ deleted: number; inserted: number }>(
      `/api/controller/kiosks/${encodeURIComponent(kiosk.value.kiosk_code)}/integrity/rebuild`,
      {},
    )
    rebuildOpen.value = false
    toast.success(`Rebuilt: deleted ${res.deleted}, inserted ${res.inserted}`)
  } catch (e) {
    toast.error((e as Error).message)
  } finally {
    rebuildSubmitting.value = false
  }
}

async function submitRepublish() {
  if (!kiosk.value) return
  const body: Record<string, string> = {}
  // Convert local-datetime inputs ("2026-05-23T14:30") to RFC3339 with Z
  // suffix so the kiosk-side parser accepts them. Empty inputs are sent
  // as omitted fields (the kiosk treats those as "no clip on that end").
  if (republishForm.value.from.trim()) {
    body.from = new Date(republishForm.value.from).toISOString()
  }
  if (republishForm.value.to.trim()) {
    body.to = new Date(republishForm.value.to).toISOString()
  }
  republishSubmitting.value = true
  try {
    const res = await api.post<{
      transactions_published: number
      lines_published: number
      skipped: number
    }>(
      `/api/controller/kiosks/${encodeURIComponent(kiosk.value.kiosk_code)}/ledger/republish`,
      body,
    )
    republishOpen.value = false
    toast.success(
      `Republished: ${res.transactions_published} transactions, ${res.lines_published} lines` +
        (res.skipped ? ` (${res.skipped} skipped)` : ''),
    )
  } catch (e) {
    toast.error((e as Error).message)
  } finally {
    republishSubmitting.value = false
  }
}
</script>

<template>
  <main class="p-4 sm:p-6 max-w-6xl mx-auto w-full">
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

    <div v-if="kiosk" class="flex gap-1 mb-4 border-b border-slate-800 tab-scroll">
      <button
        v-for="t in (['overview','items','inventory','instances','metrics'] as TabId[])"
        :key="t"
        type="button"
        class="px-3 sm:px-4 py-2 text-sm border-b-2 -mb-px whitespace-nowrap"
        :class="activeTab === t
          ? 'border-brand-primary text-slate-100'
          : 'border-transparent text-slate-400 hover:text-slate-200'"
        @click="activeTab = t"
      >
        {{ t === 'overview' ? 'Overview' : t === 'items' ? 'Items' : t === 'inventory' ? 'Inventory' : t === 'instances' ? 'Instances' : 'Metrics' }}
      </button>
    </div>

    <div v-if="kiosk && activeTab === 'overview'" class="rounded-2xl bg-slate-900 border border-slate-800 p-5">
      <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
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

      <!-- Maintenance: rebuild the kiosk's open_checkouts from its
           transaction_lines ledger, or re-emit completed transactions
           so the controller's projection can backfill after a NATS
           outage. Both run on the kiosk via the command bus. -->
      <div class="mt-6 pt-5 border-t border-slate-800">
        <h2 class="text-sm font-medium text-slate-300 mb-2">Maintenance</h2>
        <p class="text-xs text-slate-500 mb-3">
          Recover from suspected ledger drift or fill the controller&rsquo;s
          projection after an outage. Both actions are idempotent
          (repeating them yields the same final state).
        </p>
        <div class="flex gap-3 flex-wrap">
          <button
            type="button"
            class="px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
            @click="rebuildOpen = true"
          >
            Rebuild open checkouts
          </button>
          <button
            type="button"
            class="px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
            @click="republishOpen = true"
          >
            Republish ledger…
          </button>
        </div>
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

    <div v-if="kiosk && activeTab === 'metrics'">
      <KioskMetricsPanel :kiosk-code="kiosk.kiosk_code" />
    </div>

    <AppDialog
      :open="rebuildOpen"
      title="Rebuild open checkouts"
      size="sm"
      @update:open="(v) => { if (!v) rebuildOpen = false }"
    >
      <p class="text-slate-300 text-sm">
        Wipes the kiosk&rsquo;s <code class="font-mono">open_checkouts</code>
        table and rebuilds it from the transaction-line ledger. Safe to
        repeat; use when integrity reports show drift you can&rsquo;t
        otherwise explain.
      </p>
      <div class="flex justify-end gap-3 mt-5">
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          @click="rebuildOpen = false"
        >
          Cancel
        </button>
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium disabled:opacity-50"
          :disabled="rebuildSubmitting"
          @click="confirmRebuild"
        >
          {{ rebuildSubmitting ? 'Rebuilding…' : 'Rebuild now' }}
        </button>
      </div>
    </AppDialog>

    <AppDialog
      :open="republishOpen"
      title="Republish ledger"
      size="sm"
      @update:open="(v) => { if (!v) republishOpen = false }"
    >
      <form class="flex flex-col gap-4" @submit.prevent="submitRepublish">
        <p class="text-slate-300 text-sm">
          Re-emit every completed transaction (and its lines) as NATS
          events. The controller&rsquo;s projection is idempotent on
          <code class="font-mono">source_line_id</code>, so duplicates are
          no-ops. Leave both fields blank for a full-history republish.
        </p>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">From <span class="text-slate-500">(optional)</span></span>
          <input
            v-model="republishForm.from"
            type="datetime-local"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">To <span class="text-slate-500">(optional)</span></span>
          <input
            v-model="republishForm.to"
            type="datetime-local"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
        <div class="flex justify-end gap-3 mt-1">
          <button
            type="button"
            class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
            @click="republishOpen = false"
          >
            Cancel
          </button>
          <button
            type="submit"
            class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium disabled:opacity-50"
            :disabled="republishSubmitting"
          >
            {{ republishSubmitting ? 'Republishing…' : 'Republish' }}
          </button>
        </div>
      </form>
    </AppDialog>
  </main>
</template>
