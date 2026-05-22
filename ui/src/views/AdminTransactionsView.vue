<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import { download } from '../lib/api'
import { useAdminToast } from '../composables/useAdminToast'
import TransactionDetailDialog, {
  type TxSummary,
} from '../components/TransactionDetailDialog.vue'
import type { KioskRecord } from '../types'

const toast = useAdminToast()

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
const totalPages = ref(1)
const loading = ref(false)
const error = ref<string | null>(null)

// Filters
const kioskFilter = ref('') // empty = all kiosks
const fromFilter = ref('')  // <input type="date"> values; converted to RFC3339 on query
const toFilter = ref('')

const selectedTx = ref<TxSummary | null>(null)
const detailOpen = ref(false)

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
    const res = await pb.collection('transactions').getList<TxRow>(toPage, 50, {
      filter: filters.join(' && '),
      sort: '-completed_at',
      expand: 'user',
    })
    rows.value = res.items
    page.value = res.page
    totalPages.value = res.totalPages
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
      <div>
        <h1 class="text-2xl font-semibold">Transactions</h1>
        <p class="text-sm text-slate-400">Aggregated from across the fleet.</p>
      </div>
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

    <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
      <table class="w-full text-left text-sm">
        <thead class="bg-slate-950/70 text-slate-400">
          <tr>
            <th class="px-4 py-3 font-medium">Completed</th>
            <th class="px-4 py-3 font-medium">Kiosk</th>
            <th class="px-4 py-3 font-medium">Worker</th>
            <th class="px-4 py-3 font-medium text-right">Lines</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-slate-800">
          <tr v-if="loading">
            <td colspan="4" class="text-center text-slate-500 py-8">Loading…</td>
          </tr>
          <tr v-else-if="rows.length === 0">
            <td colspan="4" class="text-center text-slate-500 py-8">
              No transactions match the filters.
            </td>
          </tr>
          <tr
            v-for="t in rows"
            :key="t.id"
            class="hover:bg-slate-800/50 cursor-pointer"
            @click="openTxDetail(t)"
          >
            <td class="px-4 py-3 text-slate-300 tabular-nums">
              {{ new Date(t.completed_at).toLocaleString() }}
            </td>
            <td class="px-4 py-3 font-mono text-slate-200">
              {{ t.source_kiosk_code || t.kiosk_code }}
            </td>
            <td class="px-4 py-3 text-slate-300">
              <span class="font-mono text-slate-400">{{ t.expand?.user?.code || '—' }}</span>
              <span v-if="t.expand?.user?.name" class="ml-2">{{ t.expand.user.name }}</span>
            </td>
            <td class="px-4 py-3 text-right tabular-nums text-slate-300">{{ t.lines_count }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-if="totalPages > 1" class="flex justify-between items-center mt-4 text-sm text-slate-400">
      <button
        type="button"
        class="px-3 py-1.5 rounded bg-slate-800 hover:bg-slate-700 text-slate-200 disabled:opacity-50 disabled:cursor-not-allowed"
        :disabled="page <= 1"
        @click="loadTransactions(page - 1)"
      >
        Previous
      </button>
      <span>Page {{ page }} of {{ totalPages }}</span>
      <button
        type="button"
        class="px-3 py-1.5 rounded bg-slate-800 hover:bg-slate-700 text-slate-200 disabled:opacity-50 disabled:cursor-not-allowed"
        :disabled="page >= totalPages"
        @click="loadTransactions(page + 1)"
      >
        Next
      </button>
    </div>

    <TransactionDetailDialog
      :open="detailOpen"
      :transaction="selectedTx"
      @update:open="(v) => { detailOpen = v; if (!v) selectedTx = null }"
    />
  </main>
</template>
