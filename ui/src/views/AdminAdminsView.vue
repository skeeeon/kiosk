<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { pb } from '../lib/pb'
import AdminDialog from '../components/AdminDialog.vue'
import AppDialog from '../components/AppDialog.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import DataTable, { type ColumnDef } from '../components/DataTable.vue'
import { useAdminToast } from '../composables/useAdminToast'
import { useAuthStore } from '../stores/auth'
import { useListShortcuts } from '../composables/useListShortcuts'
import { useUrlQuerySync } from '../composables/useUrlQuerySync'
import type { AdminRecord } from '../types'

const toast = useAdminToast()
const auth = useAuthStore()

const admins = ref<AdminRecord[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const search = ref('')

const editing = ref<Partial<AdminRecord> | null>(null)
const deleting = ref<AdminRecord | null>(null)
const searchInput = ref<HTMLInputElement | null>(null)

useUrlQuerySync({
  q: { ref: search, default: '' },
})

// One-shot password display after a successful create. Held outside the
// dialog so even if AdminDialog has already closed and re-opened we keep
// the credential visible until the operator dismisses.
const passwordPrompt = ref<{ email: string; password: string } | null>(null)
const copied = ref(false)

async function load() {
  loading.value = true
  error.value = null
  try {
    admins.value = await pb.collection('admins').getFullList<AdminRecord>({ sort: '+email' })
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

onMounted(load)

const filtered = computed(() => {
  const q = search.value.trim().toLowerCase()
  if (!q) return admins.value
  return admins.value.filter(
    (a) => a.email.toLowerCase().includes(q) || a.name.toLowerCase().includes(q),
  )
})

const currentAdminId = computed(() => auth.admin?.id ?? '')

function isSelf(a: { id?: string } | null | undefined): boolean {
  return !!a?.id && a.id === currentAdminId.value
}

const columns: ColumnDef[] = [
  { key: 'email', label: 'Email' },
  { key: 'name', label: 'Name' },
  { key: 'active', label: 'Active' },
  { key: '__actions', align: 'right' },
]

function openNew() {
  editing.value = {}
}

useListShortcuts({ searchInput, onNew: openNew })

function openEdit(a: AdminRecord) {
  editing.value = { ...a }
}

// PB auth collections require a password. We generate ~24 bytes of entropy
// (192 bits) and present it once on create. Edit flow omits the password
// entirely — admins use the password-reset link from the login screen.
function randomPassword(): string {
  const bytes = new Uint8Array(24)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
}

function isFKConstraintError(msg: string): boolean {
  const m = msg.toLowerCase()
  return m.includes('foreign key') || m.includes('constraint') || m.includes('referenced')
}

// Returns the generated password on create, null on edit. The caller decides
// whether to surface the password prompt and how to advance the dialog.
async function persistSave(data: Partial<AdminRecord>): Promise<string | null> {
  const isEdit = !!data.id
  if (isEdit) {
    await pb.collection('admins').update<AdminRecord>(data.id!, {
      email: data.email,
      name: data.name,
      active: data.active,
    })
    return null
  }
  const pw = randomPassword()
  await pb.collection('admins').create<AdminRecord>({
    email: data.email,
    name: data.name,
    active: data.active ?? true,
    password: pw,
    passwordConfirm: pw,
  } as Record<string, unknown>)
  return pw
}

async function onSave(data: Partial<AdminRecord>) {
  error.value = null
  try {
    const pw = await persistSave(data)
    editing.value = null
    await load()
    if (pw) {
      passwordPrompt.value = { email: data.email ?? '', password: pw }
      copied.value = false
      toast.success(`Created ${data.email}`)
    } else {
      toast.success(`Saved ${data.email}`)
    }
  } catch (e) {
    const msg = (e as Error).message
    error.value = msg
    toast.error(msg)
  }
}

async function onSaveAndAdd(data: Partial<AdminRecord>) {
  error.value = null
  try {
    const pw = await persistSave(data)
    editing.value = {}
    await load()
    if (pw) {
      passwordPrompt.value = { email: data.email ?? '', password: pw }
      copied.value = false
    }
    toast.success(`Created ${data.email} — ready for next`)
  } catch (e) {
    const msg = (e as Error).message
    error.value = msg
    toast.error(msg)
  }
}

async function copyPassword() {
  if (!passwordPrompt.value) return
  try {
    await navigator.clipboard.writeText(passwordPrompt.value.password)
    copied.value = true
  } catch {
    copied.value = false
  }
}

async function onDelete() {
  if (!deleting.value) return
  error.value = null
  const target = deleting.value
  try {
    await pb.collection('admins').delete(target.id)
    deleting.value = null
    await load()
    toast.success(`Deleted ${target.email}`)
  } catch (e) {
    const raw = (e as Error).message
    const friendly = isFKConstraintError(raw)
      ? `${target.email} has audit history (stock adjustments or template edits) and can't be deleted. Uncheck "Active" instead to retire them.`
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
        <h1 class="text-2xl font-semibold">Admins</h1>
        <p class="text-sm text-slate-400">{{ admins.length }} total</p>
      </div>
      <button
        type="button"
        class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        @click="openNew"
      >
        New admin
      </button>
    </header>

    <input
      ref="searchInput"
      v-model="search"
      type="search"
      placeholder="Search email, name… (press / to focus)"
      class="w-full rounded-lg bg-slate-900 border border-slate-800 px-3 py-2 text-slate-100 mb-4"
    />

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 mb-3">
      {{ error }}
    </p>

    <DataTable
      :columns="columns"
      :rows="filtered"
      :row-key="(a) => a.id"
      :loading="loading"
      :empty-text="admins.length === 0 ? 'No admins yet.' : 'No admins match your filter.'"
      row-clickable
      @row-click="openEdit"
    >
      <template #cell-email="{ row }">
        <span class="font-mono text-slate-200">{{ row.email }}</span>
        <span v-if="isSelf(row)" class="ml-2 text-xs text-slate-500">(you)</span>
      </template>
      <template #cell-name="{ row }">
        <span class="text-slate-300">{{ row.name }}</span>
      </template>
      <template #cell-active="{ row }">
        <span v-if="row.active" class="text-emerald-400">●</span>
        <span v-else class="text-slate-600">●</span>
      </template>
      <template #cell-__actions="{ row }">
        <button
          v-if="!isSelf(row)"
          type="button"
          class="px-3 py-1.5 rounded-md bg-red-950/60 hover:bg-red-900/60 text-red-200 text-sm border border-red-800/70 whitespace-nowrap"
          @click.stop="deleting = row"
        >
          Delete
        </button>
      </template>
    </DataTable>

    <AdminDialog
      :open="editing !== null"
      :admin="editing"
      :is-self="isSelf(editing)"
      @update:open="(v) => { if (!v) editing = null }"
      @save="onSave"
      @save-and-add-another="onSaveAndAdd"
    />

    <AppDialog
      :open="passwordPrompt !== null"
      title="Save this password"
      description="It will not be shown again. The admin can change it later via the &quot;Forgot password&quot; link on the login screen."
      size="sm"
      @update:open="(v) => { if (!v) { passwordPrompt = null; copied = false } }"
    >
      <div v-if="passwordPrompt" class="flex flex-col gap-3">
        <div>
          <label class="text-sm text-slate-400">Email</label>
          <p class="font-mono text-slate-100 mt-1">{{ passwordPrompt.email }}</p>
        </div>
        <div>
          <label class="text-sm text-slate-400">Initial password</label>
          <p
            class="font-mono text-slate-100 mt-1 break-all rounded-lg bg-slate-950 border border-slate-800 px-3 py-2 text-sm"
          >
            {{ passwordPrompt.password }}
          </p>
        </div>
        <div class="flex justify-end gap-3 mt-2">
          <button
            type="button"
            class="px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
            @click="copyPassword"
          >
            {{ copied ? 'Copied' : 'Copy password' }}
          </button>
          <button
            type="button"
            class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
            @click="passwordPrompt = null"
          >
            Done
          </button>
        </div>
      </div>
    </AppDialog>

    <ConfirmDialog
      :open="deleting !== null"
      title="Delete admin"
      :message="deleting ? `Delete ${deleting.email}? If they have audit history, deactivate instead — delete will fail with a constraint error.` : ''"
      confirm-label="Delete"
      destructive
      @update:open="(v) => { if (!v) deleting = null }"
      @confirm="onDelete"
    />
  </main>
</template>
