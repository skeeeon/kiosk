<!-- KioskItemsPanel lists and edits the items a controller-side kiosk stocks.
     One row = one (kiosk, item) membership. Empty = the kiosk has nothing
     assigned. Embedded inside the AdminKioskDetailView Items tab (we need a
     kiosk id to FK against). -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import { useToast } from '../composables/useToast'
import DataTable, { type ColumnDef } from './DataTable.vue'
import type { ItemRecord, KioskItemRecord } from '../types'

const props = defineProps<{ kioskId: string }>()
const toast = useToast()

interface MembershipRow {
  id: string                  // kiosk_items row id
  itemId: string
  itemCode: string
  itemName: string
  itemCategory: string
  itemType: 'tool' | 'consumable'
}

const rows = ref<MembershipRow[]>([])
const allItems = ref<ItemRecord[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const page = ref(1)
const perPage = ref(25)

// Add-item picker state.
const pickerOpen = ref(false)
const pickerSearch = ref('')

// Bulk-add-by-category state.
const bulkOpen = ref(false)
const bulkCategory = ref<string>('')

async function load() {
  if (!props.kioskId) return
  loading.value = true
  error.value = null
  try {
    const [membershipRes, itemsRes] = await Promise.all([
      pb.collection('kiosk_items').getFullList<KioskItemRecord & { expand?: { item?: ItemRecord } }>({
        filter: pb.filter('kiosk = {:kiosk}', { kiosk: props.kioskId }),
        expand: 'item',
        sort: '+created',
      }),
      pb.collection('items').getFullList<ItemRecord>({ sort: '+code' }),
    ])
    rows.value = membershipRes
      .filter((r) => r.expand?.item)
      .map((r) => {
        const it = r.expand!.item!
        return {
          id: r.id,
          itemId: it.id,
          itemCode: it.code,
          itemName: it.name,
          itemCategory: it.category ?? '',
          itemType: it.type,
        }
      })
    allItems.value = itemsRes
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

watch(() => props.kioskId, (id) => { if (id) void load() }, { immediate: true })

const memberItemIds = computed(() => new Set(rows.value.map((r) => r.itemId)))

const availableItems = computed(() => {
  const q = pickerSearch.value.trim().toLowerCase()
  return allItems.value.filter((it) => {
    if (memberItemIds.value.has(it.id)) return false
    if (!q) return true
    return (
      it.code.toLowerCase().includes(q) ||
      it.name.toLowerCase().includes(q) ||
      it.category.toLowerCase().includes(q)
    )
  })
})

// Distinct categories across the whole catalog, for the bulk-add picker.
// Empty categories are dropped — the dropdown shouldn't offer "unset" as a
// batch target.
const categories = computed(() => {
  const set = new Set<string>()
  for (const it of allItems.value) if (it.category) set.add(it.category)
  return Array.from(set).sort()
})

const bulkPreview = computed(() => {
  if (!bulkCategory.value) return []
  return allItems.value.filter(
    (it) => it.category === bulkCategory.value && !memberItemIds.value.has(it.id),
  )
})

async function addOne(it: ItemRecord) {
  try {
    await pb.collection('kiosk_items').create({ kiosk: props.kioskId, item: it.id })
    await load()
    pickerSearch.value = ''
    toast.success(`Added ${it.code}`)
  } catch (e) {
    toast.error((e as Error).message)
  }
}

async function remove(row: MembershipRow) {
  if (!confirm(`Remove ${row.itemCode} from this kiosk? The kiosk will soft-deactivate the item locally; transaction history is kept.`)) return
  try {
    await pb.collection('kiosk_items').delete(row.id)
    await load()
    toast.success(`Removed ${row.itemCode}`)
  } catch (e) {
    toast.error((e as Error).message)
  }
}

async function applyBulkAdd() {
  const preview = bulkPreview.value
  if (preview.length === 0) {
    toast.error('Nothing to add — all items in this category are already assigned.')
    return
  }
  const ok = confirm(
    `Add ${preview.length} item(s) from category "${bulkCategory.value}" to this kiosk?`,
  )
  if (!ok) return
  let added = 0
  let failed = 0
  for (const it of preview) {
    try {
      await pb.collection('kiosk_items').create({ kiosk: props.kioskId, item: it.id })
      added++
    } catch {
      failed++
    }
  }
  await load()
  bulkOpen.value = false
  bulkCategory.value = ''
  if (failed > 0) {
    toast.error(`Added ${added}, failed ${failed}. Check console for details.`)
  } else {
    toast.success(`Added ${added} item(s)`)
  }
}

const columns: ColumnDef[] = [
  { key: 'itemCode', label: 'Code' },
  { key: 'itemName', label: 'Name' },
  { key: 'itemCategory', label: 'Category' },
  { key: 'itemType', label: 'Type' },
  { key: '__actions', align: 'right' },
]

const pagedRows = computed(() => {
  const start = (page.value - 1) * perPage.value
  return rows.value.slice(start, start + perPage.value)
})
</script>

<template>
  <section class="space-y-3">
    <header class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2">
      <div>
        <h3 class="text-sm font-medium text-slate-200">Stocked items</h3>
        <p class="text-xs text-slate-500">SKUs this kiosk carries. Empty kiosks receive nothing.</p>
      </div>
      <div class="flex gap-2">
        <button
          v-if="!pickerOpen && !bulkOpen"
          type="button"
          class="text-sm px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          @click="pickerOpen = true"
        >
          + Add item
        </button>
        <button
          v-if="!pickerOpen && !bulkOpen"
          type="button"
          class="text-sm px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          @click="bulkOpen = true"
        >
          Bulk add by category
        </button>
      </div>
    </header>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 text-sm px-3 py-2">
      {{ error }}
    </p>

    <!-- Add-item picker -->
    <div v-if="pickerOpen" class="rounded-lg bg-slate-900 border border-slate-800 p-3">
      <div class="flex items-center gap-2 mb-2">
        <input
          v-model="pickerSearch"
          type="search"
          placeholder="Search code, name, category…"
          class="flex-1 rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm text-slate-100"
        />
        <button
          type="button"
          class="text-sm px-3 py-1 rounded bg-slate-800 hover:bg-slate-700 text-slate-300"
          @click="pickerOpen = false; pickerSearch = ''"
        >
          Done
        </button>
      </div>
      <div class="max-h-48 overflow-y-auto divide-y divide-slate-800">
        <p v-if="availableItems.length === 0" class="text-xs text-slate-500 py-2 text-center">
          No matching items available to add.
        </p>
        <button
          v-for="it in availableItems.slice(0, 50)"
          :key="it.id"
          type="button"
          class="w-full text-left px-2 py-2 hover:bg-slate-800/60 flex items-center gap-3"
          @click="addOne(it)"
        >
          <span class="font-mono text-slate-200 text-sm">{{ it.code }}</span>
          <span class="text-slate-400 text-xs truncate flex-1">{{ it.name }}</span>
          <span v-if="it.category" class="text-slate-500 text-xs">{{ it.category }}</span>
        </button>
      </div>
    </div>

    <!-- Bulk-add-by-category picker -->
    <div v-if="bulkOpen" class="rounded-lg bg-slate-900 border border-slate-800 p-3">
      <div class="flex items-center gap-2 mb-2">
        <select
          v-model="bulkCategory"
          class="flex-1 rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm text-slate-100"
        >
          <option value="">Select a category…</option>
          <option v-for="c in categories" :key="c" :value="c">{{ c }}</option>
        </select>
        <button
          type="button"
          class="text-sm px-3 py-1 rounded bg-brand-primary hover:bg-brand-primary-hover text-white"
          :disabled="!bulkCategory || bulkPreview.length === 0"
          @click="applyBulkAdd"
        >
          Add {{ bulkPreview.length }}
        </button>
        <button
          type="button"
          class="text-sm px-3 py-1 rounded bg-slate-800 hover:bg-slate-700 text-slate-300"
          @click="bulkOpen = false; bulkCategory = ''"
        >
          Cancel
        </button>
      </div>
      <p v-if="bulkCategory && bulkPreview.length === 0" class="text-xs text-slate-500">
        All items in "{{ bulkCategory }}" are already assigned to this kiosk.
      </p>
      <ul v-else-if="bulkCategory" class="max-h-32 overflow-y-auto text-xs text-slate-400">
        <li v-for="it in bulkPreview" :key="it.id" class="py-0.5">
          <span class="font-mono text-slate-300">{{ it.code }}</span>
          — {{ it.name }}
        </li>
      </ul>
    </div>

    <DataTable
      :columns="columns"
      :rows="pagedRows"
      :row-key="(r) => r.id"
      :loading="loading"
      empty-text='No items assigned. Use "Add item" or "Bulk add by category" to stock this kiosk.'
      :page="page"
      :per-page="perPage"
      :total="rows.length"
      @update:page="(p) => page = p"
      @update:per-page="(n) => { perPage = n; page = 1 }"
    >
      <template #cell-itemCode="{ row }">
        <span class="font-mono text-slate-200">{{ row.itemCode }}</span>
      </template>
      <template #cell-itemName="{ row }">
        <span class="text-slate-300">{{ row.itemName }}</span>
      </template>
      <template #cell-itemCategory="{ row }">
        <span class="text-slate-400">{{ row.itemCategory || '—' }}</span>
      </template>
      <template #cell-itemType="{ row }">
        <span
          class="inline-block px-2 py-0.5 rounded text-xs"
          :class="row.itemType === 'tool' ? 'bg-amber-900/60 text-amber-200' : 'bg-sky-900/60 text-sky-200'"
        >{{ row.itemType }}</span>
      </template>
      <template #cell-__actions="{ row }">
        <button
          type="button"
          class="px-3 py-1.5 rounded-md bg-red-950/60 hover:bg-red-900/60 text-red-200 text-sm border border-red-800/70 whitespace-nowrap"
          @click="remove(row)"
        >
          Remove
        </button>
      </template>
    </DataTable>
  </section>
</template>
