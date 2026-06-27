<!-- KioskInstancesPanel manages a managed kiosk's item_instances from the
     controller. Mirrors KioskInventoryPanel's shape: snapshot on mount via
     GET /api/controller/kiosks/:code/instances, then mutations via the
     matching command-bus endpoints (POST create, PATCH edit, POST
     status with a target status). 503 + {error: "kiosk_offline"} renders a
     banner; everything else falls through to the usual error box. -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import DataTable, { type ColumnDef } from './DataTable.vue'
import { api, isKioskOfflineError as isOfflineError } from '../lib/api'
import { useToast } from '../composables/useToast'
import {
  instanceActions,
  statusLabel,
  statusBadgeClass,
  actionButtonClass,
  type InstanceStatus,
} from '../lib/instanceStatus'

interface InstanceRow {
  instance_id: string
  instance_code: string
  item_id: string
  item_code: string
  item_name: string
  serial: string
  rfid_epc: string
  // Lifecycle status (in_service / maintenance / retired). Carried straight
  // from the kiosk's instance snapshot.
  status: InstanceStatus
  notes: string
  // Access-controlled cabinet the unit lives in — the enclosure_diff partition
  // key. Empty = counter/crib or single-cabinet kiosk.
  enclosure_id: string
  created: string
  updated: string
  // Derived by the controller from its projected ledger: is this unit
  // currently checked out? Orthogonal to status.
  out: boolean
  // Advisory "last seen" location, carried through the kiosk's instance
  // snapshot (kept current by the node's L3 mirror watcher). Empty until
  // observed.
  last_observed_at?: string
  last_observed_zone?: string
  last_observed_gateway?: string
}

const props = defineProps<{ kioskCode: string }>()
const toast = useToast()

const rows = ref<InstanceRow[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const offline = ref(false)
const itemFilter = ref('')
const page = ref(1)
const perPage = ref(25)

// Three mutation surfaces share the panel: create (new row), edit (cosmetic),
// and status transitions (send to maintenance / return to service / retire /
// un-retire) — one reason-gated dialog drives all transitions, with the
// target status as data.
type DialogMode = 'create' | 'edit' | null
const dialogMode = ref<DialogMode>(null)
const form = ref({
  instance_id: '',
  item_code: '',
  code: '',
  serial: '',
  rfid_epc: '',
  notes: '',
  enclosure_id: '',
})
const submitting = ref(false)

// Status transitions share a single dialog with a required reason field. The
// target status is data, so one dialog covers every verb; the server derives
// the audit action from the (prev → target) transition.
const statusPending = ref<{ row: InstanceRow; target: InstanceStatus; label: string } | null>(null)
const statusReason = ref('')
const statusSubmitting = ref(false)

const filtered = computed(() => {
  const f = itemFilter.value.trim().toLowerCase()
  if (!f) return rows.value
  return rows.value.filter(
    (r) =>
      r.item_name.toLowerCase().includes(f) ||
      r.item_code.toLowerCase().includes(f),
  )
})

// Reset to page 1 when the filter narrows/changes — otherwise the user might
// land on an empty page that no longer exists at the new row count.
watch(itemFilter, () => { page.value = 1 })

const pagedRows = computed(() => {
  const start = (page.value - 1) * perPage.value
  return filtered.value.slice(start, start + perPage.value)
})

const itemCodeOptions = computed(() => {
  const set = new Set<string>()
  rows.value.forEach((r) => {
    if (r.item_code) set.add(r.item_code)
  })
  return Array.from(set).sort()
})

async function loadSnapshot() {
  if (!props.kioskCode) return
  loading.value = true
  error.value = null
  offline.value = false
  try {
    const res = await api.get<{ instances: InstanceRow[] }>(
      `/api/controller/kiosks/${encodeURIComponent(props.kioskCode)}/instances`,
    )
    rows.value = res.instances ?? []
  } catch (e) {
    if (isOfflineError(e)) {
      offline.value = true
      rows.value = []
    } else {
      error.value = (e as Error).message
    }
  } finally {
    loading.value = false
  }
}

watch(() => props.kioskCode, (c) => { if (c) void loadSnapshot() }, { immediate: true })

function openCreate() {
  form.value = {
    instance_id: '',
    item_code: itemCodeOptions.value[0] ?? '',
    code: '',
    serial: '',
    rfid_epc: '',
    notes: '',
    enclosure_id: '',
  }
  dialogMode.value = 'create'
}

function openEdit(r: InstanceRow) {
  form.value = {
    instance_id: r.instance_id,
    item_code: r.item_code,
    code: r.instance_code,
    serial: r.serial,
    rfid_epc: r.rfid_epc,
    notes: r.notes,
    enclosure_id: r.enclosure_id,
  }
  dialogMode.value = 'edit'
}

async function submitForm() {
  if (!form.value.code.trim()) {
    toast.error('Code is required')
    return
  }
  submitting.value = true
  try {
    if (dialogMode.value === 'create') {
      if (!form.value.item_code.trim()) {
        toast.error('Item code is required')
        submitting.value = false
        return
      }
      const created = await api.post<InstanceRow>(
        `/api/controller/kiosks/${encodeURIComponent(props.kioskCode)}/instances`,
        {
          item_code: form.value.item_code.trim(),
          code: form.value.code.trim(),
          serial: form.value.serial.trim() || undefined,
          rfid_epc: form.value.rfid_epc.trim() || undefined,
          notes: form.value.notes.trim() || undefined,
          enclosure_id: form.value.enclosure_id.trim() || undefined,
        },
      )
      // Append + sort; snapshot refresh would also work but skips a round-trip.
      rows.value = [...rows.value, hydrateRow(created)].sort((a, b) =>
        a.instance_code.localeCompare(b.instance_code))
      toast.success(`Created ${created.instance_code}`)
    } else {
      // Edit: only the fields that actually changed; the server's PATCH
      // ignores fields that weren't touched, so we can send everything
      // editable. Code/notes/serial/rfid only — status changes go through
      // the status-transition dialog.
      const body: Record<string, string> = {
        code: form.value.code.trim(),
        serial: form.value.serial.trim(),
        rfid_epc: form.value.rfid_epc.trim(),
        notes: form.value.notes.trim(),
        enclosure_id: form.value.enclosure_id.trim(),
      }
      const updated = await api.patch<InstanceRow>(
        `/api/controller/kiosks/${encodeURIComponent(props.kioskCode)}/instances/${encodeURIComponent(rowInstanceCode())}`,
        body,
      )
      const idx = rows.value.findIndex((x) => x.instance_id === updated.instance_id)
      if (idx >= 0) rows.value[idx] = { ...rows.value[idx], ...hydrateRow(updated) }
      toast.success(`Updated ${updated.instance_code}`)
    }
    dialogMode.value = null
  } catch (e) {
    if (isOfflineError(e)) {
      offline.value = true
      dialogMode.value = null
      toast.error('Kiosk is offline — change not applied')
    } else {
      toast.error((e as Error).message)
    }
  } finally {
    submitting.value = false
  }
}

// rowInstanceCode reads the editing row's original code (path identifier) —
// distinct from form.code which may have been changed to a new value as
// part of this same edit.
function rowInstanceCode(): string {
  const r = rows.value.find((x) => x.instance_id === form.value.instance_id)
  return r?.instance_code ?? form.value.code
}

// hydrateRow fills in any fields the kiosk's reply omitted. The reply uses
// InstanceResult which is leaner than the snapshot shape; defaults keep
// table rendering consistent.
function hydrateRow(r: Partial<InstanceRow>): InstanceRow {
  return {
    instance_id: r.instance_id ?? '',
    instance_code: r.instance_code ?? '',
    item_id: r.item_id ?? '',
    item_code: r.item_code ?? '',
    item_name: r.item_name ?? '',
    serial: r.serial ?? '',
    rfid_epc: r.rfid_epc ?? '',
    status: r.status ?? 'in_service',
    notes: r.notes ?? '',
    enclosure_id: r.enclosure_id ?? '',
    created: r.created ?? '',
    updated: r.updated ?? '',
    // A just-created/edited unit's reply doesn't carry out-status; a fresh
    // unit isn't out, and a refresh re-derives it from the ledger anyway.
    out: r.out ?? false,
  }
}

function openStatus(r: InstanceRow, target: InstanceStatus, label: string) {
  statusPending.value = { row: r, target, label }
  statusReason.value = ''
}

async function confirmStatus() {
  if (!statusPending.value) return
  if (!statusReason.value.trim()) {
    toast.error('Reason is required')
    return
  }
  const { row, target } = statusPending.value
  statusSubmitting.value = true
  try {
    // Single status endpoint: the target is data, the server picks the verb
    // and stamps the audit + lifecycle event.
    const updated = await api.post<InstanceRow>(
      `/api/controller/kiosks/${encodeURIComponent(props.kioskCode)}/instances/${encodeURIComponent(row.instance_code)}/status`,
      { status: target, reason: statusReason.value.trim() },
    )
    const idx = rows.value.findIndex((x) => x.instance_id === updated.instance_id)
    if (idx >= 0) rows.value[idx] = { ...rows.value[idx], status: updated.status }
    statusPending.value = null
    toast.success(`${updated.instance_code} → ${statusLabel(updated.status)}`)
  } catch (e) {
    if (isOfflineError(e)) {
      offline.value = true
      statusPending.value = null
      toast.error('Kiosk is offline — change not applied')
    } else {
      toast.error((e as Error).message)
    }
  } finally {
    statusSubmitting.value = false
  }
}

// "Last seen" is advisory location: shown only when some unit at this kiosk has
// been observed, so a kiosk with no gateways / no reader zone never sees it.
const hasLastSeen = computed(() => rows.value.some((r) => !!r.last_observed_at))

const columns = computed<ColumnDef[]>(() => {
  const cols: ColumnDef[] = [
    { key: 'item_name', label: 'Item' },
    { key: 'instance_code', label: 'Code' },
    { key: 'serial', label: 'Serial' },
    { key: 'rfid_epc', label: 'RFID' },
    { key: 'enclosure_id', label: 'Enclosure' },
    { key: 'status', label: 'Status' },
    { key: 'out', label: 'Out?' },
  ]
  if (hasLastSeen.value) cols.push({ key: 'last_seen', label: 'Last seen' })
  cols.push({ key: '__actions', align: 'right' })
  return cols
})

function relativeAge(iso: string): string {
  const t = new Date(iso).getTime()
  if (!Number.isFinite(t)) return ''
  const diffMs = Date.now() - t
  if (diffMs < 60_000) return 'just now'
  const minutes = Math.floor(diffMs / 60_000)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}
</script>

<template>
  <section class="space-y-3">
    <header class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3">
      <div>
        <h3 class="text-sm font-medium text-slate-200">Item instances</h3>
        <p class="text-xs text-slate-500">
          One row per serialized unit on this kiosk. Cosmetic edits don&rsquo;t
          audit; status transitions write a lifecycle event.
        </p>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        <input
          v-model="itemFilter"
          type="text"
          placeholder="Filter by item…"
          class="flex-1 sm:flex-none rounded-lg bg-slate-800 border border-slate-700 px-3 py-1.5 text-slate-100 text-sm sm:w-44"
        />
        <button
          type="button"
          class="text-sm px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 disabled:opacity-50"
          :disabled="loading"
          @click="loadSnapshot"
        >
          {{ loading ? 'Loading…' : 'Refresh' }}
        </button>
        <button
          type="button"
          class="text-sm px-3 py-1.5 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium disabled:opacity-50"
          :disabled="offline"
          @click="openCreate"
        >
          New instance
        </button>
      </div>
    </header>

    <div
      v-if="offline"
      class="rounded-lg bg-amber-900/40 border border-amber-800 text-amber-100 text-sm px-3 py-2"
    >
      This kiosk hasn&rsquo;t sent a heartbeat recently. Instance snapshot
      and remote mutations are unavailable until it reconnects.
    </div>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 text-sm px-3 py-2">
      {{ error }}
    </p>

    <DataTable
      :columns="columns"
      :rows="pagedRows"
      :row-key="(r) => r.instance_id"
      :loading="loading"
      empty-text="No instances at this kiosk yet."
      :row-class="(r) => (r.status === 'retired' ? 'text-slate-500' : undefined)"
      :page="page"
      :per-page="perPage"
      :total="filtered.length"
      @update:page="(p) => page = p"
      @update:per-page="(n) => { perPage = n; page = 1 }"
    >
      <template #cell-item_name="{ row }">
        <span class="text-slate-300 block sm:truncate sm:max-w-[10rem]" :title="row.item_name">{{ row.item_name }}</span>
      </template>
      <template #cell-instance_code="{ row }">
        <span class="font-mono">{{ row.instance_code }}</span>
      </template>
      <template #cell-serial="{ row }">
        <span class="font-mono block break-all sm:truncate sm:max-w-[7rem]" :title="row.serial">{{ row.serial || '—' }}</span>
      </template>
      <template #cell-rfid_epc="{ row }">
        <span class="font-mono block break-all sm:truncate sm:max-w-[8rem]" :title="row.rfid_epc">{{ row.rfid_epc || '—' }}</span>
      </template>
      <template #cell-enclosure_id="{ row }">
        <span class="font-mono text-slate-400" :title="row.enclosure_id">{{ row.enclosure_id || '—' }}</span>
      </template>
      <template #cell-status="{ row }">
        <span
          class="inline-block px-2 py-0.5 rounded text-[10px]"
          :class="statusBadgeClass(row.status)"
        >{{ statusLabel(row.status) }}</span>
      </template>
      <template #cell-out="{ row }">
        <span v-if="row.out" class="text-amber-300 text-xs">currently out</span>
        <span v-else class="text-slate-500 text-xs">in</span>
      </template>
      <template #cell-last_seen="{ row }">
        <template v-if="row.last_observed_at">
          <span class="text-slate-300">{{ row.last_observed_zone || '—' }}</span>
          <span class="text-slate-500 text-xs" :title="row.last_observed_at"> · {{ relativeAge(row.last_observed_at) }}</span>
        </template>
        <span v-else class="text-slate-600">—</span>
      </template>
      <template #cell-__actions="{ row }">
        <div class="inline-flex flex-wrap justify-end gap-2">
          <button
            type="button"
            class="px-3 py-1.5 rounded-md bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm border border-slate-700 whitespace-nowrap disabled:opacity-50"
            :disabled="offline"
            @click="openEdit(row)"
          >
            Edit
          </button>
          <button
            v-for="a in instanceActions(row.status)"
            :key="a.target"
            type="button"
            class="px-3 py-1.5 rounded-md text-sm border whitespace-nowrap disabled:opacity-50"
            :class="actionButtonClass(a.tone)"
            :disabled="offline"
            @click="openStatus(row, a.target, a.label)"
          >
            {{ a.label }}
          </button>
        </div>
      </template>
    </DataTable>

    <AppDialog
      :open="dialogMode !== null"
      :title="dialogMode === 'create' ? 'New instance' : 'Edit instance'"
      size="sm"
      @update:open="(v) => { if (!v) dialogMode = null }"
    >
      <form class="flex flex-col gap-4" @submit.prevent="submitForm">
        <label v-if="dialogMode === 'create'" class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Item code</span>
          <input
            v-model="form.item_code"
            type="text"
            required
            placeholder="e.g. DRILL"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 font-mono"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Instance code</span>
          <input
            v-model="form.code"
            type="text"
            required
            placeholder="e.g. DRILL-042"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 font-mono"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Serial</span>
          <input
            v-model="form.serial"
            type="text"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 font-mono"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">RFID EPC</span>
          <input
            v-model="form.rfid_epc"
            type="text"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 font-mono"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Enclosure</span>
          <input
            v-model="form.enclosure_id"
            type="text"
            placeholder="cabinet id — leave blank for counter/crib"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 font-mono"
          />
          <span class="text-xs text-slate-500">
            The access-controlled cabinet this unit lives in. Only matters when
            the kiosk hosts more than one cabinet.
          </span>
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Notes</span>
          <textarea
            v-model="form.notes"
            rows="2"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 resize-none"
          ></textarea>
        </label>

        <div class="flex justify-end gap-3 mt-1">
          <button
            type="button"
            class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
            @click="dialogMode = null"
          >
            Cancel
          </button>
          <button
            type="submit"
            class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium disabled:opacity-50"
            :disabled="submitting"
          >
            {{ submitting ? 'Submitting…' : (dialogMode === 'create' ? 'Create' : 'Save changes') }}
          </button>
        </div>
      </form>
    </AppDialog>

    <AppDialog
      :open="statusPending !== null"
      :title="statusPending?.label ?? 'Change status'"
      size="sm"
      @update:open="(v) => { if (!v) statusPending = null }"
    >
      <form class="flex flex-col gap-4" @submit.prevent="confirmStatus">
        <p class="text-slate-300 text-sm">
          {{ statusPending?.label }}
          <span class="font-mono">{{ statusPending?.row.instance_code }}</span>?
          A reason is required for the audit log.
        </p>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Reason</span>
          <textarea
            v-model="statusReason"
            rows="2"
            required
            placeholder="e.g. broken handle, returned from service"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 resize-none"
          ></textarea>
        </label>
        <div class="flex justify-end gap-3 mt-1">
          <button
            type="button"
            class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
            @click="statusPending = null"
          >
            Cancel
          </button>
          <button
            type="submit"
            class="px-4 py-2 rounded-lg font-medium text-white bg-brand-primary hover:bg-brand-primary-hover disabled:opacity-50"
            :disabled="statusSubmitting"
          >
            {{ statusPending?.label ?? 'Apply' }}
          </button>
        </div>
      </form>
    </AppDialog>
  </section>
</template>
