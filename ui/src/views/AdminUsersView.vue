<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import UserDialog from '../components/UserDialog.vue'
import GroupDialog from '../components/GroupDialog.vue'
import WorkerHistoryDialog from '../components/WorkerHistoryDialog.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import DataTable, { type ColumnDef } from '../components/DataTable.vue'
import { useAdminToast } from '../composables/useAdminToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import { useListShortcuts } from '../composables/useListShortcuts'
import { useUrlQuerySync } from '../composables/useUrlQuerySync'
import type { GroupRecord, WorkerRecord } from '../types'

const toast = useAdminToast()
const { identity } = useKioskIdentity()
const managed = computed(() => identity.value?.managed ?? false)

const users = ref<WorkerRecord[]>([])
const groups = ref<GroupRecord[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const search = ref('')
const page = ref(1)
const perPage = ref(50)
const total = ref(0)

const editing = ref<Partial<WorkerRecord> | null>(null)
const deleting = ref<WorkerRecord | null>(null)
const creatingGroup = ref<Partial<GroupRecord> | null>(null)
const viewingHistory = ref<WorkerRecord | null>(null)
const searchInput = ref<HTMLInputElement | null>(null)

useUrlQuerySync({
  page: { ref: page, default: 1, parse: (v) => Number(v) || 1 },
  q: { ref: search, default: '' },
})

// PB filter literal escaping: backslash first, then double-quotes.
function pbEscape(s: string): string {
  return s.replace(/\\/g, '\\\\').replace(/"/g, '\\"')
}

function buildFilter(): string {
  const q = search.value.trim()
  if (!q) return ''
  const safe = pbEscape(q)
  return `(code ~ "${safe}" || name ~ "${safe}" || email ~ "${safe}" || group.code ~ "${safe}")`
}

async function loadGroups() {
  try {
    groups.value = await pb.collection('groups').getFullList<GroupRecord>({ sort: '+code' })
  } catch (e) {
    error.value = (e as Error).message
  }
}

async function load() {
  loading.value = true
  error.value = null
  try {
    const filter = buildFilter()
    const res = await pb.collection('users').getList<WorkerRecord>(page.value, perPage.value, {
      filter,
      sort: '+code',
    })
    users.value = res.items
    total.value = res.totalItems
    page.value = res.page
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  await Promise.all([loadGroups(), load()])
})

// Debounce search by 250ms so typing doesn't fire a request per keystroke.
let searchTimer: ReturnType<typeof setTimeout> | null = null
watch(search, () => {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    page.value = 1
    void load()
  }, 250)
})
onUnmounted(() => {
  if (searchTimer) clearTimeout(searchTimer)
})

const groupByID = computed(() => {
  const m = new Map<string, GroupRecord>()
  for (const g of groups.value) m.set(g.id, g)
  return m
})

function groupLabel(id: string | undefined): string {
  if (!id) return ''
  return groupByID.value.get(id)?.code ?? ''
}

const columns: ColumnDef[] = [
  { key: 'code', label: 'Code' },
  { key: 'name', label: 'Name' },
  { key: 'email', label: 'Email' },
  { key: 'role', label: 'Role' },
  { key: 'group', label: 'Group' },
  { key: 'active', label: 'Active' },
  { key: '__actions', align: 'right' },
]

const emptyText = computed(() =>
  search.value.trim() === ''
    ? 'No workers yet. Click "New worker" to add one.'
    : 'No workers match your filter.',
)

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

useListShortcuts({
  searchInput,
  onNew: openNew,
  canCreate: computed(() => !managed.value),
})

function openEdit(user: WorkerRecord) {
  editing.value = { ...user }
}

// Workers don't actually authenticate via PB in v1, but PB auth collections
// require a password. We generate a long random one and never expose it.
function randomPassword(): string {
  const bytes = new Uint8Array(24)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
}

function isFKConstraintError(msg: string): boolean {
  const m = msg.toLowerCase()
  return m.includes('foreign key') || m.includes('constraint') || m.includes('referenced')
}

async function persistSave(data: Partial<WorkerRecord>): Promise<boolean> {
  const isEdit = !!data.id
  if (isEdit) {
    await pb.collection('users').update<WorkerRecord>(data.id!, data)
  } else {
    const pw = randomPassword()
    await pb.collection('users').create<WorkerRecord>({
      ...data,
      password: pw,
      passwordConfirm: pw,
    } as Record<string, unknown>)
  }
  return isEdit
}

async function onSave(data: Partial<WorkerRecord>) {
  error.value = null
  try {
    const wasEdit = await persistSave(data)
    editing.value = null
    await load()
    toast.success(wasEdit ? `Saved ${data.code ?? 'worker'}` : `Created ${data.code ?? 'worker'}`)
  } catch (e) {
    const msg = (e as Error).message
    error.value = msg
    toast.error(msg)
  }
}

async function onSaveAndAdd(data: Partial<WorkerRecord>) {
  error.value = null
  try {
    await persistSave(data)
    editing.value = {}
    await load()
    toast.success(`Created ${data.code ?? 'worker'} — ready for next`)
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
    await pb.collection('users').delete(target.id)
    deleting.value = null
    await load()
    toast.success(`Deleted ${target.code}`)
  } catch (e) {
    const raw = (e as Error).message
    const friendly = isFKConstraintError(raw)
      ? `${target.code} has transaction history and can't be deleted. Uncheck "Active" instead to retire them.`
      : raw
    error.value = friendly
    toast.error(friendly)
  }
}

async function onCreateGroupFromUser(data: Partial<GroupRecord>) {
  try {
    const created = await pb.collection('groups').create<GroupRecord>(data as Record<string, unknown>)
    groups.value = [...groups.value, created].sort((a, b) => a.code.localeCompare(b.code))
    if (editing.value) {
      editing.value = { ...editing.value, group: created.id }
    }
    creatingGroup.value = null
    toast.success(`Created group ${created.code}`)
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
        <h1 class="text-2xl font-semibold">Workers</h1>
        <p class="text-sm text-slate-400">{{ total }} total</p>
      </div>
      <button
        v-if="!managed"
        type="button"
        class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        @click="openNew"
      >
        New worker
      </button>
    </header>

    <input
      ref="searchInput"
      v-model="search"
      type="search"
      placeholder="Search code, name, email, group… (press / to focus)"
      class="w-full rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100 mb-4"
    />

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 mb-3">
      {{ error }}
    </p>

    <DataTable
      :columns="columns"
      :rows="users"
      :row-key="(u) => u.id"
      :loading="loading"
      :empty-text="emptyText"
      row-clickable
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
      <template #cell-email="{ row }">
        <span class="text-slate-400">{{ row.email }}</span>
      </template>
      <template #cell-role="{ row }">
        <span class="text-slate-400">{{ row.role }}</span>
      </template>
      <template #cell-group="{ row }">
        <span class="text-slate-400">{{ groupLabel(row.group) || '—' }}</span>
      </template>
      <template #cell-active="{ row }">
        <span v-if="row.active" class="text-emerald-400">●</span>
        <span v-else class="text-slate-600">●</span>
      </template>
      <template #cell-__actions="{ row }">
        <span class="inline-flex items-center gap-2 whitespace-nowrap">
          <button
            type="button"
            class="px-3 py-1.5 rounded-md bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm border border-slate-700"
            @click.stop="viewingHistory = row"
          >
            History
          </button>
          <button
            v-if="!managed"
            type="button"
            class="px-3 py-1.5 rounded-md bg-red-950/60 hover:bg-red-900/60 text-red-200 text-sm border border-red-800/70"
            @click.stop="deleting = row"
          >
            Delete
          </button>
        </span>
      </template>
    </DataTable>

    <UserDialog
      :open="editing !== null"
      :user="editing"
      :managed="managed"
      :groups="groups"
      @update:open="(v) => { if (!v) editing = null }"
      @save="onSave"
      @save-and-add-another="onSaveAndAdd"
      @create-group="creatingGroup = {}"
    />

    <GroupDialog
      :open="creatingGroup !== null"
      :group="creatingGroup"
      :managed="managed"
      @update:open="(v) => { if (!v) creatingGroup = null }"
      @save="onCreateGroupFromUser"
    />

    <WorkerHistoryDialog
      :open="viewingHistory !== null"
      :worker="viewingHistory"
      @update:open="(v) => { if (!v) viewingHistory = null }"
    />

    <ConfirmDialog
      :open="deleting !== null"
      title="Delete worker"
      :message="deleting ? `Delete ${deleting.code} — ${deleting.name}? Past transactions stay intact.` : ''"
      confirm-label="Delete"
      destructive
      @update:open="(v) => { if (!v) deleting = null }"
      @confirm="onDelete"
    />
  </main>
</template>
