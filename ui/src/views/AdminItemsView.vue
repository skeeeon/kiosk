<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import { download } from '../lib/api'
import ItemDialog from '../components/ItemDialog.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import DataTable, { type ColumnDef } from '../components/DataTable.vue'
import { useAdminToast } from '../composables/useAdminToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import { useListShortcuts } from '../composables/useListShortcuts'
import { useUrlQuerySync } from '../composables/useUrlQuerySync'
import type { ItemRecord } from '../types'

const toast = useAdminToast()
const { identity } = useKioskIdentity()
const managed = computed(() => identity.value?.managed ?? false)
const isController = computed(() => identity.value?.role === 'controller')

const items = ref<ItemRecord[]>([])
const instanceCounts = ref<Record<string, number>>({})
// Open-checkout count keyed by item id. Drives the "Out" column and the
// low-stock row highlight; consumables stay at zero since they don't track
// open_checkouts. These aggregates cover the whole catalog (not just the
// visible page) — their cardinality is bounded by what's currently out and
// total serialized stock, not by the items collection itself.
const outCounts = ref<Record<string, number>>({})
const loading = ref(false)
const error = ref<string | null>(null)
const search = ref('')
const typeFilter = ref<'all' | 'tool' | 'consumable'>('all')
const page = ref(1)
const perPage = ref(50)
const total = ref(0)

const editing = ref<Partial<ItemRecord> | null>(null)
const deleting = ref<ItemRecord | null>(null)
const searchInput = ref<HTMLInputElement | null>(null)

useUrlQuerySync({
  page: { ref: page, default: 1, parse: (v) => Number(v) || 1 },
  q: { ref: search, default: '' },
  type: { ref: typeFilter, default: 'all' },
})

function pbEscape(s: string): string {
  return s.replace(/\\/g, '\\\\').replace(/"/g, '\\"')
}

function buildFilter(): string {
  const clauses: string[] = []
  const q = search.value.trim()
  if (q) {
    const safe = pbEscape(q)
    clauses.push(`(code ~ "${safe}" || name ~ "${safe}" || category ~ "${safe}")`)
  }
  if (typeFilter.value !== 'all') {
    clauses.push(`type = "${typeFilter.value}"`)
  }
  return clauses.join(' && ')
}

async function loadAggregates() {
  // Controller has no open_checkouts or item_instances rows of its own.
  if (isController.value) {
    instanceCounts.value = {}
    outCounts.value = {}
    return
  }
  try {
    const [instances, opens] = await Promise.all([
      pb.collection('item_instances').getFullList<{ item: string }>({ fields: 'item' }),
      pb.collection('open_checkouts').getFullList<{ item: string }>({ fields: 'item' }),
    ])
    const inst: Record<string, number> = {}
    for (const i of instances) inst[i.item] = (inst[i.item] ?? 0) + 1
    instanceCounts.value = inst
    const oc: Record<string, number> = {}
    for (const o of opens) oc[o.item] = (oc[o.item] ?? 0) + 1
    outCounts.value = oc
  } catch {
    // Aggregates are best-effort — keep the row data rendering even if these
    // queries fail. The Out / Available columns will just show 0s.
  }
}

async function load() {
  loading.value = true
  error.value = null
  try {
    const filter = buildFilter()
    const res = await pb.collection('items').getList<ItemRecord>(page.value, perPage.value, {
      filter,
      sort: '+code',
    })
    items.value = res.items
    total.value = res.totalItems
    page.value = res.page
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

function outFor(item: ItemRecord): number {
  return item.type === 'tool' ? (outCounts.value[item.id] ?? 0) : 0
}

function availableFor(item: ItemRecord): number {
  if (item.type === 'tool') {
    return Math.max(0, (item.quantity_on_hand ?? 0) - outFor(item))
  }
  return item.quantity_on_hand ?? 0
}

function isLowStock(item: ItemRecord): boolean {
  const t = item.reorder_threshold ?? 0
  return t > 0 && availableFor(item) <= t
}

onMounted(async () => {
  await Promise.all([load(), loadAggregates()])
})

// Debounce search; type-filter changes fire immediately since they're a single
// click and the user has no expectation of further input.
let searchTimer: ReturnType<typeof setTimeout> | null = null
watch(search, () => {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    page.value = 1
    void load()
  }, 250)
})
watch(typeFilter, () => {
  page.value = 1
  void load()
})
onUnmounted(() => {
  if (searchTimer) clearTimeout(searchTimer)
})

const visibleColumns = computed<ColumnDef[]>(() => {
  const cols: ColumnDef[] = [
    { key: 'code', label: 'Code' },
    { key: 'name', label: 'Name' },
    { key: 'type', label: 'Type' },
    { key: 'tracking_mode', label: 'Tracking' },
  ]
  if (!isController.value) {
    cols.push(
      { key: 'on_hand', label: 'On hand', align: 'right' },
      { key: 'out', label: 'Out', align: 'right' },
      { key: 'available', label: 'Available', align: 'right' },
      { key: 'threshold', label: 'Threshold', align: 'right' },
    )
  }
  cols.push(
    { key: 'category', label: 'Category' },
    { key: 'active', label: 'Active' },
    { key: '__actions', align: 'right' },
  )
  return cols
})

const emptyText = computed(() => {
  const hasFilter = search.value.trim() !== '' || typeFilter.value !== 'all'
  return hasFilter
    ? 'No items match your filter.'
    : 'No items yet. Click "New item" to add one.'
})

function onPageChange(p: number) {
  page.value = p
  void load()
}
function onPerPageChange(n: number) {
  perPage.value = n
  page.value = 1
  void load()
}

function openNew() {
  editing.value = {}
}

// "/" focuses the search box, "n" opens the New item sheet (skipped when
// managed since the create button is hidden in that mode).
useListShortcuts({
  searchInput,
  onNew: openNew,
  canCreate: computed(() => !managed.value),
})

function openEdit(item: ItemRecord) {
  editing.value = { ...item }
}

// Shared inner save. Returns true on success so the caller can decide
// whether to close the sheet or reseed it for another entry.
async function persistSave(data: Partial<ItemRecord>): Promise<boolean> {
  const isEdit = !!data.id
  if (isEdit) {
    await pb.collection('items').update<ItemRecord>(data.id!, data)
  } else {
    await pb.collection('items').create<ItemRecord>(data)
  }
  return isEdit
}

async function onSave(data: Partial<ItemRecord>) {
  error.value = null
  try {
    const wasEdit = await persistSave(data)
    editing.value = null
    await Promise.all([load(), loadAggregates()])
    toast.success(wasEdit ? `Saved ${data.code ?? 'item'}` : `Created ${data.code ?? 'item'}`)
  } catch (e) {
    const msg = (e as Error).message
    error.value = msg
    toast.error(msg)
  }
}

async function onSaveAndAdd(data: Partial<ItemRecord>) {
  error.value = null
  try {
    await persistSave(data)
    // Reseed: fresh {} triggers the dialog's prop-identity watch to reset
    // the form. Sheet stays open.
    editing.value = {}
    await Promise.all([load(), loadAggregates()])
    toast.success(`Created ${data.code ?? 'item'} — ready for next`)
  } catch (e) {
    const msg = (e as Error).message
    error.value = msg
    toast.error(msg)
  }
}

// Items referenced by any transaction line or open_checkouts row are FK-pinned;
// PB rejects hard delete. Surface a friendly message and suggest soft-delete.
function isFKConstraintError(msg: string): boolean {
  const m = msg.toLowerCase()
  return m.includes('foreign key') || m.includes('constraint') || m.includes('referenced')
}

async function exportCsv() {
  try {
    await download('/api/kiosk/items.csv')
  } catch (e) {
    toast.error(`Export failed: ${(e as Error).message}`)
  }
}

async function onDelete() {
  if (!deleting.value) return
  error.value = null
  const target = deleting.value
  try {
    await pb.collection('items').delete(target.id)
    deleting.value = null
    await Promise.all([load(), loadAggregates()])
    toast.success(`Deleted ${target.code}`)
  } catch (e) {
    const raw = (e as Error).message
    const friendly = isFKConstraintError(raw)
      ? `${target.code} has transaction history and can't be deleted. Uncheck "Active" instead to retire it.`
      : raw
    error.value = friendly
    toast.error(friendly)
  }
}
</script>

<template>
  <main class="p-6 max-w-7xl mx-auto w-full">
    <header class="flex items-baseline justify-between mb-4">
      <div>
        <h1 class="text-2xl font-semibold">Items</h1>
        <p class="text-sm text-slate-400">{{ total }} total</p>
      </div>
      <div class="flex items-center gap-3">
        <button
          type="button"
          class="px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
          @click="exportCsv"
        >
          Export CSV
        </button>
        <button
          v-if="!managed"
          type="button"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
          @click="openNew"
        >
          New item
        </button>
      </div>
    </header>

    <div class="flex gap-3 mb-4">
      <input
        ref="searchInput"
        v-model="search"
        type="search"
        placeholder="Search code, name, category… (press / to focus)"
        class="flex-1 rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100"
      />
      <select
        v-model="typeFilter"
        class="rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100"
      >
        <option value="all">All types</option>
        <option value="tool">Tools</option>
        <option value="consumable">Consumables</option>
      </select>
    </div>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 mb-3">
      {{ error }}
    </p>

    <DataTable
      :columns="visibleColumns"
      :rows="items"
      :row-key="(i) => i.id"
      :loading="loading"
      :empty-text="emptyText"
      row-clickable
      :row-class="(i) => (!isController && isLowStock(i) ? 'bg-red-950/30' : undefined)"
      :page="page"
      :per-page="perPage"
      :total="total"
      @row-click="openEdit"
      @update:page="onPageChange"
      @update:per-page="onPerPageChange"
    >
      <template #cell-code="{ row }">
        <span class="font-mono text-slate-200">{{ row.code }}</span>
      </template>
      <template #cell-type="{ row }">
        <span
          class="inline-block px-2 py-0.5 rounded text-xs"
          :class="row.type === 'tool' ? 'bg-amber-900/60 text-amber-200' : 'bg-sky-900/60 text-sky-200'"
        >
          {{ row.type }}
        </span>
      </template>
      <template #cell-tracking_mode="{ row }">
        <span class="text-slate-400">{{ row.tracking_mode }}</span>
        <span
          v-if="row.tracking_mode === 'serialized'"
          class="ml-1 inline-block px-1.5 rounded text-[10px] bg-slate-800 text-slate-300"
          :title="`${instanceCounts[row.id] ?? 0} instance(s)`"
        >{{ instanceCounts[row.id] ?? 0 }} inst</span>
      </template>
      <template #cell-on_hand="{ row }">
        <span class="tabular-nums text-slate-300">{{ row.quantity_on_hand ?? 0 }}</span>
      </template>
      <template #cell-out="{ row }">
        <span class="tabular-nums text-slate-400">{{ row.type === 'tool' ? outFor(row) : '—' }}</span>
      </template>
      <template #cell-available="{ row }">
        <span
          class="tabular-nums font-semibold"
          :class="isLowStock(row) ? 'text-red-400' : 'text-slate-300'"
        >
          {{ availableFor(row) }}
        </span>
      </template>
      <template #cell-threshold="{ row }">
        <span class="tabular-nums text-slate-400">{{ row.reorder_threshold ?? 0 }}</span>
      </template>
      <template #cell-category="{ row }">
        <span class="text-slate-400">{{ row.category || '—' }}</span>
      </template>
      <template #cell-active="{ row }">
        <span v-if="row.active" class="text-emerald-400">●</span>
        <span v-else class="text-slate-600">●</span>
      </template>
      <template #cell-__actions="{ row }">
        <button
          v-if="!managed"
          type="button"
          class="text-red-400 hover:text-red-300 px-2 py-1"
          @click.stop="deleting = row"
        >
          Delete
        </button>
      </template>
    </DataTable>

    <ItemDialog
      :open="editing !== null"
      :item="editing"
      :managed="managed"
      :is-controller="isController"
      @update:open="(v) => { if (!v) { editing = null; void load() } }"
      @save="onSave"
      @save-and-add-another="onSaveAndAdd"
    />

    <ConfirmDialog
      :open="deleting !== null"
      title="Delete item"
      :message="deleting ? `Delete ${deleting.code} — ${deleting.name}? This can't be undone.` : ''"
      confirm-label="Delete"
      destructive
      @update:open="(v) => { if (!v) deleting = null }"
      @confirm="onDelete"
    />
  </main>
</template>
