<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { pb } from '../lib/pb'
import UserDialog from '../components/UserDialog.vue'
import GroupDialog from '../components/GroupDialog.vue'
import WorkerHistoryDialog from '../components/WorkerHistoryDialog.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import { useAdminToast } from '../composables/useAdminToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import type { GroupRecord, WorkerRecord } from '../types'

const toast = useAdminToast()
const { identity } = useKioskIdentity()
const managed = computed(() => identity.value?.managed ?? false)

const users = ref<WorkerRecord[]>([])
const groups = ref<GroupRecord[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const search = ref('')

const editing = ref<Partial<WorkerRecord> | null>(null)
const deleting = ref<WorkerRecord | null>(null)
const creatingGroup = ref<Partial<GroupRecord> | null>(null)
const viewingHistory = ref<WorkerRecord | null>(null)

async function load() {
  loading.value = true
  error.value = null
  try {
    // getFullList paginates internally so a roster larger than one page
    // doesn't silently truncate the list. Groups are fetched separately
    // (rather than via expand on each user) because the select needs the
    // full catalog regardless of which workers are loaded.
    const [u, g] = await Promise.all([
      pb.collection('users').getFullList<WorkerRecord>({ sort: '+code' }),
      pb.collection('groups').getFullList<GroupRecord>({ sort: '+code' }),
    ])
    users.value = u
    groups.value = g
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

onMounted(load)

const groupByID = computed(() => {
  const m = new Map<string, GroupRecord>()
  for (const g of groups.value) m.set(g.id, g)
  return m
})

function groupLabel(id: string | undefined): string {
  if (!id) return ''
  return groupByID.value.get(id)?.code ?? ''
}

const filtered = computed(() => {
  const q = search.value.trim().toLowerCase()
  if (!q) return users.value
  return users.value.filter((u) => {
    const groupCode = groupLabel(u.group).toLowerCase()
    return (
      u.code.toLowerCase().includes(q) ||
      u.name.toLowerCase().includes(q) ||
      u.email.toLowerCase().includes(q) ||
      groupCode.includes(q)
    )
  })
})

function openNew() {
  editing.value = {}
}

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

async function onSave(data: Partial<WorkerRecord>) {
  error.value = null
  const isEdit = !!data.id
  try {
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
    editing.value = null
    await load()
    toast.success(isEdit ? `Saved ${data.code ?? 'worker'}` : `Created ${data.code ?? 'worker'}`)
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
        <p class="text-sm text-slate-400">{{ users.length }} total</p>
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
      v-model="search"
      type="search"
      placeholder="Search code, name, email, group…"
      class="w-full rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100 mb-4"
    />

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 mb-3">
      {{ error }}
    </p>

    <div class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
      <table class="w-full text-left text-sm">
        <thead class="bg-slate-950/70 text-slate-400">
          <tr>
            <th class="px-4 py-3 font-medium">Code</th>
            <th class="px-4 py-3 font-medium">Name</th>
            <th class="px-4 py-3 font-medium">Email</th>
            <th class="px-4 py-3 font-medium">Role</th>
            <th class="px-4 py-3 font-medium">Group</th>
            <th class="px-4 py-3 font-medium">Active</th>
            <th class="px-4 py-3"></th>
          </tr>
        </thead>
        <tbody class="divide-y divide-slate-800">
          <tr v-if="loading">
            <td colspan="7" class="text-center text-slate-500 py-8">Loading…</td>
          </tr>
          <tr v-else-if="filtered.length === 0">
            <td colspan="7" class="text-center text-slate-500 py-8">
              {{ users.length === 0 ? 'No workers yet. Click "New worker" to add one.' : 'No workers match your filter.' }}
            </td>
          </tr>
          <tr
            v-for="user in filtered"
            :key="user.id"
            class="hover:bg-slate-800/50 cursor-pointer"
            @click="openEdit(user)"
          >
            <td class="px-4 py-3 font-mono text-slate-200">{{ user.code }}</td>
            <td class="px-4 py-3">{{ user.name }}</td>
            <td class="px-4 py-3 text-slate-400">{{ user.email }}</td>
            <td class="px-4 py-3 text-slate-400">{{ user.role }}</td>
            <td class="px-4 py-3 text-slate-400">{{ groupLabel(user.group) || '—' }}</td>
            <td class="px-4 py-3">
              <span v-if="user.active" class="text-emerald-400">●</span>
              <span v-else class="text-slate-600">●</span>
            </td>
            <td class="px-4 py-3 text-right whitespace-nowrap">
              <button
                type="button"
                class="text-slate-400 hover:text-slate-200 px-2 py-1"
                @click.stop="viewingHistory = user"
              >
                History
              </button>
              <button
                v-if="!managed"
                type="button"
                class="text-red-400 hover:text-red-300 px-2 py-1 ml-2"
                @click.stop="deleting = user"
              >
                Delete
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <UserDialog
      :open="editing !== null"
      :user="editing"
      :managed="managed"
      :groups="groups"
      @update:open="(v) => { if (!v) editing = null }"
      @save="onSave"
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
