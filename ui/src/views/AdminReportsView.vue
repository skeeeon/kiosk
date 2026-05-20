<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import TransactionDetailDialog, {
  type TxSummary,
} from '../components/TransactionDetailDialog.vue'

type Tab = 'currently-out' | 'recent'
const tab = ref<Tab>('currently-out')

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

const openRows = ref<OpenRow[]>([])
const openSearch = ref('')
const txRows = ref<TxRow[]>([])
// Line counts are fetched per-row after the page loads. Keyed by transaction
// id; undefined means "not loaded yet" so the cell shows a dash rather than 0.
const txLineCounts = ref<Record<string, number>>({})
const txPage = ref(1)
const txTotalPages = ref(1)
const loading = ref(false)
const error = ref<string | null>(null)

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
async function loadLineCounts(ids: string[]) {
  txLineCounts.value = {}
  const results = await Promise.all(
    ids.map((id) =>
      pb
        .collection('transaction_lines')
        .getList(1, 1, { filter: `transaction = "${id}"` })
        .then((r) => [id, r.totalItems] as const)
        .catch(() => [id, -1] as const),
    ),
  )
  const next: Record<string, number> = {}
  for (const [id, n] of results) next[id] = n
  txLineCounts.value = next
}

watch(
  tab,
  (t) => {
    if (t === 'currently-out') loadCurrentlyOut()
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
              <td class="px-4 py-3 text-slate-300" :title="formatDateTime(r.checked_out_at)">
                {{ formatRelative(r.checked_out_at) }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Recent transactions -->
    <div v-else class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
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

    <TransactionDetailDialog
      :open="detailOpen"
      :transaction="selectedTx"
      @update:open="detailOpen = $event"
    />
  </main>
</template>
