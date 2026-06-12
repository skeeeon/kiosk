<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { pb } from '../lib/pb'
import ScheduledReportDialog from '../components/ScheduledReportDialog.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import NotificationsTabs from '../components/NotificationsTabs.vue'
import DataTable, { type ColumnDef } from '../components/DataTable.vue'
import { useToast } from '../composables/useToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import { useUrlQuerySync } from '../composables/useUrlQuerySync'
import type { ScheduledReportRecord } from '../types'

const toast = useToast()
const { identity } = useKioskIdentity()
const isController = computed(() => identity.value?.role === 'controller')
const managed = computed(() => identity.value?.managed ?? false)

const rows = ref<ScheduledReportRecord[]>([])
const loading = ref(false)
const editing = ref<Partial<ScheduledReportRecord> | null>(null)
const deleting = ref<ScheduledReportRecord | null>(null)
const page = ref(1)
const perPage = ref(25)

useUrlQuerySync({
  page: { ref: page, default: 1, parse: (v) => Number(v) || 1 },
})

const pagedRows = computed(() => {
  const start = (page.value - 1) * perPage.value
  return rows.value.slice(start, start + perPage.value)
})

async function load() {
  // Managed kiosks: the controller owns the schedules now, so this view
  // stays empty on the kiosk side and a banner directs operators to the
  // controller. The controller and standalone-kiosk paths both load.
  if (!isController.value && managed.value) {
    rows.value = []
    return
  }
  loading.value = true
  try {
    rows.value = await pb.collection('scheduled_reports').getFullList<ScheduledReportRecord>({
      sort: '+report_key',
    })
  } catch (e) {
    toast.error(`Load failed: ${(e as Error).message}`)
  } finally {
    loading.value = false
  }
}

onMounted(load)

const weekdayLabels = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']

function hourLabel(h: number): string {
  if (h === 0) return '12 AM'
  if (h < 12) return `${h} AM`
  if (h === 12) return '12 PM'
  return `${h - 12} PM`
}

function ordinal(n: number): string {
  const s = ['th', 'st', 'nd', 'rd']
  const v = n % 100
  return n + (s[(v - 20) % 10] || s[v] || s[0])
}

function cadenceLabel(r: ScheduledReportRecord): string {
  if (r.cadence === 'daily') return `Daily at ${hourLabel(r.hour)}`
  if (r.cadence === 'weekly') return `Weekly on ${weekdayLabels[r.weekday] ?? '?'} at ${hourLabel(r.hour)}`
  return `Monthly on the ${ordinal(r.day_of_month)} at ${hourLabel(r.hour)}`
}

function reportLabel(key: string): string {
  switch (key) {
    case 'open_checkouts':
      return 'Currently checked out'
    case 'daily_activity':
      return 'Daily activity'
    case 'maintenance':
      return 'Items in maintenance'
    case 'timeclock':
      return 'Timeclock'
  }
  return key
}

function recipientsLabel(r: ScheduledReportRecord): string {
  const parts: string[] = []
  if (r.recipients.all_admins) parts.push('all admins')
  if (r.recipients.extras && r.recipients.extras.length > 0) {
    parts.push(`${r.recipients.extras.length} extra${r.recipients.extras.length === 1 ? '' : 's'}`)
  }
  return parts.length === 0 ? '—' : parts.join(' + ')
}

function statusClass(status?: string): string {
  switch (status) {
    case 'sent':
      return 'text-emerald-300 bg-emerald-900/40'
    case 'failed':
      return 'text-red-300 bg-red-900/40'
    case 'skipped':
      return 'text-slate-300 bg-slate-800/70'
    default:
      return 'text-slate-500 bg-slate-900/40'
  }
}

function openNew() {
  editing.value = {}
}
function openEdit(r: ScheduledReportRecord) {
  editing.value = { ...r }
}

const columns: ColumnDef[] = [
  { key: 'report_key', label: 'Report' },
  { key: 'schedule', label: 'Schedule' },
  { key: 'recipients', label: 'Recipients' },
  { key: 'last_run', label: 'Last run' },
  { key: 'enabled', label: 'Enabled' },
  { key: '__actions', align: 'right' },
]

const emptyText = computed(() =>
  !isController.value && managed.value
    ? ''
    : 'No scheduled reports yet. Click "New schedule" to create one.',
)

async function persistSave(data: Partial<ScheduledReportRecord>): Promise<boolean> {
  if (data.id) {
    await pb.collection('scheduled_reports').update<ScheduledReportRecord>(data.id, data)
    return true
  }
  await pb.collection('scheduled_reports').create<ScheduledReportRecord>(data as Record<string, unknown>)
  return false
}

async function onSave(data: Partial<ScheduledReportRecord>) {
  try {
    const wasEdit = await persistSave(data)
    editing.value = null
    await load()
    toast.success(wasEdit ? 'Schedule updated' : 'Schedule created')
  } catch (e) {
    toast.error((e as Error).message)
  }
}

async function onSaveAndAdd(data: Partial<ScheduledReportRecord>) {
  try {
    await persistSave(data)
    editing.value = {}
    await load()
    toast.success('Schedule created — ready for next')
  } catch (e) {
    toast.error((e as Error).message)
  }
}

async function onDelete() {
  if (!deleting.value) return
  try {
    await pb.collection('scheduled_reports').delete(deleting.value.id)
    deleting.value = null
    await load()
    toast.success('Schedule deleted')
  } catch (e) {
    toast.error((e as Error).message)
  }
}
</script>

<template>
  <main class="p-6 max-w-7xl mx-auto w-full">
    <header class="mb-4">
      <h1 class="text-2xl font-semibold">Notifications</h1>
    </header>

    <NotificationsTabs />

    <div class="mb-4 flex items-start justify-between gap-4">
      <p class="text-sm text-slate-400">
        Email a report on a recurring schedule. The scheduler re-reads
        this list whenever you save, so changes apply without a restart.
      </p>
      <button
        v-if="isController || !managed"
        type="button"
        class="shrink-0 px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        @click="openNew"
      >
        New schedule
      </button>
    </div>

    <div
      v-if="!isController && managed"
      class="rounded-lg bg-slate-900 border border-slate-800 text-slate-300 px-4 py-3 mb-4 text-sm"
    >
      Scheduled reports are managed on the controller in this deployment. Configure them from the controller&rsquo;s admin SPA.
    </div>

    <DataTable
      :columns="columns"
      :rows="pagedRows"
      :row-key="(r) => r.id"
      :loading="loading"
      :empty-text="emptyText"
      row-clickable
      :page="page"
      :per-page="perPage"
      :total="rows.length"
      @row-click="openEdit"
      @update:page="(p) => page = p"
      @update:per-page="(n) => { perPage = n; page = 1 }"
    >
      <template #cell-report_key="{ row }">
        <span class="text-slate-200">{{ reportLabel(row.report_key) }}</span>
      </template>
      <template #cell-schedule="{ row }">
        <span class="text-slate-300">{{ cadenceLabel(row) }}</span>
      </template>
      <template #cell-recipients="{ row }">
        <span class="text-slate-400">{{ recipientsLabel(row) }}</span>
      </template>
      <template #cell-last_run="{ row }">
        <div v-if="row.last_run_at" class="flex flex-col gap-1 text-xs text-slate-400">
          <span class="font-mono">{{ row.last_run_at }}</span>
          <span :class="['inline-block px-2 py-0.5 rounded text-[10px] w-fit', statusClass(row.last_status)]">
            {{ row.last_status || 'unknown' }}
          </span>
          <span v-if="row.last_error" class="text-red-300 break-words">{{ row.last_error }}</span>
        </div>
        <span v-else class="text-slate-500 text-xs">Never</span>
      </template>
      <template #cell-enabled="{ row }">
        <span v-if="row.enabled" class="text-emerald-400">●</span>
        <span v-else class="text-slate-600">●</span>
      </template>
      <template #cell-__actions="{ row }">
        <button
          type="button"
          class="px-3 py-1.5 rounded-md bg-red-950/60 hover:bg-red-900/60 text-red-200 text-sm border border-red-800/70 whitespace-nowrap"
          @click.stop="deleting = row"
        >
          Delete
        </button>
      </template>
    </DataTable>

    <ScheduledReportDialog
      :open="editing !== null"
      :report="editing"
      @update:open="(v) => { if (!v) editing = null }"
      @save="onSave"
      @save-and-add-another="onSaveAndAdd"
    />

    <ConfirmDialog
      :open="deleting !== null"
      title="Delete schedule"
      :message="deleting ? `Delete the ${deleting.cadence} ${reportLabel(deleting.report_key)} schedule? Send history stays intact.` : ''"
      confirm-label="Delete"
      destructive
      @update:open="(v) => { if (!v) deleting = null }"
      @confirm="onDelete"
    />
  </main>
</template>
