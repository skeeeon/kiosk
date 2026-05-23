<!-- DataTable is a presentation-only table card.
     - Caller passes `rows` (already paged/filtered/sorted server-side, or the
       full list for client-side modes) and `columns` describing headers.
     - Per-cell rendering goes through the `cell-<key>` slot; the default is
       to text-render `row[col.key]`.
     - If `total` is provided, the component renders a Prev/Next pagination
       footer and emits `update:page` / `update:perPage`. Caller refetches.
     - Sort is opt-in per column; the component just emits the new state.

     This keeps the table presentational: no internal filtering, sorting, or
     fetching. List views orchestrate, DataTable presents. -->
<script setup lang="ts" generic="T">
import { computed } from 'vue'

export type ColumnDef = {
  key: string
  label?: string
  align?: 'left' | 'right' | 'center'
  headerClass?: string
  cellClass?: string
  width?: string
  sortable?: boolean
}

export type SortState = { key: string; dir: 'asc' | 'desc' } | null

const props = withDefaults(
  defineProps<{
    columns: ColumnDef[]
    rows: T[]
    rowKey: (row: T) => string
    loading?: boolean
    error?: string | null
    emptyText?: string
    rowClass?: (row: T) => string | undefined
    rowClickable?: boolean
    // Pagination. When `total` is undefined, the footer is hidden and the
    // caller is responsible for showing all rows it wants to display.
    page?: number
    perPage?: number
    total?: number
    pageSizeOptions?: number[]
    // Sort state. v-model:sort.
    sort?: SortState
  }>(),
  {
    loading: false,
    error: null,
    emptyText: 'No results.',
    rowClickable: false,
    page: 1,
    perPage: 25,
    pageSizeOptions: () => [25, 50, 100],
    sort: null,
  },
)

const emit = defineEmits<{
  'update:page': [page: number]
  'update:perPage': [perPage: number]
  'update:sort': [sort: SortState]
  'row-click': [row: T]
}>()

const totalPages = computed(() => {
  if (props.total === undefined) return 1
  return Math.max(1, Math.ceil(props.total / props.perPage))
})

const rangeLabel = computed(() => {
  if (props.total === undefined || props.total === 0) return ''
  const start = (props.page - 1) * props.perPage + 1
  const end = Math.min(props.total, props.page * props.perPage)
  return `${start}–${end} of ${props.total}`
})

const showPagination = computed(() => props.total !== undefined)

// When the table is row-clickable we render a thin right-edge chevron column
// so the affordance is visible without hovering. Same colspan math used for
// the loading / empty / error rows so they still span the full table.
const totalCols = computed(() => props.columns.length + (props.rowClickable ? 1 : 0))

function alignClass(align?: 'left' | 'right' | 'center') {
  if (align === 'right') return 'text-right'
  if (align === 'center') return 'text-center'
  return 'text-left'
}

// 2-state sort cycle: clicking a different column starts at asc; clicking the
// same column toggles direction. Returning to null requires the caller to set
// sort = null externally (e.g. via a "clear sort" affordance).
function onSortClick(col: ColumnDef) {
  if (!col.sortable) return
  const current = props.sort
  if (current?.key === col.key) {
    emit('update:sort', { key: col.key, dir: current.dir === 'asc' ? 'desc' : 'asc' })
  } else {
    emit('update:sort', { key: col.key, dir: 'asc' })
  }
}

function goPrev() {
  if (props.page > 1) emit('update:page', props.page - 1)
}
function goNext() {
  if (props.page < totalPages.value) emit('update:page', props.page + 1)
}

function onPerPageChange(e: Event) {
  const v = Number((e.target as HTMLSelectElement).value)
  if (Number.isFinite(v) && v > 0) emit('update:perPage', v)
}

function onRowClick(row: T) {
  if (props.rowClickable) emit('row-click', row)
}
</script>

<template>
  <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
    <div class="overflow-x-auto">
      <table class="w-full text-left text-sm">
        <thead class="bg-slate-950/70 text-slate-400">
          <tr>
            <th
              v-for="col in columns"
              :key="col.key"
              :class="[
                'px-4 py-3 font-medium select-none',
                alignClass(col.align),
                col.headerClass,
                col.sortable ? 'cursor-pointer hover:text-slate-200' : '',
              ]"
              :style="col.width ? { width: col.width } : undefined"
              @click="onSortClick(col)"
            >
              <span class="inline-flex items-center gap-1">
                {{ col.label ?? '' }}
                <svg
                  v-if="col.sortable && sort?.key === col.key"
                  xmlns="http://www.w3.org/2000/svg"
                  viewBox="0 0 20 20"
                  fill="currentColor"
                  class="h-3 w-3 text-slate-300"
                >
                  <path v-if="sort.dir === 'asc'" d="M10 4l5 6H5l5-6Z" />
                  <path v-else d="M10 16l-5-6h10l-5 6Z" />
                </svg>
                <svg
                  v-else-if="col.sortable"
                  xmlns="http://www.w3.org/2000/svg"
                  viewBox="0 0 20 20"
                  fill="currentColor"
                  class="h-3 w-3 text-slate-600"
                >
                  <path d="M6 8l4-4 4 4H6Zm0 4l4 4 4-4H6Z" />
                </svg>
              </span>
            </th>
            <th v-if="rowClickable" class="w-8" aria-hidden="true" />
          </tr>
        </thead>
        <tbody class="divide-y divide-slate-800">
          <tr v-if="loading">
            <td :colspan="totalCols" class="text-center text-slate-500 py-8">Loading…</td>
          </tr>
          <tr v-else-if="error">
            <td :colspan="totalCols" class="text-center py-8">
              <slot name="error" :error="error">
                <span class="text-red-300">{{ error }}</span>
              </slot>
            </td>
          </tr>
          <tr v-else-if="rows.length === 0">
            <td :colspan="totalCols" class="text-center text-slate-500 py-8">
              <slot name="empty">{{ emptyText }}</slot>
            </td>
          </tr>
          <tr
            v-for="(row, index) in rows"
            v-else
            :key="rowKey(row)"
            :class="[
              rowClickable ? 'group hover:bg-slate-800/50 cursor-pointer' : '',
              rowClass?.(row) ?? '',
            ]"
            @click="onRowClick(row)"
          >
            <td
              v-for="col in columns"
              :key="col.key"
              :class="['px-4 py-3', alignClass(col.align), col.cellClass]"
            >
              <slot :name="`cell-${col.key}`" :row="row" :index="index">
                {{ (row as Record<string, unknown>)[col.key] ?? '' }}
              </slot>
            </td>
            <td v-if="rowClickable" class="pr-3 pl-0 text-right">
              <svg
                xmlns="http://www.w3.org/2000/svg"
                viewBox="0 0 20 20"
                fill="currentColor"
                class="inline-block h-4 w-4 text-slate-600 group-hover:text-slate-300 transition-colors"
                aria-hidden="true"
              >
                <path fill-rule="evenodd" d="M7.21 14.77a.75.75 0 0 1 .02-1.06L10.44 10 7.23 6.29a.75.75 0 1 1 1.06-1.06l3.75 4.25a.75.75 0 0 1 0 1.04l-3.75 4.25a.75.75 0 0 1-1.06.02Z" clip-rule="evenodd" />
              </svg>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- Pagination footer. Hidden when `total` is undefined. -->
    <div
      v-if="showPagination"
      class="flex flex-wrap items-center justify-between gap-3 px-4 py-3 border-t border-slate-800 text-sm text-slate-400"
    >
      <span class="tabular-nums">{{ rangeLabel || '0 results' }}</span>
      <div class="flex items-center gap-2">
        <button
          type="button"
          class="px-3 py-1.5 rounded bg-slate-800 hover:bg-slate-700 text-slate-200 disabled:opacity-50 disabled:cursor-not-allowed"
          :disabled="page <= 1"
          @click="goPrev"
        >
          Previous
        </button>
        <span class="tabular-nums">Page {{ page }} of {{ totalPages }}</span>
        <button
          type="button"
          class="px-3 py-1.5 rounded bg-slate-800 hover:bg-slate-700 text-slate-200 disabled:opacity-50 disabled:cursor-not-allowed"
          :disabled="page >= totalPages"
          @click="goNext"
        >
          Next
        </button>
      </div>
      <label class="flex items-center gap-2">
        <span class="sr-only">Rows per page</span>
        <select
          :value="perPage"
          class="rounded bg-slate-900 border border-slate-800 px-2 py-1 text-slate-200"
          @change="onPerPageChange"
        >
          <option v-for="opt in pageSizeOptions" :key="opt" :value="opt">{{ opt }} / page</option>
        </select>
      </label>
    </div>
  </div>
</template>
