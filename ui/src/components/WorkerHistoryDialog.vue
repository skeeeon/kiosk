<!-- WorkerHistoryDialog shows one worker's completed transactions, paginated.
     Click a row to drill into the per-line detail via the existing
     TransactionDetailDialog. The transaction history is read directly from
     PB; on the controller this surfaces every transaction the aggregator
     has projected, on a standalone kiosk it's the local ledger. -->
<script setup lang="ts">
import { ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
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
const totalPages = ref(1)
const loading = ref(false)
const error = ref<string | null>(null)

const detailOpen = ref(false)
const selected = ref<TxSummary | null>(null)

async function load(p = 1) {
  if (!props.worker) return
  loading.value = true
  error.value = null
  try {
    const parts = [`user = "${props.worker.id}"`, 'status = "completed"']
    if (props.kioskCode) {
      parts.push(`kiosk_code = "${props.kioskCode.replace(/"/g, '\\"')}"`)
    }
    const res = await pb.collection('transactions').getList<TxRow>(p, 50, {
      filter: parts.join(' && '),
      sort: '-completed_at',
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

      <div class="rounded-2xl bg-slate-950 border border-slate-800 overflow-hidden">
        <table class="w-full text-left text-sm">
          <thead class="bg-slate-900/70 text-slate-400">
            <tr>
              <th class="px-4 py-2 font-medium">Completed</th>
              <th class="px-4 py-2 font-medium">Kiosk</th>
              <th class="px-4 py-2 font-medium">Group</th>
              <th class="px-4 py-2 font-medium text-right">Lines</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-800">
            <tr v-if="loading">
              <td colspan="4" class="text-center text-slate-500 py-6">Loading…</td>
            </tr>
            <tr v-else-if="rows.length === 0">
              <td colspan="4" class="text-center text-slate-500 py-6">No transactions for this worker.</td>
            </tr>
            <tr v-for="t in rows" :key="t.id" class="hover:bg-slate-800/40 cursor-pointer" @click="openDetail(t)">
              <td class="px-4 py-2 text-slate-200">{{ formatDateTime(t.completed_at) }}</td>
              <td class="px-4 py-2 font-mono text-slate-400">{{ t.kiosk_code }}</td>
              <td class="px-4 py-2 text-slate-400">{{ t.user_group || '—' }}</td>
              <td class="px-4 py-2 text-right tabular-nums text-slate-300">{{ t.lines_count }}</td>
            </tr>
          </tbody>
        </table>
        <div
          v-if="totalPages > 1"
          class="flex items-center justify-between px-4 py-2 border-t border-slate-800 text-sm"
        >
          <button
            type="button"
            class="px-2 py-1 rounded bg-slate-800 hover:bg-slate-700 disabled:opacity-40"
            :disabled="page <= 1 || loading"
            @click="load(page - 1)"
          >
            Previous
          </button>
          <span class="text-slate-400">Page {{ page }} of {{ totalPages }}</span>
          <button
            type="button"
            class="px-2 py-1 rounded bg-slate-800 hover:bg-slate-700 disabled:opacity-40"
            :disabled="page >= totalPages || loading"
            @click="load(page + 1)"
          >
            Next
          </button>
        </div>
      </div>
    </div>

    <TransactionDetailDialog
      :open="detailOpen"
      :transaction="selected"
      @update:open="detailOpen = $event"
    />
  </AppDialog>
</template>
