<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { pb } from '../lib/pb'
import { download } from '../lib/api'
import ItemDialog from '../components/ItemDialog.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import { useAdminToast } from '../composables/useAdminToast'
import type { ItemRecord } from '../types'

const toast = useAdminToast()

const items = ref<ItemRecord[]>([])
const instanceCounts = ref<Record<string, number>>({})
const loading = ref(false)
const error = ref<string | null>(null)
const search = ref('')
const typeFilter = ref<'all' | 'tool' | 'consumable'>('all')

const editing = ref<Partial<ItemRecord> | null>(null)
const deleting = ref<ItemRecord | null>(null)

async function load() {
  loading.value = true
  error.value = null
  try {
    const [itemsRes, instancesRes] = await Promise.all([
      pb.collection('items').getList<ItemRecord>(1, 500, { sort: '+code' }),
      pb.collection('item_instances').getFullList<{ item: string }>({ fields: 'item' }),
    ])
    items.value = itemsRes.items
    const counts: Record<string, number> = {}
    for (const i of instancesRes) counts[i.item] = (counts[i.item] ?? 0) + 1
    instanceCounts.value = counts
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

onMounted(load)

const filtered = computed(() => {
  const q = search.value.trim().toLowerCase()
  return items.value.filter((i) => {
    if (typeFilter.value !== 'all' && i.type !== typeFilter.value) return false
    if (!q) return true
    return (
      i.code.toLowerCase().includes(q) ||
      i.name.toLowerCase().includes(q) ||
      i.category.toLowerCase().includes(q) ||
      i.serial.toLowerCase().includes(q)
    )
  })
})

function openNew() {
  editing.value = {}
}

function openEdit(item: ItemRecord) {
  editing.value = { ...item }
}

async function onSave(data: Partial<ItemRecord>) {
  error.value = null
  const isEdit = !!data.id
  try {
    if (isEdit) {
      await pb.collection('items').update<ItemRecord>(data.id!, data)
    } else {
      await pb.collection('items').create<ItemRecord>(data)
    }
    editing.value = null
    await load()
    toast.success(isEdit ? `Saved ${data.code ?? 'item'}` : `Created ${data.code ?? 'item'}`)
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
    await load()
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
        <p class="text-sm text-slate-400">{{ items.length }} total</p>
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
        v-model="search"
        type="search"
        placeholder="Search code, name, category, serial…"
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

    <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
      <table class="w-full text-left text-sm">
        <thead class="bg-slate-950/70 text-slate-400">
          <tr>
            <th class="px-4 py-3 font-medium">Code</th>
            <th class="px-4 py-3 font-medium">Name</th>
            <th class="px-4 py-3 font-medium">Type</th>
            <th class="px-4 py-3 font-medium">Tracking</th>
            <th class="px-4 py-3 font-medium">Serial</th>
            <th class="px-4 py-3 font-medium">Category</th>
            <th class="px-4 py-3 font-medium">Active</th>
            <th class="px-4 py-3"></th>
          </tr>
        </thead>
        <tbody class="divide-y divide-slate-800">
          <tr v-if="loading">
            <td colspan="8" class="text-center text-slate-500 py-8">Loading…</td>
          </tr>
          <tr v-else-if="filtered.length === 0">
            <td colspan="8" class="text-center text-slate-500 py-8">
              {{ items.length === 0 ? 'No items yet. Click "New item" to add one.' : 'No items match your filter.' }}
            </td>
          </tr>
          <tr
            v-for="item in filtered"
            :key="item.id"
            class="hover:bg-slate-800/50 cursor-pointer"
            @click="openEdit(item)"
          >
            <td class="px-4 py-3 font-mono text-slate-200">{{ item.code }}</td>
            <td class="px-4 py-3">{{ item.name }}</td>
            <td class="px-4 py-3">
              <span
                class="inline-block px-2 py-0.5 rounded text-xs"
                :class="item.type === 'tool' ? 'bg-amber-900/60 text-amber-200' : 'bg-sky-900/60 text-sky-200'"
              >
                {{ item.type }}
              </span>
            </td>
            <td class="px-4 py-3 text-slate-400">
              {{ item.tracking_mode }}
              <span
                v-if="item.tracking_mode === 'serialized'"
                class="ml-1 inline-block px-1.5 rounded text-[10px] bg-slate-800 text-slate-300"
                :title="`${instanceCounts[item.id] ?? 0} instance(s)`"
              >{{ instanceCounts[item.id] ?? 0 }} inst</span>
            </td>
            <td class="px-4 py-3 font-mono text-slate-400">{{ item.serial || '—' }}</td>
            <td class="px-4 py-3 text-slate-400">{{ item.category || '—' }}</td>
            <td class="px-4 py-3">
              <span v-if="item.active" class="text-emerald-400">●</span>
              <span v-else class="text-slate-600">●</span>
            </td>
            <td class="px-4 py-3 text-right">
              <button
                type="button"
                class="text-red-400 hover:text-red-300 px-2 py-1"
                @click.stop="deleting = item"
              >
                Delete
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <ItemDialog
      :open="editing !== null"
      :item="editing"
      @update:open="(v) => { if (!v) { editing = null; void load() } }"
      @save="onSave"
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
