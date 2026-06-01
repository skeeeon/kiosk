<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import GroupDialog from '../components/GroupDialog.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import DataTable, { type ColumnDef } from '../components/DataTable.vue'
import { useToast } from '../composables/useToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import { useListShortcuts } from '../composables/useListShortcuts'
import { useUrlQuerySync } from '../composables/useUrlQuerySync'
import type { GroupRecord } from '../types'

const toast = useToast()
const { identity } = useKioskIdentity()
const managed = computed(() => identity.value?.managed ?? false)

const groups = ref<GroupRecord[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const search = ref('')
const showInactive = ref(false)
const page = ref(1)
const perPage = ref(25)

const editing = ref<Partial<GroupRecord> | null>(null)
const deleting = ref<GroupRecord | null>(null)
const searchInput = ref<HTMLInputElement | null>(null)

useUrlQuerySync({
  page: { ref: page, default: 1, parse: (v) => Number(v) || 1 },
  q: { ref: search, default: '' },
  inactive: { ref: showInactive, default: false, parse: (v) => v === 'true' },
})

async function load() {
  loading.value = true
  error.value = null
  try {
    groups.value = await pb.collection('groups').getFullList<GroupRecord>({
      sort: '+code',
    })
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

onMounted(load)

const filtered = computed(() => {
  const q = search.value.trim().toLowerCase()
  return groups.value.filter((g) => {
    if (!showInactive.value && !g.active) return false
    if (!q) return true
    return (
      g.code.toLowerCase().includes(q) ||
      g.name.toLowerCase().includes(q) ||
      (g.contact_email ?? '').toLowerCase().includes(q)
    )
  })
})

watch([search, showInactive], () => { page.value = 1 })

const pagedRows = computed(() => {
  const start = (page.value - 1) * perPage.value
  return filtered.value.slice(start, start + perPage.value)
})

function openNew() {
  editing.value = {}
}

useListShortcuts({
  searchInput,
  onNew: openNew,
  canCreate: computed(() => !managed.value),
})

function openEdit(group: GroupRecord) {
  editing.value = { ...group }
}

const columns: ColumnDef[] = [
  { key: 'name', label: 'Name' },
  { key: 'contact_email', label: 'Contact email' },
  { key: 'contact_phone', label: 'Phone' },
  { key: 'active', label: 'Active' },
]

const emptyText = computed(() => {
  if (groups.value.length === 0) return 'No groups yet. Click "New group" to add one.'
  if (search.value.trim() !== '') return 'No groups match your filter.'
  if (!showInactive.value) {
    return 'No active groups. Click "New group" to add one, or check "Show inactive" to see retired groups.'
  }
  return 'No groups match your filter.'
})

async function persistSave(data: Partial<GroupRecord>): Promise<boolean> {
  const isEdit = !!data.id
  if (isEdit) {
    await pb.collection('groups').update<GroupRecord>(data.id!, data)
  } else {
    await pb.collection('groups').create<GroupRecord>(data as Record<string, unknown>)
  }
  return isEdit
}

async function onSave(data: Partial<GroupRecord>) {
  error.value = null
  try {
    const wasEdit = await persistSave(data)
    editing.value = null
    await load()
    toast.success(wasEdit ? `Saved ${data.code ?? 'group'}` : `Created ${data.code ?? 'group'}`)
  } catch (e) {
    const msg = (e as Error).message
    error.value = msg
    toast.error(msg)
  }
}

async function onSaveAndAdd(data: Partial<GroupRecord>) {
  error.value = null
  try {
    await persistSave(data)
    editing.value = {}
    await load()
    toast.success(`Created ${data.code ?? 'group'} — ready for next`)
  } catch (e) {
    const msg = (e as Error).message
    error.value = msg
    toast.error(msg)
  }
}

// Delete now lives in the edit sheet. Hand off to the existing confirm flow:
// close the sheet, then open the ConfirmDialog.
function requestDelete() {
  const target = editing.value
  if (!target?.id) return
  deleting.value = target as GroupRecord
  editing.value = null
}

async function onDelete() {
  if (!deleting.value) return
  error.value = null
  const target = deleting.value
  try {
    await pb.collection('groups').delete(target.id)
    deleting.value = null
    await load()
    toast.success(`Deleted ${target.code}`)
  } catch (e) {
    const msg = (e as Error).message
    error.value = msg
    toast.error(msg)
  }
}
</script>

<template>
  <main class="p-6 max-w-7xl mx-auto w-full">
    <header class="flex items-baseline justify-between mb-4">
      <div>
        <h1 class="text-2xl font-semibold">Groups</h1>
        <p class="text-sm text-slate-400">{{ groups.length }} total</p>
      </div>
      <button
        v-if="!managed"
        type="button"
        class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        @click="openNew"
      >
        New group
      </button>
    </header>

    <div class="flex flex-col sm:flex-row sm:items-center gap-3 mb-4">
      <input
        ref="searchInput"
        v-model="search"
        type="search"
        placeholder="Search code, name, contact email… (press / to focus)"
        class="flex-1 rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100"
      />
      <label class="flex items-center gap-2 text-sm text-slate-300 whitespace-nowrap">
        <input v-model="showInactive" type="checkbox" class="w-4 h-4" />
        Show inactive
      </label>
    </div>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 mb-3">
      {{ error }}
    </p>

    <DataTable
      :columns="columns"
      :rows="pagedRows"
      :row-key="(g) => g.id"
      :loading="loading"
      :empty-text="emptyText"
      row-clickable
      :page="page"
      :per-page="perPage"
      :total="filtered.length"
      @row-click="openEdit"
      @update:page="(p) => page = p"
      @update:per-page="(n) => { perPage = n; page = 1 }"
    >
      <template #cell-contact_email="{ row }">
        <span class="text-slate-400">{{ row.contact_email || '—' }}</span>
      </template>
      <template #cell-contact_phone="{ row }">
        <span class="text-slate-400">{{ row.contact_phone || '—' }}</span>
      </template>
      <template #cell-active="{ row }">
        <span v-if="row.active" class="text-emerald-400">●</span>
        <span v-else class="text-slate-600">●</span>
      </template>
    </DataTable>

    <GroupDialog
      :open="editing !== null"
      :group="editing"
      :managed="managed"
      @update:open="(v) => { if (!v) editing = null }"
      @save="onSave"
      @save-and-add-another="onSaveAndAdd"
      @delete="requestDelete"
    />

    <ConfirmDialog
      :open="deleting !== null"
      title="Delete group"
      :message="deleting ? `Delete ${deleting.code} — ${deleting.name}? Workers assigned to it will become ungrouped. Past transactions stay intact.` : ''"
      confirm-label="Delete"
      destructive
      @update:open="(v) => { if (!v) deleting = null }"
      @confirm="onDelete"
    />
  </main>
</template>
