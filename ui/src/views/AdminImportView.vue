<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import { download } from '../lib/api'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import DataTable, { type ColumnDef } from '../components/DataTable.vue'

const { identity } = useKioskIdentity()
const managed = computed(() => identity.value?.managed ?? false)
const isController = computed(() => identity.value?.role === 'controller')

interface ImportError {
  code: string
  message: string
}

interface RowResult {
  row: number
  code: string
  name: string
  action: 'insert' | 'update' | 'error'
  errors?: ImportError[]
}

interface ImportResult {
  dry_run: boolean
  rows_total: number
  rows_inserted: number
  rows_updated: number
  rows_errored: number
  rows: RowResult[]
}

type Kind = 'items' | 'users' | 'groups'
// All three kinds are available on both binaries. Managed kiosks see the
// read-only banner below instead of any tab because their catalog/workers/
// groups are owned by the controller.
const availableKinds: Kind[] = ['items', 'users', 'groups']
const kind = ref<Kind>('items')

const KIND_META: Record<Kind, { label: string; columns: string; required: string }> = {
  items: {
    label: 'Items',
    columns: 'code, name, type, unit, tracking_mode, category, active, notes, quantity_on_hand, reorder_threshold',
    required: 'code, name, type (tool|consumable); tracking_mode defaults to quantity',
  },
  users: {
    label: 'Workers',
    columns: 'code, name, email, role, group, active',
    required: 'code, name; role defaults to worker, group auto-creates if missing',
  },
  groups: {
    label: 'Groups',
    columns: 'code, name, contact_email, contact_phone, notes, active',
    required: 'code, name',
  },
}

const file = ref<File | null>(null)
const dryRun = ref(true)
const submitting = ref(false)
const result = ref<ImportResult | null>(null)
const error = ref<string | null>(null)

// Result-table state. Reset on every tab switch + every new submit.
type Filter = 'all' | 'insert' | 'update' | 'error'
const filter = ref<Filter>('all')
const page = ref(1)
const perPage = ref(50)

function resetResult() {
  result.value = null
  error.value = null
  filter.value = 'all'
  page.value = 1
}
watch(kind, () => {
  file.value = null
  resetResult()
})
watch(filter, () => {
  page.value = 1
})

function onFileChange(e: Event) {
  const input = e.target as HTMLInputElement
  file.value = input.files?.[0] ?? null
  resetResult()
}

async function onSubmit() {
  if (!file.value || submitting.value) return
  submitting.value = true
  error.value = null
  result.value = null
  filter.value = 'all'
  page.value = 1
  try {
    const formData = new FormData()
    formData.append('file', file.value)
    formData.append('dry_run', dryRun.value ? 'true' : 'false')

    result.value = await pb.send<ImportResult>(`/api/kiosk/${kind.value}/import`, {
      method: 'POST',
      body: formData,
    })
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    submitting.value = false
  }
}

async function onDownloadTemplate() {
  try {
    await download(
      `/api/kiosk/${kind.value}/import/template`,
      `${kind.value}-template.csv`,
    )
  } catch (e) {
    error.value = (e as Error).message
  }
}

// Filtered + paged result rows for the table. Client-side because the
// importer returns the whole set in one response — even tens of thousands
// of rows are cheap to slice in memory, and a server-paged endpoint here
// would require persisting the run.
const filteredRows = computed<RowResult[]>(() => {
  if (!result.value) return []
  if (filter.value === 'all') return result.value.rows
  return result.value.rows.filter((r) => r.action === filter.value)
})
const pagedRows = computed<RowResult[]>(() => {
  const start = (page.value - 1) * perPage.value
  return filteredRows.value.slice(start, start + perPage.value)
})

// Action labels reflect dry-run vs real-run: dry-run reports "would
// insert / would update" so the operator knows nothing's been written.
const actionLabel = computed<(action: RowResult['action']) => string>(() => {
  const dry = result.value?.dry_run ?? false
  return (action) => {
    if (action === 'error') return 'Error'
    if (action === 'insert') return dry ? 'Would insert' : 'Inserted'
    return dry ? 'Would update' : 'Updated'
  }
})

function actionBadgeClass(action: RowResult['action']): string {
  if (action === 'error') return 'bg-red-900/40 text-red-300 border border-red-800'
  if (action === 'insert') return 'bg-emerald-900/40 text-emerald-300 border border-emerald-800'
  return 'bg-sky-900/40 text-sky-300 border border-sky-800'
}

function errorDetail(row: RowResult): string {
  if (!row.errors || row.errors.length === 0) return ''
  return row.errors.map((e) => `${e.code}: ${e.message}`).join('; ')
}

const columns: ColumnDef[] = [
  { key: 'row', label: 'Row', width: '5rem', cellClass: 'font-mono text-slate-400' },
  { key: 'code', label: 'Code', cellClass: 'font-mono text-slate-200' },
  { key: 'name', label: 'Name', cellClass: 'text-slate-200' },
  { key: 'action', label: 'Status', width: '10rem' },
  { key: 'detail', label: 'Detail', cellClass: 'text-slate-400' },
]
</script>

<template>
  <main class="p-6 max-w-7xl mx-auto w-full">
    <h1 class="text-2xl font-semibold mb-2">Import</h1>

    <div
      v-if="managed && !isController"
      class="rounded-lg bg-sky-950/60 border border-sky-800 text-sky-200 px-4 py-3 mb-4"
    >
      Catalog is managed by the controller — import there instead. This
      kiosk receives updates over JetStream as the central catalog changes.
    </div>

    <template v-if="!(managed && !isController)">
      <div class="flex gap-1 border-b border-slate-800 mb-4 tab-scroll">
        <button
          v-for="k in availableKinds"
          :key="k"
          type="button"
          :class="[
            'px-4 py-2 -mb-px border-b-2 text-sm font-medium transition-colors whitespace-nowrap shrink-0',
            kind === k
              ? 'border-brand-primary text-slate-100'
              : 'border-transparent text-slate-400 hover:text-slate-200',
          ]"
          @click="kind = k"
        >
          {{ KIND_META[k].label }}
        </button>
      </div>

      <p class="text-sm text-slate-400 mb-6">
        Upload a CSV with columns:
        <code class="font-mono text-slate-300">{{ KIND_META[kind].columns }}</code>.
        Required: {{ KIND_META[kind].required }}. Rows match existing records by
        <code class="font-mono text-slate-300">code</code> (upsert); records not in the CSV are left alone.
      </p>

      <form
        class="bg-slate-900 border border-slate-800 rounded-2xl p-6 flex flex-col gap-4"
        @submit.prevent="onSubmit"
      >
        <input
          type="file"
          accept=".csv,text/csv"
          class="block w-full text-slate-300 file:mr-4 file:py-2 file:px-4 file:rounded-lg file:border-0 file:bg-brand-primary file:hover:bg-brand-primary-hover file:text-white file:cursor-pointer"
          @change="onFileChange"
        />

        <label class="flex items-center gap-2 text-slate-300">
          <input v-model="dryRun" type="checkbox" class="w-4 h-4" />
          Dry run — validate only, don't write changes
        </label>

        <div class="flex justify-between items-center gap-3 flex-wrap">
          <button
            type="button"
            class="px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm font-medium"
            @click="onDownloadTemplate"
          >
            Download {{ KIND_META[kind].label.toLowerCase() }} template
          </button>
          <button
            type="submit"
            :disabled="!file || submitting"
            class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover disabled:bg-slate-700 text-white font-medium"
          >
            {{ submitting ? 'Processing…' : dryRun ? 'Validate' : 'Import' }}
          </button>
        </div>

        <p v-if="error" class="text-red-400">{{ error }}</p>
      </form>

      <section v-if="result" class="mt-6 flex flex-col gap-4">
        <header class="flex items-center justify-between flex-wrap gap-3">
          <h2 class="text-xl font-semibold">
            {{ result.dry_run ? 'Validation results' : 'Import results' }}
          </h2>
          <div
            v-if="!result.dry_run && result.rows_errored === 0"
            class="rounded-lg bg-emerald-900/40 border border-emerald-700/60 text-emerald-200 px-3 py-1.5 text-sm"
          >
            Import complete — no errors.
          </div>
          <div
            v-else-if="result.dry_run && result.rows_errored === 0"
            class="rounded-lg bg-sky-900/40 border border-sky-700/60 text-sky-200 px-3 py-1.5 text-sm"
          >
            Validation passed — uncheck "Dry run" and submit to write.
          </div>
        </header>

        <!-- Summary cards double as filter chips: clicking switches the
             table view, with the current selection highlighted. -->
        <ul class="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <li>
            <button
              type="button"
              class="w-full rounded-lg bg-slate-800 p-3 text-center transition-colors hover:bg-slate-700/70 border-2"
              :class="filter === 'all' ? 'border-brand-primary' : 'border-transparent'"
              @click="filter = 'all'"
            >
              <p class="text-2xl font-semibold">{{ result.rows_total }}</p>
              <p class="text-xs text-slate-400 uppercase tracking-wide">Total rows</p>
            </button>
          </li>
          <li>
            <button
              type="button"
              class="w-full rounded-lg bg-slate-800 p-3 text-center transition-colors hover:bg-slate-700/70 border-2"
              :class="filter === 'insert' ? 'border-brand-primary' : 'border-transparent'"
              @click="filter = 'insert'"
            >
              <p class="text-2xl font-semibold text-emerald-400">{{ result.rows_inserted }}</p>
              <p class="text-xs text-slate-400 uppercase tracking-wide">
                {{ result.dry_run ? 'Would insert' : 'Inserted' }}
              </p>
            </button>
          </li>
          <li>
            <button
              type="button"
              class="w-full rounded-lg bg-slate-800 p-3 text-center transition-colors hover:bg-slate-700/70 border-2"
              :class="filter === 'update' ? 'border-brand-primary' : 'border-transparent'"
              @click="filter = 'update'"
            >
              <p class="text-2xl font-semibold text-sky-400">{{ result.rows_updated }}</p>
              <p class="text-xs text-slate-400 uppercase tracking-wide">
                {{ result.dry_run ? 'Would update' : 'Updated' }}
              </p>
            </button>
          </li>
          <li>
            <button
              type="button"
              class="w-full rounded-lg bg-slate-800 p-3 text-center transition-colors hover:bg-slate-700/70 border-2"
              :class="filter === 'error' ? 'border-brand-primary' : 'border-transparent'"
              @click="filter = 'error'"
            >
              <p
                class="text-2xl font-semibold"
                :class="result.rows_errored > 0 ? 'text-red-400' : 'text-slate-500'"
              >
                {{ result.rows_errored }}
              </p>
              <p class="text-xs text-slate-400 uppercase tracking-wide">Errors</p>
            </button>
          </li>
        </ul>

        <DataTable
          :columns="columns"
          :rows="pagedRows"
          :row-key="(r: RowResult) => String(r.row)"
          :page="page"
          :per-page="perPage"
          :total="filteredRows.length"
          :page-size-options="[25, 50, 100, 250]"
          empty-text="No rows match this filter."
          @update:page="page = $event"
          @update:per-page="(v: number) => { perPage = v; page = 1 }"
        >
          <template #cell-action="{ row }: { row: RowResult }">
            <span
              class="inline-block px-2 py-0.5 rounded-md text-xs font-medium"
              :class="actionBadgeClass(row.action)"
            >
              {{ actionLabel(row.action) }}
            </span>
          </template>
          <template #cell-detail="{ row }: { row: RowResult }">
            {{ errorDetail(row) }}
          </template>
        </DataTable>
      </section>
    </template>
  </main>
</template>
