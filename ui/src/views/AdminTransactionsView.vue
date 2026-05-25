<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import { download } from '../lib/api'
import { useToast } from '../composables/useToast'
import TransactionDetailDialog, {
  type TxSummary,
} from '../components/TransactionDetailDialog.vue'
import DataTable, { type ColumnDef } from '../components/DataTable.vue'
import { useUrlQuerySync } from '../composables/useUrlQuerySync'
import type { KioskRecord } from '../types'

const toast = useToast()

interface TxRow {
  id: string
  source_kiosk_code: string
  source_transaction_id: string
  kiosk_code: string
  location_code: string
  started_at: string
  completed_at: string
  status: string
  lines_count: number
  expand?: { user?: { id: string; code: string; name: string } }
}

const kiosks = ref<KioskRecord[]>([])
const rows = ref<TxRow[]>([])
const page = ref(1)
const perPage = ref(50)
const total = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

const columns: ColumnDef[] = [
  { key: 'completed', label: 'Completed' },
  { key: 'kiosk', label: 'Kiosk' },
  { key: 'worker', label: 'Worker' },
  { key: 'lines', label: 'Lines', align: 'right' },
]

// Filters
const kioskFilter = ref('') // empty = all kiosks
const fromFilter = ref('')  // <input type="date"> values; converted to RFC3339 on query
const toFilter = ref('')

const selectedTx = ref<TxSummary | null>(null)
const detailOpen = ref(false)

useUrlQuerySync({
  page: { ref: page, default: 1, parse: (v) => Number(v) || 1 },
  kiosk: { ref: kioskFilter, default: '' },
  from: { ref: fromFilter, default: '' },
  to: { ref: toFilter, default: '' },
})

async function loadKiosks() {
  try {
    kiosks.value = await pb.collection('kiosks').getFullList<KioskRecord>({
      sort: '+kiosk_code',
    })
  } catch {
    // Non-fatal — the dropdown just stays empty.
  }
}

// dateBoundary converts a yyyy-mm-dd value to RFC3339 at the start or end of
// day in UTC. Empty input returns empty.
function dateBoundary(d: string, end: boolean): string {
  if (!d) return ''
  return end ? `${d}T23:59:59Z` : `${d}T00:00:00Z`
}

async function loadTransactions(toPage = 1) {
  loading.value = true
  error.value = null
  try {
    const filters: string[] = ['status = "completed"']
    if (kioskFilter.value) {
      filters.push(`source_kiosk_code = "${kioskFilter.value.replace(/"/g, '\\"')}"`)
    }
    if (fromFilter.value) {
      filters.push(`completed_at >= "${dateBoundary(fromFilter.value, false)}"`)
    }
    if (toFilter.value) {
      filters.push(`completed_at <= "${dateBoundary(toFilter.value, true)}"`)
    }
    const res = await pb.collection('transactions').getList<TxRow>(toPage, perPage.value, {
      filter: filters.join(' && '),
      sort: '-completed_at',
      expand: 'user',
    })
    rows.value = res.items
    page.value = res.page
    total.value = res.totalItems
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  await loadKiosks()
  await loadTransactions(1)
})

watch([kioskFilter, fromFilter, toFilter], () => {
  void loadTransactions(1)
})

function openTxDetail(t: TxRow) {
  selectedTx.value = {
    id: t.id,
    completedAt: t.completed_at,
    userName: t.expand?.user?.name ?? '(unknown)',
    userCode: t.expand?.user?.code ?? '',
    kioskCode: t.source_kiosk_code || t.kiosk_code,
    locationCode: t.location_code,
  }
  detailOpen.value = true
}

async function exportCsv() {
  try {
    const params = new URLSearchParams()
    if (fromFilter.value) params.set('from', dateBoundary(fromFilter.value, false))
    if (toFilter.value) params.set('to', dateBoundary(toFilter.value, true))
    const qs = params.toString()
    await download(`/api/kiosk/transactions.csv${qs ? `?${qs}` : ''}`)
  } catch (e) {
    toast.error(`Export failed: ${(e as Error).message}`)
  }
}

const filteredCountLabel = computed(() => {
  if (loading.value) return 'Loading…'
  const n = rows.value.length
  return n === 1 ? '1 transaction' : `${n} transactions`
})
</script>

<template>
  <main class="p-6 max-w-7xl mx-auto w-full">
    <header class="flex items-baseline justify-between mb-4">
      <h1 class="text-2xl font-semibold">Transactions</h1>
      <button
        type="button"
        class="px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
        @click="exportCsv"
      >
        Export CSV
      </button>
    </header>

    <div class="flex flex-wrap gap-3 mb-4 items-end">
      <label class="flex flex-col gap-1">
        <span class="text-xs text-slate-400">Kiosk</span>
        <select
          v-model="kioskFilter"
          class="rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100"
        >
          <option value="">All kiosks</option>
          <option v-for="k in kiosks" :key="k.id" :value="k.kiosk_code">
            {{ k.kiosk_code }}{{ k.location_code ? ` — ${k.location_code}` : '' }}
          </option>
        </select>
      </label>
      <label class="flex flex-col gap-1">
        <span class="text-xs text-slate-400">From</span>
        <input
          v-model="fromFilter"
          type="date"
          class="rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100"
        />
      </label>
      <label class="flex flex-col gap-1">
        <span class="text-xs text-slate-400">To</span>
        <input
          v-model="toFilter"
          type="date"
          class="rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100"
        />
      </label>
      <span class="ml-auto text-sm text-slate-400">{{ filteredCountLabel }}</span>
    </div>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 mb-3">
      {{ error }}
    </p>

    <DataTable
      :columns="columns"
      :rows="rows"
      :row-key="(t) => t.id"
      :loading="loading"
      empty-text="No transactions match the filters."
      row-clickable
      :page="page"
      :per-page="perPage"
      :total="total"
      @row-click="openTxDetail"
      @update:page="(p) => loadTransactions(p)"
      @update:per-page="(n) => { perPage = n; loadTransactions(1) }"
    >
      <template #cell-completed="{ row }">
        <span class="text-slate-300 tabular-nums">{{ new Date(row.completed_at).toLocaleString() }}</span>
      </template>
      <template #cell-kiosk="{ row }">
        <span class="font-mono text-slate-200">{{ row.source_kiosk_code || row.kiosk_code }}</span>
      </template>
      <template #cell-worker="{ row }">
        <span class="font-mono text-slate-400">{{ row.expand?.user?.code || '—' }}</span>
        <span v-if="row.expand?.user?.name" class="ml-2 text-slate-300">{{ row.expand.user.name }}</span>
      </template>
      <template #cell-lines="{ row }">
        <span class="tabular-nums text-slate-300">{{ row.lines_count }}</span>
      </template>
    </DataTable>

    <TransactionDetailDialog
      :open="detailOpen"
      :transaction="selectedTx"
      @update:open="(v) => { detailOpen = v; if (!v) selectedTx = null }"
    />
  </main>
</template>
