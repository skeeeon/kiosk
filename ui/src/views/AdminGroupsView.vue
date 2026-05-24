<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { pb } from '../lib/pb'
import GroupDialog from '../components/GroupDialog.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import DataTable, { type ColumnDef } from '../components/DataTable.vue'
import { useAdminToast } from '../composables/useAdminToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import { useListShortcuts } from '../composables/useListShortcuts'
import { useUrlQuerySync } from '../composables/useUrlQuerySync'
import type { GroupRecord } from '../types'

const toast = useAdminToast()
const { identity } = useKioskIdentity()
const managed = computed(() => identity.value?.managed ?? false)

const groups = ref<GroupRecord[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const search = ref('')

const editing = ref<Partial<GroupRecord> | null>(null)
const deleting = ref<GroupRecord | null>(null)
const searchInput = ref<HTMLInputElement | null>(null)

useUrlQuerySync({
  q: { ref: search, default: '' },
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
  if (!q) return groups.value
  return groups.value.filter(
    (g) =>
      g.code.toLowerCase().includes(q) ||
      g.name.toLowerCase().includes(q) ||
      (g.contact_email ?? '').toLowerCase().includes(q),
  )
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
  { key: 'code', label: 'Code' },
  { key: 'name', label: 'Name' },
  { key: 'contact_email', label: 'Contact email' },
  { key: 'contact_phone', label: 'Phone' },
  { key: 'active', label: 'Active' },
  { key: '__actions', align: 'right' },
]

const emptyText = computed(() =>
  groups.value.length === 0
    ? 'No groups yet. Click "New group" to add one.'
    : 'No groups match your filter.',
)

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
        <p class="text-sm text-slate-400">
          {{ groups.length }} total — sub-contractors / trades. Workers reference a group via FK; receipts to the
          group's contact email roll up activity per sub.
        </p>
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

    <input
      ref="searchInput"
      v-model="search"
      type="search"
      placeholder="Search code, name, contact email… (press / to focus)"
      class="w-full rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100 mb-4"
    />

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 mb-3">
      {{ error }}
    </p>

    <DataTable
      :columns="columns"
      :rows="filtered"
      :row-key="(g) => g.id"
      :loading="loading"
      :empty-text="emptyText"
      row-clickable
      @row-click="openEdit"
    >
      <template #cell-code="{ row }">
        <span class="font-mono text-slate-200">{{ row.code }}</span>
      </template>
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
      <template #cell-__actions="{ row }">
        <button
          v-if="!managed"
          type="button"
          class="px-3 py-1.5 rounded-md bg-red-950/60 hover:bg-red-900/60 text-red-200 text-sm border border-red-800/70 whitespace-nowrap"
          @click.stop="deleting = row"
        >
          Delete
        </button>
      </template>
    </DataTable>

    <GroupDialog
      :open="editing !== null"
      :group="editing"
      :managed="managed"
      @update:open="(v) => { if (!v) editing = null }"
      @save="onSave"
      @save-and-add-another="onSaveAndAdd"
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
