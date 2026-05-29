<!-- WorkerHistoryDialog shows one worker's completed transactions, paginated.
     Click a row to drill into the per-line detail via the existing
     TransactionDetailDialog. The transaction history is read directly from
     PB; on the controller this surfaces every transaction the aggregator
     has projected, on a standalone kiosk it's the local ledger. -->
<script setup lang="ts">
import { ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import DataTable, { type ColumnDef } from './DataTable.vue'
import TransactionDetailDialog, { type TxSummary } from './TransactionDetailDialog.vue'
import { pb } from '../lib/pb'
import type { WorkerRecord } from '../types'

interface TxRow {
  id: string
  kiosk_code: string
  location_code: string
  completed_at: string
  status: string
  lines_count: number
  user_group: string
}

const props = withDefaults(
  defineProps<{
    open: boolean
    worker: WorkerRecord | null
    // Optional kiosk_code scope. Empty = all kiosks (kiosk binary by
    // definition only has local data; on the controller this scopes the
    // fleet-wide ledger to one kiosk).
    kioskCode?: string
  }>(),
  { kioskCode: '' },
)
const emit = defineEmits<{ 'update:open': [value: boolean] }>()

const rows = ref<TxRow[]>([])
const page = ref(1)
const perPage = ref(50)
const total = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

const detailOpen = ref(false)
const selected = ref<TxSummary | null>(null)

const columns: ColumnDef[] = [
  { key: 'completed', label: 'Completed' },
  { key: 'kiosk', label: 'Kiosk' },
  { key: 'group', label: 'Group' },
  { key: 'lines', label: 'Lines', align: 'right' },
]

async function load(p = 1) {
  if (!props.worker) return
  loading.value = true
  error.value = null
  try {
    const parts = [pb.filter('user = {:u}', { u: props.worker.id }), 'status = "completed"']
    if (props.kioskCode) {
      parts.push(pb.filter('kiosk_code = {:k}', { k: props.kioskCode }))
    }
    const res = await pb.collection('transactions').getList<TxRow>(p, perPage.value, {
      filter: parts.join(' && '),
      sort: '-completed_at',
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

watch(
  () => [props.open, props.worker?.id, props.kioskCode] as const,
  ([open]) => {
    if (open) load(1)
    else rows.value = []
  },
)

function openDetail(t: TxRow) {
  if (!props.worker) return
  selected.value = {
    id: t.id,
    completedAt: t.completed_at,
    userName: props.worker.name,
    userCode: props.worker.code,
    kioskCode: t.kiosk_code,
    locationCode: t.location_code,
  }
  detailOpen.value = true
}

function formatDateTime(iso: string): string {
  return new Date(iso).toLocaleString()
}
</script>

<template>
  <AppDialog
    :open="open"
    variant="sheet"
    :title="worker ? `History — ${worker.name}` : 'History'"
    size="lg"
    @update:open="emit('update:open', $event)"
  >
    <div v-if="worker" class="flex flex-col gap-3">
      <div class="text-sm text-slate-400">
        <span class="font-mono">{{ worker.code }}</span>
        <span v-if="worker.role" class="ml-2 px-2 py-0.5 rounded bg-slate-800 text-xs">{{ worker.role }}</span>
      </div>

      <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2">
        {{ error }}
      </p>

      <DataTable
        :columns="columns"
        :rows="rows"
        :row-key="(t) => t.id"
        :loading="loading"
        empty-text="No transactions for this worker."
        row-clickable
        :page="page"
        :per-page="perPage"
        :total="total"
        @row-click="openDetail"
        @update:page="(p) => load(p)"
        @update:per-page="(n) => { perPage = n; load(1) }"
      >
        <template #cell-completed="{ row }">
          <span class="text-slate-200">{{ formatDateTime(row.completed_at) }}</span>
        </template>
        <template #cell-kiosk="{ row }">
          <span class="font-mono text-slate-400">{{ row.kiosk_code }}</span>
        </template>
        <template #cell-group="{ row }">
          <span class="text-slate-400">{{ row.user_group || '—' }}</span>
        </template>
        <template #cell-lines="{ row }">
          <span class="tabular-nums text-slate-300">{{ row.lines_count }}</span>
        </template>
      </DataTable>
    </div>

    <TransactionDetailDialog
      :open="detailOpen"
      :transaction="selected"
      @update:open="detailOpen = $event"
    />
  </AppDialog>
</template>
