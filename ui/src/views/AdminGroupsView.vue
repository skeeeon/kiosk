<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { pb } from '../lib/pb'
import GroupDialog from '../components/GroupDialog.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import { useAdminToast } from '../composables/useAdminToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
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

function openEdit(group: GroupRecord) {
  editing.value = { ...group }
}

async function onSave(data: Partial<GroupRecord>) {
  error.value = null
  const isEdit = !!data.id
  try {
    if (isEdit) {
      await pb.collection('groups').update<GroupRecord>(data.id!, data)
    } else {
      await pb.collection('groups').create<GroupRecord>(data as Record<string, unknown>)
    }
    editing.value = null
    await load()
    toast.success(isEdit ? `Saved ${data.code ?? 'group'}` : `Created ${data.code ?? 'group'}`)
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
      v-model="search"
      type="search"
      placeholder="Search code, name, contact email…"
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
            <th class="px-4 py-3 font-medium">Contact email</th>
            <th class="px-4 py-3 font-medium">Phone</th>
            <th class="px-4 py-3 font-medium">Active</th>
            <th class="px-4 py-3"></th>
          </tr>
        </thead>
        <tbody class="divide-y divide-slate-800">
          <tr v-if="loading">
            <td colspan="6" class="text-center text-slate-500 py-8">Loading…</td>
          </tr>
          <tr v-else-if="filtered.length === 0">
            <td colspan="6" class="text-center text-slate-500 py-8">
              {{ groups.length === 0 ? 'No groups yet. Click "New group" to add one.' : 'No groups match your filter.' }}
            </td>
          </tr>
          <tr
            v-for="group in filtered"
            :key="group.id"
            class="hover:bg-slate-800/50 cursor-pointer"
            @click="openEdit(group)"
          >
            <td class="px-4 py-3 font-mono text-slate-200">{{ group.code }}</td>
            <td class="px-4 py-3">{{ group.name }}</td>
            <td class="px-4 py-3 text-slate-400">{{ group.contact_email || '—' }}</td>
            <td class="px-4 py-3 text-slate-400">{{ group.contact_phone || '—' }}</td>
            <td class="px-4 py-3">
              <span v-if="group.active" class="text-emerald-400">●</span>
              <span v-else class="text-slate-600">●</span>
            </td>
            <td class="px-4 py-3 text-right">
              <button
                v-if="!managed"
                type="button"
                class="text-red-400 hover:text-red-300 px-2 py-1"
                @click.stop="deleting = group"
              >
                Delete
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <GroupDialog
      :open="editing !== null"
      :group="editing"
      :managed="managed"
      @update:open="(v) => { if (!v) editing = null }"
      @save="onSave"
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
