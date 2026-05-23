<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import { api, download } from '../lib/api'
import { useAdminToast } from '../composables/useAdminToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import TransactionDetailDialog, {
  type TxSummary,
} from '../components/TransactionDetailDialog.vue'
import type { ItemRecord, KioskRecord } from '../types'

const { identity } = useKioskIdentity()
const isController = computed(() => identity.value?.role === 'controller')

type Tab = 'currently-out' | 'aging' | 'low-stock' | 'group-activity' | 'recent' | 'audit' | 'notifications'
const tab = ref<Tab>('currently-out')
const toast = useAdminToast()

interface OpenRow {
  id: string
  serial: string
  checked_out_at: string
  kiosk_code?: string
  expand?: {
    item?: { id: string; code: string; name: string; type: string }
    user?: { id: string; code: string; name: string }
  }
}

interface TxRow {
  id: string
  kiosk_code: string
  location_code: string
  started_at: string
  completed_at: string
  status: string
  lines_count: number
  expand?: { user?: { id: string; code: string; name: string } }
}

interface LowStockRow {
  item: ItemRecord
  out: number // open_checkouts count (tools only)
  available: number
  deficit: number // threshold - available; positive means low
}

// FleetLowStockRow is one (kiosk, item) row from the controller's
// /api/controller/reports/low-stock endpoint. The flat shape matches the
// server-side fan-out + reduce; the SPA groups by item visually but the
// row itself stays atomic so kiosk filtering is a plain v-show filter.
interface FleetLowStockRow {
  kiosk_code: string
  item_code: string
  item_name: string
  tracking_mode: string
  quantity_on_hand: number
  out: number
  available: number
  reorder_threshold: number
}

interface FleetLowStockResponse {
  rows: FleetLowStockRow[]
  errors?: { kiosk_code: string; error: string }[]
}

// AdjustmentAuditRow is one inventory_audit collection row. The shape
// mirrors the migration's columns; the SPA loads them via pb-sdk so this
// is the wire JSON.
interface AdjustmentAuditRow {
  id: string
  kiosk_code: string
  item_code: string
  item_name: string
  source_adjustment_id: string
  admin_id: string
  mode: string
  delta: number
  prev_quantity: number
  new_quantity: number
  reason: string
  source: 'local' | 'controller' | ''
  command_id: string
  occurred_at: string
  created: string
}

// SendLogRow is one notification_send_log row, used by both the
// totals tally and the "Recent failures" panel.
interface SendLogRow {
  id: string
  event_type: string
  recipient: string
  status: 'sent' | 'failed' | 'skipped'
  error: string
  created: string
}

// SendLogByEvent groups send log counts by event_type. Computed
// client-side because PB's filter language doesn't support GROUP BY —
// the data volume is tiny (one row per recipient, capped at 90 days)
// so a single getList covers it.
interface SendLogByEvent {
  event_type: string
  sent: number
  failed: number
  skipped: number
}

// AgingGroup buckets all of one worker's overdue rows together so the table
// can show "Alice has 4 tools out >7 days, oldest is 23 days ago" rather than
// a flat list that hides repeat offenders.
interface AgingGroup {
  userId: string
  userCode: string
  userName: string
  rows: OpenRow[]
  oldestDays: number
}

interface GroupActivityRow {
  code: string                // group code (empty string = ungrouped)
  name: string                // group display name; equals code when ungrouped or unknown
  contactEmail: string
  transactions: number
  checkedOut: number
  returned: number
  consumed: number
}

const openRows = ref<OpenRow[]>([])
const openSearch = ref('')
const txRows = ref<TxRow[]>([])
const txPage = ref(1)
const txTotalPages = ref(1)
const lowStockRows = ref<LowStockRow[]>([])
const fleetLowStockRows = ref<FleetLowStockRow[]>([])
const fleetLowStockErrors = ref<{ kiosk_code: string; error: string }[]>([])
const auditRows = ref<AdjustmentAuditRow[]>([])
const auditPage = ref(1)
const auditTotalPages = ref(1)
const auditSourceFilter = ref<'' | 'local' | 'controller'>('')
const auditFrom = ref<string>(defaultFromDate())
const auditTo = ref<string>(defaultToDate())
const notificationsLookback = ref<number>(7)
const notificationsTotals = ref({ sent: 0, failed: 0, skipped: 0 })
const notificationsByEvent = ref<SendLogByEvent[]>([])
const notificationsRecentFailures = ref<SendLogRow[]>([])
const agingThresholdDays = ref(7)
const agingGroups = ref<AgingGroup[]>([])
const groupActivityFrom = ref<string>(defaultFromDate())
const groupActivityTo = ref<string>(defaultToDate())
const groupActivityRows = ref<GroupActivityRow[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

// Kiosk filter — only meaningful on the controller, where projected
// transactions span the fleet. On the kiosk binary this stays empty
// (local data is single-kiosk by definition).
const kioskFilter = ref<string>('')
const kiosks = ref<KioskRecord[]>([])

async function loadKiosks() {
  if (!isController.value) return
  try {
    kiosks.value = await pb.collection('kiosks').getFullList<KioskRecord>({ sort: '+kiosk_code' })
  } catch {
    // Non-fatal — dropdown stays empty, reports still work fleet-wide.
  }
}

const rebuildOpen = ref(false)
const rebuilding = ref(false)

const selectedTx = ref<TxSummary | null>(null)
const detailOpen = ref(false)

async function loadCurrentlyOut() {
  loading.value = true
  error.value = null
  try {
    // Computed by ledger replay on the server — same shape for both
    // binaries. On the controller we pass kiosk_code to slice the fleet.
    const qs = kioskFilter.value
      ? `?kiosk_code=${encodeURIComponent(kioskFilter.value)}`
      : ''
    openRows.value = await api.get<OpenRow[]>(`/api/kiosk/reports/open-checkouts${qs}`)
    openRows.value.sort((a, b) =>
      a.checked_out_at.localeCompare(b.checked_out_at),
    )
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

async function loadTransactions(page = 1) {
  loading.value = true
  error.value = null
  try {
    const filterParts = ['status = "completed"']
    if (kioskFilter.value) {
      filterParts.push(`kiosk_code = "${kioskFilter.value.replace(/"/g, '\\"')}"`)
    }
    const res = await pb.collection('transactions').getList<TxRow>(page, 50, {
      filter: filterParts.join(' && '),
      sort: '-completed_at',
      expand: 'user',
    })
    txRows.value = res.items
    txPage.value = res.page
    txTotalPages.value = res.totalPages
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

async function loadAging() {
  loading.value = true
  error.value = null
  try {
    // Same data source as Currently out — bucket by user and sort by
    // oldest-out. Threshold is a display-only hint; we keep every row so
    // the operator sees who's accumulating regardless of the cutoff.
    const qs = kioskFilter.value
      ? `?kiosk_code=${encodeURIComponent(kioskFilter.value)}`
      : ''
    const rows = await api.get<OpenRow[]>(`/api/kiosk/reports/open-checkouts${qs}`)
    const byUser = new Map<string, AgingGroup>()
    const now = Date.now()
    for (const r of rows) {
      const u = r.expand?.user
      if (!u) continue
      const days = Math.floor((now - new Date(r.checked_out_at).getTime()) / (1000 * 60 * 60 * 24))
      let g = byUser.get(u.id)
      if (!g) {
        g = { userId: u.id, userCode: u.code, userName: u.name, rows: [], oldestDays: days }
        byUser.set(u.id, g)
      }
      g.rows.push(r)
      if (days > g.oldestDays) g.oldestDays = days
    }
    agingGroups.value = Array.from(byUser.values()).sort((a, b) => b.oldestDays - a.oldestDays)
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

async function loadGroupActivity() {
  loading.value = true
  error.value = null
  try {
    const filter = buildGroupActivityFilter()
    // Lines are scoped via their parent transaction's date range; PB
    // supports indirect filters like `transaction.completed_at >= ...` so
    // we avoid enumerating transaction IDs in a giant OR.
    const linesFilter = buildGroupActivityLinesFilter()
    const [txs, lines, groupsList] = await Promise.all([
      pb.collection('transactions').getFullList<TxRow & { user_group?: string }>({
        filter,
        sort: '-completed_at',
      }),
      pb.collection('transaction_lines').getFullList<{
        transaction: string
        action: 'checkout' | 'return' | 'consume'
      }>({ filter: linesFilter }),
      pb.collection('groups').getFullList<{ code: string; name: string; contact_email: string }>(),
    ])
    if (txs.length === 0) {
      groupActivityRows.value = []
      return
    }
    const txByID = new Map(txs.map((t) => [t.id, t.user_group ?? '']))
    const groupByCode = new Map(groupsList.map((g) => [g.code, g]))

    const rolledUp = new Map<string, GroupActivityRow>()
    for (const t of txs) {
      const code = t.user_group ?? ''
      let row = rolledUp.get(code)
      if (!row) {
        const meta = code ? groupByCode.get(code) : undefined
        row = {
          code,
          name: meta?.name ?? (code || '(ungrouped)'),
          contactEmail: meta?.contact_email ?? '',
          transactions: 0,
          checkedOut: 0,
          returned: 0,
          consumed: 0,
        }
        rolledUp.set(code, row)
      }
      row.transactions++
    }
    for (const l of lines) {
      const code = txByID.get(l.transaction) ?? ''
      const row = rolledUp.get(code)
      if (!row) continue
      if (l.action === 'checkout') row.checkedOut++
      else if (l.action === 'return') row.returned++
      else if (l.action === 'consume') row.consumed++
    }
    groupActivityRows.value = Array.from(rolledUp.values()).sort(
      (a, b) => b.transactions - a.transactions,
    )
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

function defaultFromDate(): string {
  const d = new Date()
  d.setDate(1) // start of current month
  return d.toISOString().slice(0, 10)
}

function defaultToDate(): string {
  return new Date().toISOString().slice(0, 10)
}

function buildGroupActivityFilter(): string {
  const parts = ['status = "completed"']
  if (groupActivityFrom.value) parts.push(`completed_at >= "${groupActivityFrom.value} 00:00:00.000Z"`)
  if (groupActivityTo.value) parts.push(`completed_at <= "${groupActivityTo.value} 23:59:59.999Z"`)
  if (kioskFilter.value) parts.push(`kiosk_code = "${kioskFilter.value.replace(/"/g, '\\"')}"`)
  return parts.join(' && ')
}

function buildGroupActivityLinesFilter(): string {
  const parts = ['transaction.status = "completed"']
  if (groupActivityFrom.value)
    parts.push(`transaction.completed_at >= "${groupActivityFrom.value} 00:00:00.000Z"`)
  if (groupActivityTo.value)
    parts.push(`transaction.completed_at <= "${groupActivityTo.value} 23:59:59.999Z"`)
  if (kioskFilter.value)
    parts.push(`transaction.kiosk_code = "${kioskFilter.value.replace(/"/g, '\\"')}"`)
  return parts.join(' && ')
}

// loadFleetLowStock fans inventory.snapshot to every online managed kiosk
// via the controller's reports endpoint and renders the aggregated result.
// Offline kiosks are excluded server-side and surfaced under fleetLowStockErrors
// so the operator sees that the view is partial.
async function loadFleetLowStock() {
  loading.value = true
  error.value = null
  try {
    const qs = kioskFilter.value
      ? `?kiosk_code=${encodeURIComponent(kioskFilter.value)}`
      : ''
    const res = await api.get<FleetLowStockResponse>(`/api/controller/reports/low-stock${qs}`)
    fleetLowStockRows.value = res.rows ?? []
    fleetLowStockErrors.value = res.errors ?? []
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

async function loadAudit(page = 1) {
  loading.value = true
  error.value = null
  try {
    // inventory_audit lives only on the controller; the kiosk binary's PB
    // never sees the migration. Page-level pb-sdk query — kiosk + source +
    // date are stacked into the filter so paging stays correct.
    const parts: string[] = []
    if (kioskFilter.value) {
      parts.push(`kiosk_code = "${kioskFilter.value.replace(/"/g, '\\"')}"`)
    }
    if (auditSourceFilter.value) {
      parts.push(`source = "${auditSourceFilter.value}"`)
    }
    if (auditFrom.value) parts.push(`created >= "${auditFrom.value} 00:00:00.000Z"`)
    if (auditTo.value) parts.push(`created <= "${auditTo.value} 23:59:59.999Z"`)
    const res = await pb.collection('inventory_audit').getList<AdjustmentAuditRow>(page, 50, {
      filter: parts.join(' && '),
      sort: '-created',
    })
    auditRows.value = res.items
    auditPage.value = res.page
    auditTotalPages.value = res.totalPages
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

// loadNotificationsSummary aggregates notification_send_log over the
// selected lookback window. Cheap because the table is pruned at 90d
// retention; we pull every row in window and roll up client-side rather
// than spinning up dedicated server endpoints. Works identically on
// standalone kiosks (their own log) and the controller (fleet-wide log).
async function loadNotificationsSummary() {
  loading.value = true
  error.value = null
  try {
    const cutoff = new Date(Date.now() - notificationsLookback.value * 24 * 60 * 60 * 1000)
    const cutoffISO = cutoff.toISOString().replace('T', ' ').replace('Z', 'Z')
    const filter = `created >= "${cutoffISO}"`
    const rows = await pb.collection('notification_send_log').getFullList<SendLogRow>({
      filter,
      sort: '-created',
    })

    const totals = { sent: 0, failed: 0, skipped: 0 }
    const byEvent = new Map<string, SendLogByEvent>()
    for (const r of rows) {
      if (r.status === 'sent') totals.sent++
      else if (r.status === 'failed') totals.failed++
      else if (r.status === 'skipped') totals.skipped++

      let bucket = byEvent.get(r.event_type)
      if (!bucket) {
        bucket = { event_type: r.event_type, sent: 0, failed: 0, skipped: 0 }
        byEvent.set(r.event_type, bucket)
      }
      if (r.status === 'sent') bucket.sent++
      else if (r.status === 'failed') bucket.failed++
      else if (r.status === 'skipped') bucket.skipped++
    }

    notificationsTotals.value = totals
    notificationsByEvent.value = Array.from(byEvent.values()).sort((a, b) =>
      a.event_type.localeCompare(b.event_type),
    )
    notificationsRecentFailures.value = rows.filter((r) => r.status === 'failed').slice(0, 10)
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

function successRateLabel(r: SendLogByEvent): string {
  const denom = r.sent + r.failed
  if (denom === 0) return '—'
  return `${Math.round((r.sent / denom) * 100)}%`
}

function successRateClass(r: SendLogByEvent): string {
  const denom = r.sent + r.failed
  if (denom === 0) return 'text-slate-500'
  const rate = r.sent / denom
  if (rate >= 0.99) return 'text-emerald-300'
  if (rate >= 0.9) return 'text-amber-300'
  return 'text-red-400 font-semibold'
}

async function loadLowStock() {
  loading.value = true
  error.value = null
  try {
    // Pull all active items + all open_checkouts in two calls, then compute
    // low-stock client-side. Cheap up to a few thousand items; if the catalog
    // grows beyond that, move this to a server endpoint.
    const [itemsRes, opensRes] = await Promise.all([
      pb.collection('items').getFullList<ItemRecord>({ filter: 'active = true', sort: 'code' }),
      pb.collection('open_checkouts').getFullList<{ item: string }>(),
    ])
    const openByItem: Record<string, number> = {}
    for (const o of opensRes) openByItem[o.item] = (openByItem[o.item] ?? 0) + 1

    const rows: LowStockRow[] = []
    for (const item of itemsRes) {
      const threshold = item.reorder_threshold ?? 0
      let available: number
      let out = 0
      if (item.type === 'tool') {
        out = openByItem[item.id] ?? 0
        available = Math.max(0, (item.quantity_on_hand ?? 0) - out)
      } else {
        available = item.quantity_on_hand ?? 0
      }
      if (threshold > 0 && available <= threshold) {
        rows.push({ item, out, available, deficit: threshold - available })
      }
    }
    rows.sort((a, b) => b.deficit - a.deficit)
    lowStockRows.value = rows
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

async function onRebuild() {
  rebuildOpen.value = false
  rebuilding.value = true
  try {
    const r = await api.post<{ deleted: number; inserted: number }>(
      '/api/kiosk/integrity/rebuild',
    )
    toast.success(`Rebuilt open_checkouts: deleted ${r.deleted}, inserted ${r.inserted}.`)
    await loadCurrentlyOut()
  } catch (e) {
    toast.error(`Rebuild failed: ${(e as Error).message}`)
  } finally {
    rebuilding.value = false
  }
}

async function exportCsv() {
  try {
    await download('/api/kiosk/transactions.csv')
  } catch (e) {
    toast.error(`Export failed: ${(e as Error).message}`)
  }
}

function loadCurrentTab() {
  if (tab.value === 'currently-out') loadCurrentlyOut()
  else if (tab.value === 'aging') loadAging()
  else if (tab.value === 'low-stock') {
    // Controller fans out inventory.snapshot fleet-wide; kiosks compute
    // their local view from items + open_checkouts. Same tab, two code
    // paths because the data sources are fundamentally different (one
    // is centralized, one is local).
    if (isController.value) loadFleetLowStock()
    else loadLowStock()
  }
  else if (tab.value === 'group-activity') loadGroupActivity()
  else if (tab.value === 'recent') loadTransactions(1)
  else if (tab.value === 'audit') loadAudit(1)
  else if (tab.value === 'notifications') loadNotificationsSummary()
}

loadKiosks()

watch(tab, loadCurrentTab, { immediate: true })
watch(kioskFilter, loadCurrentTab)

function formatRelative(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime()
  const days = Math.floor(ms / (1000 * 60 * 60 * 24))
  if (days >= 1) return `${days} day${days === 1 ? '' : 's'} ago`
  const hours = Math.floor(ms / (1000 * 60 * 60))
  if (hours >= 1) return `${hours} hr${hours === 1 ? '' : 's'} ago`
  const mins = Math.max(1, Math.floor(ms / (1000 * 60)))
  return `${mins} min${mins === 1 ? '' : 's'} ago`
}

function formatDateTime(iso: string): string {
  return new Date(iso).toLocaleString()
}

function tabClasses(target: Tab) {
  return target === tab.value
    ? 'border-emerald-500 text-slate-100'
    : 'border-transparent text-slate-400 hover:text-slate-200'
}

const filteredOpen = computed(() => {
  const q = openSearch.value.trim().toLowerCase()
  if (!q) return openRows.value
  return openRows.value.filter((r) => {
    const item = r.expand?.item
    const user = r.expand?.user
    return (
      (item?.code?.toLowerCase().includes(q) ?? false) ||
      (item?.name?.toLowerCase().includes(q) ?? false) ||
      (user?.code?.toLowerCase().includes(q) ?? false) ||
      (user?.name?.toLowerCase().includes(q) ?? false) ||
      (r.serial?.toLowerCase().includes(q) ?? false)
    )
  })
})

function openTxDetail(t: TxRow) {
  selectedTx.value = {
    id: t.id,
    completedAt: t.completed_at,
    userName: t.expand?.user?.name ?? '(unknown)',
    userCode: t.expand?.user?.code ?? '',
    kioskCode: t.kiosk_code,
    locationCode: t.location_code,
  }
  detailOpen.value = true
}
</script>

<template>
  <main class="p-6 max-w-7xl mx-auto w-full">
    <header class="mb-4 flex items-baseline justify-between gap-4">
      <h1 class="text-2xl font-semibold">Reports</h1>
      <label v-if="isController" class="flex items-center gap-2 text-sm">
        <span class="text-slate-400">Kiosk</span>
        <select
          v-model="kioskFilter"
          class="rounded-lg bg-slate-900 border border-slate-700 px-3 py-1.5 text-slate-100"
        >
          <option value="">All kiosks</option>
          <option v-for="k in kiosks" :key="k.id" :value="k.kiosk_code">
            {{ k.kiosk_code }}{{ k.location_code ? ` — ${k.location_code}` : '' }}
          </option>
        </select>
      </label>
    </header>

    <nav class="flex gap-1 mb-4 border-b border-slate-800">
      <button
        type="button"
        class="px-4 py-2 border-b-2 transition-colors"
        :class="tabClasses('currently-out')"
        @click="tab = 'currently-out'"
      >
        Currently out
      </button>
      <button
        type="button"
        class="px-4 py-2 border-b-2 transition-colors"
        :class="tabClasses('aging')"
        @click="tab = 'aging'"
      >
        Aging
      </button>
      <button
        type="button"
        class="px-4 py-2 border-b-2 transition-colors"
        :class="tabClasses('low-stock')"
        @click="tab = 'low-stock'"
      >
        Low stock
      </button>
      <button
        type="button"
        class="px-4 py-2 border-b-2 transition-colors"
        :class="tabClasses('group-activity')"
        @click="tab = 'group-activity'"
      >
        Group activity
      </button>
      <button
        type="button"
        class="px-4 py-2 border-b-2 transition-colors"
        :class="tabClasses('recent')"
        @click="tab = 'recent'"
      >
        Recent transactions
      </button>
      <button
        v-if="isController"
        type="button"
        class="px-4 py-2 border-b-2 transition-colors"
        :class="tabClasses('audit')"
        @click="tab = 'audit'"
      >
        Adjustment audit
      </button>
      <button
        type="button"
        class="px-4 py-2 border-b-2 transition-colors"
        :class="tabClasses('notifications')"
        @click="tab = 'notifications'"
      >
        Notifications
      </button>
    </nav>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 mb-3">
      {{ error }}
    </p>

    <!-- Currently out -->
    <div v-if="tab === 'currently-out'" class="flex flex-col gap-3">
      <input
        v-model="openSearch"
        type="search"
        placeholder="Search by item, worker, or serial"
        class="w-full max-w-md rounded-lg bg-slate-900 border border-slate-700 px-3 py-2 text-slate-100"
      />

      <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
        <table class="w-full text-left text-sm">
          <thead class="bg-slate-950/70 text-slate-400">
            <tr>
              <th class="px-4 py-3 font-medium">Item</th>
              <th class="px-4 py-3 font-medium">Who</th>
              <th class="px-4 py-3 font-medium">Serial</th>
              <th class="px-4 py-3 font-medium">Out for</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-800">
            <tr v-if="loading">
              <td colspan="4" class="text-center text-slate-500 py-8">Loading…</td>
            </tr>
            <tr v-else-if="filteredOpen.length === 0">
              <td colspan="4" class="text-center text-slate-500 py-8">
                {{ openSearch ? 'No matches.' : 'Nothing is currently out.' }}
              </td>
            </tr>
            <tr v-for="r in filteredOpen" :key="r.id" class="hover:bg-slate-800/40">
              <td class="px-4 py-3">
                <div class="font-medium">{{ r.expand?.item?.name }}</div>
                <div class="text-xs text-slate-500 font-mono">{{ r.expand?.item?.code }}</div>
              </td>
              <td class="px-4 py-3">
                <div>{{ r.expand?.user?.name }}</div>
                <div class="text-xs text-slate-500 font-mono">{{ r.expand?.user?.code }}</div>
              </td>
              <td class="px-4 py-3 font-mono text-slate-400">{{ r.serial || '—' }}</td>
              <td
                class="px-4 py-3 text-slate-200 tabular-nums"
                :title="formatDateTime(r.checked_out_at)"
              >
                {{ formatRelative(r.checked_out_at) }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <div v-if="!isController" class="flex justify-end gap-4 mt-2">
        <button
          type="button"
          class="text-xs text-slate-500 hover:text-slate-300 underline-offset-2 hover:underline"
          :disabled="rebuilding"
          @click="rebuildOpen = true"
        >
          {{ rebuilding ? 'Rebuilding…' : 'Rebuild from ledger' }}
        </button>
      </div>
    </div>

    <!-- Aging -->
    <div v-else-if="tab === 'aging'" class="flex flex-col gap-3">
      <div class="flex items-center gap-3 text-sm">
        <label class="flex items-center gap-2 text-slate-300">
          Highlight rows older than
          <input
            v-model.number="agingThresholdDays"
            type="number"
            min="0"
            max="365"
            class="w-20 rounded-lg bg-slate-900 border border-slate-700 px-2 py-1 text-slate-100 tabular-nums"
          />
          days
        </label>
        <span class="text-slate-500">
          {{ agingGroups.reduce((n, g) => n + g.rows.filter((r) => Math.floor((Date.now() - new Date(r.checked_out_at).getTime()) / (1000 * 60 * 60 * 24)) >= agingThresholdDays).length, 0) }}
          row(s) over threshold,
          {{ agingGroups.length }} worker(s) with anything out
        </span>
      </div>

      <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
        <table class="w-full text-left text-sm">
          <thead class="bg-slate-950/70 text-slate-400">
            <tr>
              <th class="px-4 py-3 font-medium">Worker</th>
              <th class="px-4 py-3 font-medium">Item</th>
              <th class="px-4 py-3 font-medium">Serial</th>
              <th class="px-4 py-3 font-medium text-right">Days out</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-800">
            <tr v-if="loading">
              <td colspan="4" class="text-center text-slate-500 py-8">Loading…</td>
            </tr>
            <tr v-else-if="agingGroups.length === 0">
              <td colspan="4" class="text-center text-slate-500 py-8">Nothing is currently out.</td>
            </tr>
            <template v-for="g in agingGroups" :key="g.userId">
              <tr class="bg-slate-950/60">
                <td colspan="4" class="px-4 py-2">
                  <span class="font-semibold text-slate-200">{{ g.userName }}</span>
                  <span class="ml-2 text-xs text-slate-500 font-mono">{{ g.userCode }}</span>
                  <span class="ml-3 text-xs text-slate-400">
                    {{ g.rows.length }} out · oldest
                    <span :class="g.oldestDays >= agingThresholdDays ? 'text-amber-300' : 'text-slate-400'">
                      {{ g.oldestDays }} day{{ g.oldestDays === 1 ? '' : 's' }} ago
                    </span>
                  </span>
                </td>
              </tr>
              <tr v-for="r in g.rows" :key="r.id" class="hover:bg-slate-800/40">
                <td class="px-4 py-2"></td>
                <td class="px-4 py-2">
                  <div>{{ r.expand?.item?.name }}</div>
                  <div class="text-xs text-slate-500 font-mono">{{ r.expand?.item?.code }}</div>
                </td>
                <td class="px-4 py-2 font-mono text-slate-400">{{ r.serial || '—' }}</td>
                <td
                  class="px-4 py-2 text-right tabular-nums"
                  :title="formatDateTime(r.checked_out_at)"
                  :class="
                    Math.floor((Date.now() - new Date(r.checked_out_at).getTime()) / (1000 * 60 * 60 * 24)) >= agingThresholdDays
                      ? 'text-amber-300 font-semibold'
                      : 'text-slate-300'
                  "
                >
                  {{ Math.floor((Date.now() - new Date(r.checked_out_at).getTime()) / (1000 * 60 * 60 * 24)) }}
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Low stock (fleet-wide, snapshot fan-out) -->
    <div v-else-if="tab === 'low-stock' && isController" class="flex flex-col gap-3">
      <p
        v-if="fleetLowStockErrors.length > 0"
        class="rounded-lg bg-amber-950/40 border border-amber-800/60 text-amber-200 text-sm px-4 py-2"
      >
        Partial result — {{ fleetLowStockErrors.length }}
        {{ fleetLowStockErrors.length === 1 ? 'kiosk' : 'kiosks' }} excluded
        ({{ fleetLowStockErrors.map((e) => e.kiosk_code).join(', ') }}). Offline
        kiosks don&rsquo;t respond to the live snapshot.
      </p>
      <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
        <table class="w-full text-left text-sm">
          <thead class="bg-slate-950/70 text-slate-400">
            <tr>
              <th class="px-4 py-3 font-medium">Kiosk</th>
              <th class="px-4 py-3 font-medium">Item</th>
              <th class="px-4 py-3 font-medium text-right">On hand</th>
              <th class="px-4 py-3 font-medium text-right">Out</th>
              <th class="px-4 py-3 font-medium text-right">Available</th>
              <th class="px-4 py-3 font-medium text-right">Threshold</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-800">
            <tr v-if="loading">
              <td colspan="6" class="text-center text-slate-500 py-8">Loading…</td>
            </tr>
            <tr v-else-if="fleetLowStockRows.length === 0">
              <td colspan="6" class="text-center text-slate-500 py-8">
                Nothing is low across the fleet. Set a reorder threshold on items to enable alerts.
              </td>
            </tr>
            <tr v-for="r in fleetLowStockRows" :key="`${r.kiosk_code}::${r.item_code}`" class="hover:bg-slate-800/40">
              <td class="px-4 py-3 font-mono text-slate-300">{{ r.kiosk_code }}</td>
              <td class="px-4 py-3">
                <div class="font-medium">{{ r.item_name }}</div>
                <div class="text-xs text-slate-500 font-mono">{{ r.item_code }}</div>
              </td>
              <td class="px-4 py-3 text-right tabular-nums text-slate-300">{{ r.quantity_on_hand }}</td>
              <td class="px-4 py-3 text-right tabular-nums text-slate-400">{{ r.out }}</td>
              <td
                class="px-4 py-3 text-right tabular-nums font-semibold"
                :class="r.available <= 0 ? 'text-red-400' : 'text-amber-400'"
              >
                {{ r.available }}
              </td>
              <td class="px-4 py-3 text-right tabular-nums text-slate-400">{{ r.reorder_threshold }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Low stock (standalone kiosk, computed locally) -->
    <div v-else-if="tab === 'low-stock'" class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
      <table class="w-full text-left text-sm">
        <thead class="bg-slate-950/70 text-slate-400">
          <tr>
            <th class="px-4 py-3 font-medium">Item</th>
            <th class="px-4 py-3 font-medium">Type</th>
            <th class="px-4 py-3 font-medium text-right">On hand</th>
            <th class="px-4 py-3 font-medium text-right">Out</th>
            <th class="px-4 py-3 font-medium text-right">Available</th>
            <th class="px-4 py-3 font-medium text-right">Threshold</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-slate-800">
          <tr v-if="loading">
            <td colspan="6" class="text-center text-slate-500 py-8">Loading…</td>
          </tr>
          <tr v-else-if="lowStockRows.length === 0">
            <td colspan="6" class="text-center text-slate-500 py-8">
              Nothing is low. Set a reorder threshold on items to enable alerts.
            </td>
          </tr>
          <tr v-for="r in lowStockRows" :key="r.item.id" class="hover:bg-slate-800/40">
            <td class="px-4 py-3">
              <div class="font-medium">{{ r.item.name }}</div>
              <div class="text-xs text-slate-500 font-mono">{{ r.item.code }}</div>
            </td>
            <td class="px-4 py-3 text-slate-400 capitalize">{{ r.item.type }}</td>
            <td class="px-4 py-3 text-right tabular-nums text-slate-300">{{ r.item.quantity_on_hand }}</td>
            <td class="px-4 py-3 text-right tabular-nums text-slate-400">
              {{ r.item.type === 'tool' ? r.out : '—' }}
            </td>
            <td
              class="px-4 py-3 text-right tabular-nums font-semibold"
              :class="r.available <= 0 ? 'text-red-400' : 'text-amber-400'"
            >
              {{ r.available }}
            </td>
            <td class="px-4 py-3 text-right tabular-nums text-slate-400">{{ r.item.reorder_threshold }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- Group activity -->
    <div v-else-if="tab === 'group-activity'" class="flex flex-col gap-3">
      <div class="flex items-end gap-3 text-sm">
        <label class="flex flex-col gap-1">
          <span class="text-slate-400 text-xs">From</span>
          <input
            v-model="groupActivityFrom"
            type="date"
            class="rounded-lg bg-slate-900 border border-slate-700 px-2 py-1 text-slate-100"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-slate-400 text-xs">To</span>
          <input
            v-model="groupActivityTo"
            type="date"
            class="rounded-lg bg-slate-900 border border-slate-700 px-2 py-1 text-slate-100"
          />
        </label>
        <button
          type="button"
          class="px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          :disabled="loading"
          @click="loadGroupActivity"
        >
          Apply
        </button>
        <span class="text-slate-500 text-xs ml-auto">
          Rolls up by the group code snapshotted on each transaction at commit time;
          renames after the fact don't change history.
        </span>
      </div>

      <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
        <table class="w-full text-left text-sm">
          <thead class="bg-slate-950/70 text-slate-400">
            <tr>
              <th class="px-4 py-3 font-medium">Group</th>
              <th class="px-4 py-3 font-medium">Contact</th>
              <th class="px-4 py-3 font-medium text-right">Transactions</th>
              <th class="px-4 py-3 font-medium text-right">Checked out</th>
              <th class="px-4 py-3 font-medium text-right">Returned</th>
              <th class="px-4 py-3 font-medium text-right">Consumed</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-800">
            <tr v-if="loading">
              <td colspan="6" class="text-center text-slate-500 py-8">Loading…</td>
            </tr>
            <tr v-else-if="groupActivityRows.length === 0">
              <td colspan="6" class="text-center text-slate-500 py-8">
                No completed transactions in the selected range.
              </td>
            </tr>
            <tr v-for="r in groupActivityRows" :key="r.code || '__ungrouped__'" class="hover:bg-slate-800/40">
              <td class="px-4 py-3">
                <div class="font-medium">{{ r.name }}</div>
                <div class="text-xs text-slate-500 font-mono">{{ r.code || '—' }}</div>
              </td>
              <td class="px-4 py-3 text-slate-400">{{ r.contactEmail || '—' }}</td>
              <td class="px-4 py-3 text-right tabular-nums text-slate-200">{{ r.transactions }}</td>
              <td class="px-4 py-3 text-right tabular-nums text-slate-300">{{ r.checkedOut }}</td>
              <td class="px-4 py-3 text-right tabular-nums text-slate-300">{{ r.returned }}</td>
              <td class="px-4 py-3 text-right tabular-nums text-slate-300">{{ r.consumed }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Recent transactions -->
    <div v-else-if="tab === 'recent'" class="flex flex-col gap-3">
      <div class="flex justify-end">
        <button
          type="button"
          class="px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
          @click="exportCsv"
        >
          Export CSV
        </button>
      </div>
      <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
      <table class="w-full text-left text-sm">
        <thead class="bg-slate-950/70 text-slate-400">
          <tr>
            <th class="px-4 py-3 font-medium">Completed</th>
            <th class="px-4 py-3 font-medium">Who</th>
            <th class="px-4 py-3 font-medium">Kiosk</th>
            <th class="px-4 py-3 font-medium">Location</th>
            <th class="px-4 py-3 font-medium">Lines</th>
            <th class="px-4 py-3 font-medium">Status</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-slate-800">
          <tr v-if="loading">
            <td colspan="6" class="text-center text-slate-500 py-8">Loading…</td>
          </tr>
          <tr v-else-if="txRows.length === 0">
            <td colspan="6" class="text-center text-slate-500 py-8">No transactions yet.</td>
          </tr>
          <tr
            v-for="t in txRows"
            :key="t.id"
            class="hover:bg-slate-800/40 cursor-pointer"
            @click="openTxDetail(t)"
          >
            <td class="px-4 py-3 text-slate-300">{{ formatDateTime(t.completed_at) }}</td>
            <td class="px-4 py-3">
              <div>{{ t.expand?.user?.name }}</div>
              <div class="text-xs text-slate-500 font-mono">{{ t.expand?.user?.code }}</div>
            </td>
            <td class="px-4 py-3 font-mono text-slate-400">{{ t.kiosk_code }}</td>
            <td class="px-4 py-3 text-slate-400">{{ t.location_code }}</td>
            <td class="px-4 py-3 tabular-nums text-slate-300">{{ t.lines_count }}</td>
            <td class="px-4 py-3">
              <span class="inline-block px-2 py-0.5 rounded text-xs bg-emerald-900/60 text-emerald-200">
                {{ t.status }}
              </span>
            </td>
          </tr>
        </tbody>
      </table>

      <div
        v-if="txTotalPages > 1"
        class="flex items-center justify-between px-4 py-3 border-t border-slate-800 text-sm"
      >
        <button
          type="button"
          class="px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 disabled:opacity-40"
          :disabled="txPage <= 1 || loading"
          @click="loadTransactions(txPage - 1)"
        >
          Previous
        </button>
        <span class="text-slate-400">Page {{ txPage }} of {{ txTotalPages }}</span>
        <button
          type="button"
          class="px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 disabled:opacity-40"
          :disabled="txPage >= txTotalPages || loading"
          @click="loadTransactions(txPage + 1)"
        >
          Next
        </button>
      </div>
      </div>
    </div>

    <!-- Adjustment audit (controller-only) -->
    <div v-else-if="tab === 'audit'" class="flex flex-col gap-3">
      <div class="flex items-end gap-3 text-sm flex-wrap">
        <label class="flex flex-col gap-1">
          <span class="text-slate-400 text-xs">From</span>
          <input
            v-model="auditFrom"
            type="date"
            class="rounded-lg bg-slate-900 border border-slate-700 px-2 py-1 text-slate-100"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-slate-400 text-xs">To</span>
          <input
            v-model="auditTo"
            type="date"
            class="rounded-lg bg-slate-900 border border-slate-700 px-2 py-1 text-slate-100"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-slate-400 text-xs">Source</span>
          <select
            v-model="auditSourceFilter"
            class="rounded-lg bg-slate-900 border border-slate-700 px-2 py-1 text-slate-100"
          >
            <option value="">All sources</option>
            <option value="local">Local (at kiosk)</option>
            <option value="controller">Controller (remote)</option>
          </select>
        </label>
        <button
          type="button"
          class="px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          :disabled="loading"
          @click="loadAudit(1)"
        >
          Apply
        </button>
        <span class="text-slate-500 text-xs ml-auto">
          Every stock adjustment fan-out, every kiosk. Append-only audit projected from
          <code class="font-mono text-xs">inventory.adjust</code> events.
        </span>
      </div>

      <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
        <table class="w-full text-left text-sm">
          <thead class="bg-slate-950/70 text-slate-400">
            <tr>
              <th class="px-4 py-3 font-medium">When</th>
              <th class="px-4 py-3 font-medium">Kiosk</th>
              <th class="px-4 py-3 font-medium">Item</th>
              <th class="px-4 py-3 font-medium">Source</th>
              <th class="px-4 py-3 font-medium text-right">Δ</th>
              <th class="px-4 py-3 font-medium text-right">Prev → New</th>
              <th class="px-4 py-3 font-medium">Reason</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-800">
            <tr v-if="loading">
              <td colspan="7" class="text-center text-slate-500 py-8">Loading…</td>
            </tr>
            <tr v-else-if="auditRows.length === 0">
              <td colspan="7" class="text-center text-slate-500 py-8">
                No adjustments in the selected window.
              </td>
            </tr>
            <tr v-for="r in auditRows" :key="r.id" class="hover:bg-slate-800/40">
              <td class="px-4 py-3 text-slate-300">{{ formatDateTime(r.created) }}</td>
              <td class="px-4 py-3 font-mono text-slate-400">{{ r.kiosk_code }}</td>
              <td class="px-4 py-3">
                <div class="font-medium">{{ r.item_name || '—' }}</div>
                <div class="text-xs text-slate-500 font-mono">{{ r.item_code }}</div>
              </td>
              <td class="px-4 py-3">
                <span
                  class="inline-block px-2 py-0.5 rounded text-xs"
                  :class="r.source === 'controller'
                    ? 'bg-sky-900/60 text-sky-200'
                    : 'bg-slate-800/80 text-slate-300'"
                >
                  {{ r.source || 'local' }}
                </span>
              </td>
              <td
                class="px-4 py-3 text-right tabular-nums font-semibold"
                :class="r.delta < 0 ? 'text-red-400' : r.delta > 0 ? 'text-emerald-400' : 'text-slate-400'"
              >
                {{ r.delta > 0 ? '+' : '' }}{{ r.delta }}
              </td>
              <td class="px-4 py-3 text-right tabular-nums text-slate-300">
                {{ r.prev_quantity }} → {{ r.new_quantity }}
              </td>
              <td class="px-4 py-3 text-slate-400">{{ r.reason || '—' }}</td>
            </tr>
          </tbody>
        </table>

        <div
          v-if="auditTotalPages > 1"
          class="flex items-center justify-between px-4 py-3 border-t border-slate-800 text-sm"
        >
          <button
            type="button"
            class="px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 disabled:opacity-40"
            :disabled="auditPage <= 1 || loading"
            @click="loadAudit(auditPage - 1)"
          >
            Previous
          </button>
          <span class="text-slate-400">Page {{ auditPage }} of {{ auditTotalPages }}</span>
          <button
            type="button"
            class="px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 disabled:opacity-40"
            :disabled="auditPage >= auditTotalPages || loading"
            @click="loadAudit(auditPage + 1)"
          >
            Next
          </button>
        </div>
      </div>
    </div>

    <!-- Notifications deliverability -->
    <div v-else-if="tab === 'notifications'" class="flex flex-col gap-3">
      <div class="flex items-end gap-3 text-sm flex-wrap">
        <label class="flex flex-col gap-1">
          <span class="text-slate-400 text-xs">Lookback</span>
          <select
            v-model="notificationsLookback"
            class="rounded-lg bg-slate-900 border border-slate-700 px-2 py-1 text-slate-100"
            @change="loadNotificationsSummary"
          >
            <option :value="1">Last 24 hours</option>
            <option :value="7">Last 7 days</option>
            <option :value="30">Last 30 days</option>
            <option :value="90">Last 90 days</option>
          </select>
        </label>
        <span class="text-slate-500 text-xs ml-auto">
          Aggregated from <code class="font-mono text-xs">notification_send_log</code>; see
          <RouterLink :to="{ name: 'admin-notifications-log' }" class="underline">Recent sends</RouterLink>
          for the per-recipient list.
        </span>
      </div>

      <div class="grid grid-cols-1 md:grid-cols-3 gap-3">
        <div class="rounded-2xl bg-slate-900 border border-slate-800 p-4">
          <div class="text-xs text-slate-400 uppercase tracking-wider mb-1">Sent</div>
          <div class="text-2xl font-semibold text-emerald-400 tabular-nums">{{ notificationsTotals.sent }}</div>
        </div>
        <div class="rounded-2xl bg-slate-900 border border-slate-800 p-4">
          <div class="text-xs text-slate-400 uppercase tracking-wider mb-1">Failed</div>
          <div
            class="text-2xl font-semibold tabular-nums"
            :class="notificationsTotals.failed > 0 ? 'text-red-400' : 'text-slate-500'"
          >
            {{ notificationsTotals.failed }}
          </div>
        </div>
        <div class="rounded-2xl bg-slate-900 border border-slate-800 p-4">
          <div class="text-xs text-slate-400 uppercase tracking-wider mb-1">Skipped</div>
          <div class="text-2xl font-semibold text-slate-400 tabular-nums">{{ notificationsTotals.skipped }}</div>
        </div>
      </div>

      <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
        <table class="w-full text-left text-sm">
          <thead class="bg-slate-950/70 text-slate-400">
            <tr>
              <th class="px-4 py-3 font-medium">Event</th>
              <th class="px-4 py-3 font-medium text-right">Sent</th>
              <th class="px-4 py-3 font-medium text-right">Failed</th>
              <th class="px-4 py-3 font-medium text-right">Skipped</th>
              <th class="px-4 py-3 font-medium text-right">Success rate</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-800">
            <tr v-if="loading">
              <td colspan="5" class="text-center text-slate-500 py-8">Loading…</td>
            </tr>
            <tr v-else-if="notificationsByEvent.length === 0">
              <td colspan="5" class="text-center text-slate-500 py-8">
                Nothing sent in the selected window.
              </td>
            </tr>
            <tr v-for="r in notificationsByEvent" :key="r.event_type" class="hover:bg-slate-800/40">
              <td class="px-4 py-3 font-mono text-slate-200">{{ r.event_type }}</td>
              <td class="px-4 py-3 text-right tabular-nums text-emerald-300">{{ r.sent }}</td>
              <td
                class="px-4 py-3 text-right tabular-nums"
                :class="r.failed > 0 ? 'text-red-400 font-semibold' : 'text-slate-500'"
              >
                {{ r.failed }}
              </td>
              <td class="px-4 py-3 text-right tabular-nums text-slate-400">{{ r.skipped }}</td>
              <td
                class="px-4 py-3 text-right tabular-nums"
                :class="successRateClass(r)"
              >
                {{ successRateLabel(r) }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <div v-if="notificationsRecentFailures.length > 0" class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
        <div class="px-4 py-3 bg-slate-950/70 text-slate-400 text-xs uppercase tracking-wider">
          Recent failures
        </div>
        <ul class="divide-y divide-slate-800">
          <li v-for="f in notificationsRecentFailures" :key="f.id" class="px-4 py-3 text-sm">
            <div class="flex items-baseline gap-3">
              <span class="font-mono text-slate-300">{{ f.event_type }}</span>
              <span class="text-slate-400">→ {{ f.recipient || '(no recipient)' }}</span>
              <span class="text-slate-500 text-xs ml-auto">{{ formatDateTime(f.created) }}</span>
            </div>
            <div v-if="f.error" class="text-xs text-red-300 mt-1 font-mono">{{ f.error }}</div>
          </li>
        </ul>
      </div>
    </div>

    <ConfirmDialog
      :open="rebuildOpen"
      title="Rebuild open checkouts"
      message="This deletes every row in open_checkouts and rebuilds the table from the transaction ledger. Use only if Integrity reports drift. Continue?"
      confirm-label="Rebuild"
      destructive
      @update:open="rebuildOpen = $event"
      @confirm="onRebuild"
    />

    <TransactionDetailDialog
      :open="detailOpen"
      :transaction="selectedTx"
      @update:open="detailOpen = $event"
    />
  </main>
</template>
