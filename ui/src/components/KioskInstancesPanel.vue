<!-- KioskInstancesPanel manages a managed kiosk's item_instances from the
     controller. Mirrors KioskInventoryPanel's shape: snapshot on mount via
     GET /api/controller/kiosks/:code/instances, then mutations via the
     matching command-bus endpoints (POST create, PATCH edit, POST
     {decommission|reactivate}). 503 + {error: "kiosk_offline"} renders a
     banner; everything else falls through to the usual error box. -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import DataTable, { type ColumnDef } from './DataTable.vue'
import { api, ApiError } from '../lib/api'
import { useAdminToast } from '../composables/useAdminToast'
import type { KioskOfflineError } from '../types'

interface InstanceRow {
  instance_id: string
  instance_code: string
  item_id: string
  item_code: string
  item_name: string
  serial: string
  rfid_epc: string
  active: boolean
  notes: string
  created: string
  updated: string
}

const props = defineProps<{ kioskCode: string }>()
const toast = useAdminToast()

const rows = ref<InstanceRow[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const offline = ref(false)
const itemFilter = ref('')
const page = ref(1)
const perPage = ref(25)

// Three mutation surfaces share the panel: create (new row), edit (cosmetic),
// decommission / reactivate (active toggle with a reason).
type DialogMode = 'create' | 'edit' | null
const dialogMode = ref<DialogMode>(null)
const form = ref({
  instance_id: '',
  item_code: '',
  code: '',
  serial: '',
  rfid_epc: '',
  notes: '',
})
const submitting = ref(false)

// Decommission/reactivate share a single ConfirmDialog with a reason field.
const togglePending = ref<{ row: InstanceRow; targetActive: boolean } | null>(null)
const toggleReason = ref('')
const toggleSubmitting = ref(false)

const filtered = computed(() => {
  const f = itemFilter.value.trim().toUpperCase()
  if (!f) return rows.value
  return rows.value.filter((r) => r.item_code.toUpperCase().includes(f))
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

function isOfflineError(e: unknown): boolean {
  if (e instanceof ApiError && e.status === 503) {
    const data = e.data as KioskOfflineError | null
    return data?.error === 'kiosk_offline'
  }
  return false
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
        },
      )
      // Append + sort; snapshot refresh would also work but skips a round-trip.
      rows.value = [...rows.value, hydrateRow(created)].sort((a, b) =>
        a.instance_code.localeCompare(b.instance_code))
      toast.success(`Created ${created.instance_code}`)
    } else {
      // Edit: only the fields that actually changed; the server's PATCH
      // ignores fields that weren't touched, so we can send everything
      // editable. Code/notes/serial/rfid only — active changes go through
      // decommission/reactivate.
      const body: Record<string, string> = {
        code: form.value.code.trim(),
        serial: form.value.serial.trim(),
        rfid_epc: form.value.rfid_epc.trim(),
        notes: form.value.notes.trim(),
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
    active: r.active ?? true,
    notes: r.notes ?? '',
    created: r.created ?? '',
    updated: r.updated ?? '',
  }
}

function openToggle(r: InstanceRow, targetActive: boolean) {
  togglePending.value = { row: r, targetActive }
  toggleReason.value = ''
}

async function confirmToggle() {
  if (!togglePending.value) return
  if (!toggleReason.value.trim()) {
    toast.error('Reason is required')
    return
  }
  const { row, targetActive } = togglePending.value
  const verb = targetActive ? 'reactivate' : 'decommission'
  toggleSubmitting.value = true
  try {
    const updated = await api.post<InstanceRow>(
      `/api/controller/kiosks/${encodeURIComponent(props.kioskCode)}/instances/${encodeURIComponent(row.instance_code)}/${verb}`,
      { reason: toggleReason.value.trim() },
    )
    const idx = rows.value.findIndex((x) => x.instance_id === updated.instance_id)
    if (idx >= 0) rows.value[idx] = { ...rows.value[idx], active: updated.active }
    togglePending.value = null
    toast.success(`${updated.instance_code} ${verb}d`)
  } catch (e) {
    if (isOfflineError(e)) {
      offline.value = true
      togglePending.value = null
      toast.error('Kiosk is offline — change not applied')
    } else {
      toast.error((e as Error).message)
    }
  } finally {
    toggleSubmitting.value = false
  }
}

const columns: ColumnDef[] = [
  { key: 'item_code', label: 'Item' },
  { key: 'instance_code', label: 'Code' },
  { key: 'serial', label: 'Serial' },
  { key: 'rfid_epc', label: 'RFID' },
  { key: 'active', label: 'Active' },
  { key: '__actions', align: 'right' },
]
</script>

<template>
  <section class="space-y-3">
    <header class="flex items-center justify-between gap-3">
      <div>
        <h3 class="text-sm font-medium text-slate-200">Item instances</h3>
        <p class="text-xs text-slate-500">
          One row per serialized unit on this kiosk. Cosmetic edits don&rsquo;t
          audit; decommission / reactivate writes a lifecycle event.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <input
          v-model="itemFilter"
          type="text"
          placeholder="Filter by item code…"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-1.5 text-slate-100 text-sm w-44"
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
      :row-class="(r) => (r.active ? undefined : 'text-slate-500')"
      :page="page"
      :per-page="perPage"
      :total="filtered.length"
      @update:page="(p) => page = p"
      @update:per-page="(n) => { perPage = n; page = 1 }"
    >
      <template #cell-item_code="{ row }">
        <span class="font-mono">{{ row.item_code }}</span>
      </template>
      <template #cell-instance_code="{ row }">
        <span class="font-mono">{{ row.instance_code }}</span>
      </template>
      <template #cell-serial="{ row }">
        <span class="font-mono">{{ row.serial || '—' }}</span>
      </template>
      <template #cell-rfid_epc="{ row }">
        <span class="font-mono">{{ row.rfid_epc || '—' }}</span>
      </template>
      <template #cell-active="{ row }">
        <span v-if="row.active" class="text-emerald-400">●</span>
        <span v-else class="text-slate-600">●</span>
      </template>
      <template #cell-__actions="{ row }">
        <div class="inline-flex justify-end gap-2">
          <button
            type="button"
            class="px-3 py-1.5 rounded-md bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm border border-slate-700 whitespace-nowrap disabled:opacity-50"
            :disabled="offline"
            @click="openEdit(row)"
          >
            Edit
          </button>
          <button
            v-if="row.active"
            type="button"
            class="px-3 py-1.5 rounded-md bg-amber-950/60 hover:bg-amber-900/60 text-amber-200 text-sm border border-amber-800/70 whitespace-nowrap disabled:opacity-50"
            :disabled="offline"
            @click="openToggle(row, false)"
          >
            Decommission
          </button>
          <button
            v-else
            type="button"
            class="px-3 py-1.5 rounded-md bg-emerald-950/60 hover:bg-emerald-900/60 text-emerald-200 text-sm border border-emerald-800/70 whitespace-nowrap disabled:opacity-50"
            :disabled="offline"
            @click="openToggle(row, true)"
          >
            Reactivate
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
      :open="togglePending !== null"
      :title="togglePending?.targetActive ? 'Reactivate instance' : 'Decommission instance'"
      size="sm"
      @update:open="(v) => { if (!v) togglePending = null }"
    >
      <form class="flex flex-col gap-4" @submit.prevent="confirmToggle">
        <p class="text-slate-300 text-sm">
          {{ togglePending?.targetActive ? 'Reactivate' : 'Decommission' }}
          <span class="font-mono">{{ togglePending?.row.instance_code }}</span>?
          A reason is required for the audit log.
        </p>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Reason</span>
          <textarea
            v-model="toggleReason"
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
            @click="togglePending = null"
          >
            Cancel
          </button>
          <button
            type="submit"
            class="px-4 py-2 rounded-lg font-medium text-white disabled:opacity-50"
            :class="togglePending?.targetActive ? 'bg-emerald-600 hover:bg-emerald-500' : 'bg-red-600 hover:bg-red-500'"
            :disabled="toggleSubmitting"
          >
            {{ togglePending?.targetActive ? 'Reactivate' : 'Decommission' }}
          </button>
        </div>
      </form>
    </AppDialog>
  </section>
</template>
