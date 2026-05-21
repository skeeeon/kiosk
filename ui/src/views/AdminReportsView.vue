<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import { api } from '../lib/api'
import { useAdminToast } from '../composables/useAdminToast'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import TransactionDetailDialog, {
  type TxSummary,
} from '../components/TransactionDetailDialog.vue'
import type { ItemRecord } from '../types'

type Tab = 'currently-out' | 'low-stock' | 'recent'
const tab = ref<Tab>('currently-out')
const toast = useAdminToast()

interface OpenRow {
  id: string
  serial: string
  checked_out_at: string
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
  expand?: { user?: { id: string; code: string; name: string } }
}

interface LowStockRow {
  item: ItemRecord
  out: number // open_checkouts count (tools only)
  available: number
  deficit: number // threshold - available; positive means low
}

const openRows = ref<OpenRow[]>([])
const openSearch = ref('')
const txRows = ref<TxRow[]>([])
// Line counts are fetched per-row after the page loads. Keyed by transaction
// id; undefined means "not loaded yet" so the cell shows a dash rather than 0.
const txLineCounts = ref<Record<string, number>>({})
const txPage = ref(1)
const txTotalPages = ref(1)
const lowStockRows = ref<LowStockRow[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

const rebuildOpen = ref(false)
const rebuilding = ref(false)

const selectedTx = ref<TxSummary | null>(null)
const detailOpen = ref(false)

async function loadCurrentlyOut() {
  loading.value = true
  error.value = null
  try {
    const res = await pb.collection('open_checkouts').getList<OpenRow>(1, 500, {
      expand: 'item,user',
      sort: '+checked_out_at',
    })
    openRows.value = res.items
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
    const res = await pb.collection('transactions').getList<TxRow>(page, 50, {
      filter: 'status = "completed"',
      sort: '-completed_at',
      expand: 'user',
    })
    txRows.value = res.items
    txPage.value = res.page
    txTotalPages.value = res.totalPages
    void loadLineCounts(res.items.map((t) => t.id))
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

// Fetches line counts for the current page in parallel. Each `getList` call
// with perPage=1 returns `totalItems`, which avoids fetching the full payload
// just to count rows. Cheap enough for 50 rows per page.
//
// requestKey: null disables the PB SDK's per-endpoint auto-cancellation —
// without it, the SDK treats N parallel calls to the same collection as
// duplicates and cancels all but the latest, leaving every cell as a "?".
async function loadLineCounts(ids: string[]) {
  txLineCounts.value = {}
  const results = await Promise.all(
    ids.map((id) =>
      pb
        .collection('transaction_lines')
        .getList(1, 1, { filter: `transaction = "${id}"`, requestKey: null })
        .then((r) => [id, r.totalItems] as const)
        .catch(() => [id, -1] as const),
    ),
  )
  const next: Record<string, number> = {}
  for (const [id, n] of results) next[id] = n
  txLineCounts.value = next
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

function csvExportUrl(): string {
  return '/api/kiosk/transactions.csv'
}

watch(
  tab,
  (t) => {
    if (t === 'currently-out') loadCurrentlyOut()
    else if (t === 'low-stock') loadLowStock()
    else if (t === 'recent') loadTransactions(1)
  },
  { immediate: true },
)

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
    <header class="mb-4">
      <h1 class="text-2xl font-semibold">Reports</h1>
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
        :class="tabClasses('low-stock')"
        @click="tab = 'low-stock'"
      >
        Low stock
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
              <th class="px-4 py-3 font-medium">Since</th>
              <th class="px-4 py-3 font-medium">Duration</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-800">
            <tr v-if="loading">
              <td colspan="5" class="text-center text-slate-500 py-8">Loading…</td>
            </tr>
            <tr v-else-if="filteredOpen.length === 0">
              <td colspan="5" class="text-center text-slate-500 py-8">
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
              <td class="px-4 py-3 text-slate-400" :title="formatDateTime(r.checked_out_at)">
                {{ formatDateTime(r.checked_out_at) }}
              </td>
              <td class="px-4 py-3 text-slate-200 tabular-nums">
                {{ formatRelative(r.checked_out_at) }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="flex justify-end mt-2">
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

    <!-- Recent transactions -->
    <div v-else-if="tab === 'recent'" class="flex flex-col gap-3">
      <div class="flex justify-end">
        <a
          :href="csvExportUrl()"
          class="px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
        >
          Export CSV
        </a>
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
            <td class="px-4 py-3 tabular-nums text-slate-300">
              <template v-if="txLineCounts[t.id] === undefined">—</template>
              <template v-else-if="txLineCounts[t.id] < 0">?</template>
              <template v-else>{{ txLineCounts[t.id] }}</template>
            </td>
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
