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

type Tab = 'currently-out' | 'aging' | 'low-stock' | 'group-activity' | 'recent'
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
  else if (tab.value === 'low-stock') loadLowStock()
  else if (tab.value === 'group-activity') loadGroupActivity()
  else if (tab.value === 'recent') loadTransactions(1)
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

    <!-- Low stock (kiosk-local data, not projected to controller in v1) -->
    <div
      v-else-if="tab === 'low-stock' && isController"
      class="rounded-2xl bg-amber-950/30 border border-amber-800/60 p-6 text-amber-200 text-sm"
    >
      Low-stock reporting reads each kiosk's local <code class="font-mono text-xs">quantity_on_hand</code>
      and <code class="font-mono text-xs">reorder_threshold</code>, which the controller doesn't project
      in v1. Check the Reports view on the kiosk itself for now — controller-side fleet rollup is on
      the roadmap.
    </div>

    <!-- Low stock -->
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
