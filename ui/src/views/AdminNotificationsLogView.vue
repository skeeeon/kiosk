<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import { useAdminToast } from '../composables/useAdminToast'

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

const toast = useAdminToast()

const rows = ref<SendLogRow[]>([])
const total = ref(0)
const page = ref(1)
const perPage = 50
const loading = ref(false)

const eventTypeFilter = ref('')
const statusFilter = ref<'' | 'sent' | 'failed' | 'skipped'>('')

// Default lookback. 30 days matches the SPA help text; the retention cron
// keeps the underlying table at 90 days max.
const lookbackDays = 30

const eventTypes = ref<string[]>([])

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
    const res = await pb.collection('notification_send_log').getList<SendLogRow>(page.value, perPage, {
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

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / perPage)))
const showingFrom = computed(() => (total.value === 0 ? 0 : (page.value - 1) * perPage + 1))
const showingTo = computed(() => Math.min(page.value * perPage, total.value))

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

function nextPage() {
  if (page.value < totalPages.value) {
    page.value++
    void load()
  }
}
function prevPage() {
  if (page.value > 1) {
    page.value--
    void load()
  }
}
</script>

<template>
  <main class="p-6 max-w-6xl mx-auto w-full">
    <header class="mb-4 flex items-start justify-between gap-4">
      <div>
        <h1 class="text-2xl font-semibold">Recent sends</h1>
        <p class="text-sm text-slate-400 mt-1">
          One row per attempted recipient over the last {{ lookbackDays }} days. Older entries are pruned automatically.
        </p>
      </div>
      <RouterLink
        :to="{ name: 'admin-notifications' }"
        class="shrink-0 px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
      >
        ← Back to notifications
      </RouterLink>
    </header>

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

    <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
      <table class="w-full text-left text-sm">
        <thead class="bg-slate-950/70 text-slate-400">
          <tr>
            <th class="px-4 py-3 font-medium">When</th>
            <th class="px-4 py-3 font-medium">Event</th>
            <th class="px-4 py-3 font-medium">Recipient</th>
            <th class="px-4 py-3 font-medium">Status</th>
            <th class="px-4 py-3 font-medium">Detail</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-slate-800">
          <tr v-if="loading">
            <td colspan="5" class="text-center text-slate-500 py-8">Loading…</td>
          </tr>
          <tr v-else-if="rows.length === 0">
            <td colspan="5" class="text-center text-slate-500 py-8">No send log entries in the selected window.</td>
          </tr>
          <tr v-for="r in rows" :key="r.id" class="align-top">
            <td class="px-4 py-3 text-slate-300 whitespace-nowrap font-mono text-xs">{{ r.created }}</td>
            <td class="px-4 py-3 text-slate-300 font-mono text-xs">{{ r.event_type }}</td>
            <td class="px-4 py-3 text-slate-200 break-all">{{ r.recipient || '—' }}</td>
            <td class="px-4 py-3">
              <span :class="['inline-block px-2 py-0.5 rounded text-xs', statusClass(r.status)]">
                {{ r.status }}
              </span>
            </td>
            <td class="px-4 py-3 text-slate-400 text-xs">
              <p v-if="r.error" class="text-red-300 break-words">{{ r.error }}</p>
              <p v-else class="break-words">{{ r.payload_summary || '—' }}</p>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <footer class="flex items-center justify-between mt-3 text-sm text-slate-400">
      <span>{{ showingFrom }}–{{ showingTo }} of {{ total }}</span>
      <div class="flex items-center gap-2">
        <button
          type="button"
          class="px-3 py-1 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 disabled:opacity-40"
          :disabled="page <= 1 || loading"
          @click="prevPage"
        >
          Prev
        </button>
        <span>Page {{ page }} of {{ totalPages }}</span>
        <button
          type="button"
          class="px-3 py-1 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 disabled:opacity-40"
          :disabled="page >= totalPages || loading"
          @click="nextPage"
        >
          Next
        </button>
      </div>
    </footer>
  </main>
</template>
