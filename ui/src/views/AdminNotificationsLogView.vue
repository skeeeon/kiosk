<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import { useToast } from '../composables/useToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import NotificationsTabs from '../components/NotificationsTabs.vue'
import DataTable, { type ColumnDef } from '../components/DataTable.vue'
import { useUrlQuerySync } from '../composables/useUrlQuerySync'

interface SendLogRow {
  id: string
  event_type: string
  recipient: string
  status: 'sent' | 'failed' | 'skipped'
  error: string
  payload_summary: string
  created: string
}

interface SendLogPage {
  items: SendLogRow[]
  totalItems: number
  totalPages: number
  page: number
  perPage: number
}

const toast = useToast()
const { identity } = useKioskIdentity()
const managed = computed(() => identity.value?.managed ?? false)

const rows = ref<SendLogRow[]>([])
const total = ref(0)
const page = ref(1)
const perPage = ref(50)
const loading = ref(false)

const columns: ColumnDef[] = [
  { key: 'created', label: 'When' },
  { key: 'event_type', label: 'Event' },
  { key: 'recipient', label: 'Recipient' },
  { key: 'status', label: 'Status' },
  { key: 'detail', label: 'Detail' },
]

const eventTypeFilter = ref('')
const statusFilter = ref<'' | 'sent' | 'failed' | 'skipped'>('')

// Default lookback. 30 days matches the SPA help text; the retention cron
// keeps the underlying table at 90 days max.
const lookbackDays = 30

const eventTypes = ref<string[]>([])

useUrlQuerySync({
  page: { ref: page, default: 1, parse: (v) => Number(v) || 1 },
  event: { ref: eventTypeFilter, default: '' },
  status: { ref: statusFilter, default: '' },
})

async function loadEventTypes() {
  try {
    const list = await pb.collection('notification_templates').getFullList<{ event_type: string }>({
      sort: '+event_type',
      fields: 'event_type',
    })
    eventTypes.value = list.map((t) => t.event_type)
  } catch {
    eventTypes.value = []
  }
}

function isoDaysAgo(days: number): string {
  const d = new Date()
  d.setUTCDate(d.getUTCDate() - days)
  // PB stores datetimes as ISO strings; comparing as ISO is lexicographically
  // safe so a plain >= filter on `created` works.
  return d.toISOString().replace('T', ' ').replace('Z', '')
}

async function load() {
  loading.value = true
  try {
    const clauses: string[] = [`created >= "${isoDaysAgo(lookbackDays)}"`]
    if (eventTypeFilter.value) clauses.push(`event_type = "${eventTypeFilter.value}"`)
    if (statusFilter.value) clauses.push(`status = "${statusFilter.value}"`)
    const filter = clauses.join(' && ')
    const res = await pb.collection('notification_send_log').getList<SendLogRow>(page.value, perPage.value, {
      filter,
      sort: '-created',
    })
    const p = res as unknown as SendLogPage
    rows.value = p.items
    total.value = p.totalItems
  } catch (e) {
    toast.error(`Load failed: ${(e as Error).message}`)
  } finally {
    loading.value = false
  }
}

watch([eventTypeFilter, statusFilter], () => {
  page.value = 1
  void load()
})

onMounted(async () => {
  await loadEventTypes()
  await load()
})

function statusClass(status: SendLogRow['status']): string {
  switch (status) {
    case 'sent':
      return 'text-emerald-300 bg-emerald-900/40'
    case 'failed':
      return 'text-red-300 bg-red-900/40'
    case 'skipped':
      return 'text-slate-300 bg-slate-800/70'
  }
}

function onPageChange(p: number) {
  page.value = p
  void load()
}

function onPerPageChange(n: number) {
  perPage.value = n
  page.value = 1
  void load()
}
</script>

<template>
  <main class="p-6 max-w-7xl mx-auto w-full">
    <header class="mb-4">
      <h1 class="text-2xl font-semibold">Notifications</h1>
    </header>

    <NotificationsTabs />

    <div
      v-if="managed"
      class="rounded-lg bg-sky-950/60 border border-sky-800 text-sky-200 px-4 py-3 mb-4 text-sm"
    >
      Send activity for managed kiosks is logged on the controller, not
      here — this table will stay empty. Open the controller&rsquo;s
      Notifications &rarr; Recent sends tab for the full audit trail.
    </div>

    <p class="text-sm text-slate-400 mb-4">
      One row per attempted recipient over the last {{ lookbackDays }} days.
      Older entries are pruned automatically.
    </p>

    <div class="flex gap-3 mb-3">
      <select
        v-model="eventTypeFilter"
        class="rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100 text-sm"
      >
        <option value="">All events</option>
        <option v-for="et in eventTypes" :key="et" :value="et">{{ et }}</option>
      </select>
      <select
        v-model="statusFilter"
        class="rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100 text-sm"
      >
        <option value="">All statuses</option>
        <option value="sent">Sent</option>
        <option value="failed">Failed</option>
        <option value="skipped">Skipped</option>
      </select>
    </div>

    <DataTable
      :columns="columns"
      :rows="rows"
      :row-key="(r) => r.id"
      :loading="loading"
      empty-text="No send log entries in the selected window."
      :page="page"
      :per-page="perPage"
      :total="total"
      @update:page="onPageChange"
      @update:per-page="onPerPageChange"
    >
      <template #cell-created="{ row }">
        <span class="text-slate-300 whitespace-nowrap font-mono text-xs">{{ row.created }}</span>
      </template>
      <template #cell-event_type="{ row }">
        <span class="text-slate-300 font-mono text-xs">{{ row.event_type }}</span>
      </template>
      <template #cell-recipient="{ row }">
        <span class="text-slate-200 break-all">{{ row.recipient || '—' }}</span>
      </template>
      <template #cell-status="{ row }">
        <span :class="['inline-block px-2 py-0.5 rounded text-xs', statusClass(row.status)]">
          {{ row.status }}
        </span>
      </template>
      <template #cell-detail="{ row }">
        <div class="text-slate-400 text-xs">
          <p v-if="row.error" class="text-red-300 break-words">{{ row.error }}</p>
          <p v-else class="break-words">{{ row.payload_summary || '—' }}</p>
        </div>
      </template>
    </DataTable>
  </main>
</template>
