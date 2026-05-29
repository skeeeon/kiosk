<!-- ItemInstancesPanel lists and edits per-unit instances of a serialized
     item. One instance row = one physical thing in the world. Embedded inside
     ItemDialog when the open item has tracking_mode='serialized' and already
     exists (we need an item id to FK against). -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import { useToast } from '../composables/useToast'
import type { ItemInstance } from '../types'

const props = defineProps<{ itemId: string }>()

const toast = useToast()

interface InstanceRow extends ItemInstance {
  out: boolean // true when at least one open_checkouts row references this instance
}

const rows = ref<InstanceRow[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

// Draft row for "new instance" inline form. Null when not adding.
const draft = ref<Partial<ItemInstance> | null>(null)
const editingId = ref<string | null>(null)
const editingDraft = ref<Partial<ItemInstance> | null>(null)

async function load() {
  loading.value = true
  error.value = null
  try {
    const [instRes, openRes] = await Promise.all([
      pb.collection('item_instances').getFullList<ItemInstance>({
        filter: pb.filter('item = {:item}', { item: props.itemId }),
        sort: '+code',
      }),
      pb.collection('open_checkouts').getFullList<{ item_instance: string }>({
        filter: pb.filter('item = {:item}', { item: props.itemId }),
        fields: 'item_instance',
      }),
    ])
    const outIds = new Set(openRes.map((o) => o.item_instance).filter(Boolean))
    rows.value = instRes.map((i) => ({ ...i, out: outIds.has(i.id) }))
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

watch(() => props.itemId, (id) => { if (id) void load() }, { immediate: true })

function startAdd() {
  draft.value = { code: '', serial: '', rfid_epc: '', active: true, notes: '' }
}

async function saveDraft() {
  if (!draft.value?.code) {
    toast.error('Instance code is required')
    return
  }
  try {
    await pb.collection('item_instances').create({
      ...draft.value,
      item: props.itemId,
    })
    draft.value = null
    await load()
    toast.success('Instance added')
  } catch (e) {
    toast.error((e as Error).message)
  }
}

function startEdit(row: InstanceRow) {
  editingId.value = row.id
  editingDraft.value = { ...row }
}

async function saveEdit() {
  if (!editingId.value || !editingDraft.value) return
  try {
    await pb.collection('item_instances').update(editingId.value, editingDraft.value)
    editingId.value = null
    editingDraft.value = null
    await load()
    toast.success('Instance updated')
  } catch (e) {
    toast.error((e as Error).message)
  }
}

async function remove(row: InstanceRow) {
  if (row.out) {
    toast.error(`${row.code} is currently checked out — return it before deleting`)
    return
  }
  if (!confirm(`Delete instance ${row.code}? This can't be undone.`)) return
  try {
    await pb.collection('item_instances').delete(row.id)
    await load()
    toast.success(`Deleted ${row.code}`)
  } catch (e) {
    // FK constraint from transaction_lines (the ledger keeps the FK alive) —
    // surface the same suggestion as items: deactivate instead.
    const m = (e as Error).message.toLowerCase()
    if (m.includes('foreign key') || m.includes('constraint') || m.includes('referenced')) {
      toast.error(`${row.code} has transaction history — uncheck Active to retire instead.`)
    } else {
      toast.error((e as Error).message)
    }
  }
}

const hasRows = computed(() => rows.value.length > 0)
</script>

<template>
  <section class="rounded-xl bg-slate-950/40 border border-slate-800 p-4">
    <header class="flex items-center justify-between mb-3">
      <div>
        <h3 class="text-sm font-medium text-slate-200">Instances</h3>
        <p class="text-xs text-slate-500">One row per physical unit of this SKU.</p>
      </div>
      <button
        v-if="!draft"
        type="button"
        class="text-sm px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
        @click="startAdd"
      >
        + Add instance
      </button>
    </header>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 text-sm px-3 py-2 mb-2">
      {{ error }}
    </p>

    <table class="w-full text-left text-xs">
      <thead class="text-slate-500">
        <tr>
          <th class="px-2 py-2 font-medium">Code</th>
          <th class="px-2 py-2 font-medium">Serial</th>
          <th class="px-2 py-2 font-medium">RFID</th>
          <th class="px-2 py-2 font-medium">Active</th>
          <th class="px-2 py-2 font-medium">Status</th>
          <th class="px-2 py-2"></th>
        </tr>
      </thead>
      <tbody class="divide-y divide-slate-800">
        <tr v-if="loading">
          <td colspan="6" class="text-center text-slate-500 py-3">Loading…</td>
        </tr>
        <tr v-else-if="!hasRows && !draft">
          <td colspan="6" class="text-center text-slate-500 py-3">
            No instances yet. Add one to enable scanning.
          </td>
        </tr>

        <template v-for="row in rows" :key="row.id">
          <tr v-if="editingId !== row.id" class="hover:bg-slate-900/50">
            <td class="px-2 py-2 font-mono text-slate-200">{{ row.code }}</td>
            <td class="px-2 py-2 font-mono text-slate-400">{{ row.serial || '—' }}</td>
            <td class="px-2 py-2 font-mono text-slate-400 truncate max-w-[10rem]">{{ row.rfid_epc || '—' }}</td>
            <td class="px-2 py-2">
              <span v-if="row.active" class="text-emerald-400">●</span>
              <span v-else class="text-slate-600">●</span>
            </td>
            <td class="px-2 py-2">
              <span
                v-if="row.out"
                class="inline-block px-2 py-0.5 rounded text-[10px] bg-amber-900/60 text-amber-200"
              >currently out</span>
              <span v-else class="text-slate-500 text-[10px]">available</span>
            </td>
            <td class="px-2 py-2 text-right whitespace-nowrap">
              <button
                type="button"
                class="text-sky-400 hover:text-sky-300 px-1 mr-1"
                @click="startEdit(row)"
              >Edit</button>
              <button
                type="button"
                class="text-red-400 hover:text-red-300 px-1"
                @click="remove(row)"
              >Delete</button>
            </td>
          </tr>
          <tr v-else class="bg-slate-900/70">
            <td class="px-2 py-2">
              <input
                v-model="editingDraft!.code"
                type="text"
                class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm"
              />
            </td>
            <td class="px-2 py-2">
              <input v-model="editingDraft!.serial" type="text" class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm" />
            </td>
            <td class="px-2 py-2">
              <input v-model="editingDraft!.rfid_epc" type="text" class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm" />
            </td>
            <td class="px-2 py-2">
              <input v-model="editingDraft!.active" type="checkbox" class="w-4 h-4" />
            </td>
            <td class="px-2 py-2 text-slate-500 text-[10px]">—</td>
            <td class="px-2 py-2 text-right whitespace-nowrap">
              <button
                type="button"
                class="text-emerald-400 hover:text-emerald-300 px-1 mr-1"
                @click="saveEdit"
              >Save</button>
              <button
                type="button"
                class="text-slate-400 hover:text-slate-300 px-1"
                @click="editingId = null; editingDraft = null"
              >Cancel</button>
            </td>
          </tr>
        </template>

        <tr v-if="draft" class="bg-slate-900/70">
          <td class="px-2 py-2">
            <input
              v-model="draft.code"
              type="text"
              placeholder="DR-042-A"
              class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm"
            />
          </td>
          <td class="px-2 py-2">
            <input v-model="draft.serial" type="text" class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm" />
          </td>
          <td class="px-2 py-2">
            <input v-model="draft.rfid_epc" type="text" class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm" />
          </td>
          <td class="px-2 py-2">
            <input v-model="draft.active" type="checkbox" class="w-4 h-4" />
          </td>
          <td class="px-2 py-2 text-slate-500 text-[10px]">new</td>
          <td class="px-2 py-2 text-right whitespace-nowrap">
            <button
              type="button"
              class="text-emerald-400 hover:text-emerald-300 px-1 mr-1"
              @click="saveDraft"
            >Save</button>
            <button
              type="button"
              class="text-slate-400 hover:text-slate-300 px-1"
              @click="draft = null"
            >Cancel</button>
          </td>
        </tr>
      </tbody>
    </table>
  </section>
</template>
