<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import { download } from '../lib/api'
import { useKioskIdentity } from '../composables/useKioskIdentity'

const { identity } = useKioskIdentity()
const managed = computed(() => identity.value?.managed ?? false)
const isController = computed(() => identity.value?.role === 'controller')

interface ImportError {
  row: number
  code: string
  message: string
}

interface ImportResult {
  dry_run: boolean
  rows_total: number
  rows_inserted: number
  rows_updated: number
  errors: ImportError[]
}

type Kind = 'items' | 'users' | 'groups'

// All three kinds are available on both binaries. Managed kiosks see the
// read-only banner below instead of any tab because their catalog/workers/
// groups are owned by the controller; the requireAdmin gate is the trust
// boundary either way.
const availableKinds: Kind[] = ['items', 'users', 'groups']
const kind = ref<Kind>('items')

const KIND_META: Record<Kind, { label: string; columns: string; required: string }> = {
  items: {
    label: 'Items',
    columns: 'code, name, type, unit, tracking_mode, category, active, notes, quantity_on_hand, reorder_threshold',
    required: 'code, name, type (tool|consumable); tracking_mode defaults to quantity',
  },
  users: {
    label: 'Workers',
    columns: 'code, name, email, role, group, active',
    required: 'code, name; role defaults to worker, group auto-creates if missing',
  },
  groups: {
    label: 'Groups',
    columns: 'code, name, contact_email, contact_phone, notes, active',
    required: 'code, name',
  },
}

const file = ref<File | null>(null)
const dryRun = ref(true) // safe default — operators see validation before writes
const submitting = ref(false)
const result = ref<ImportResult | null>(null)
const error = ref<string | null>(null)

// Reset state when switching tabs so a stale "Import complete" panel from
// the previous kind doesn't mislead the operator.
watch(kind, () => {
  file.value = null
  result.value = null
  error.value = null
})

function onFileChange(e: Event) {
  const input = e.target as HTMLInputElement
  file.value = input.files?.[0] ?? null
  result.value = null
  error.value = null
}

async function onSubmit() {
  if (!file.value || submitting.value) return
  submitting.value = true
  error.value = null
  result.value = null
  try {
    const formData = new FormData()
    formData.append('file', file.value)
    formData.append('dry_run', dryRun.value ? 'true' : 'false')

    result.value = await pb.send<ImportResult>(`/api/kiosk/${kind.value}/import`, {
      method: 'POST',
      body: formData,
    })
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    submitting.value = false
  }
}

async function onDownloadTemplate() {
  try {
    await download(
      `/api/kiosk/${kind.value}/import/template`,
      `${kind.value}-template.csv`,
    )
  } catch (e) {
    error.value = (e as Error).message
  }
}
</script>

<template>
  <main class="p-6 max-w-3xl mx-auto w-full">
    <h1 class="text-2xl font-semibold mb-2">Import</h1>

    <div
      v-if="managed && !isController"
      class="rounded-lg bg-sky-950/60 border border-sky-800 text-sky-200 px-4 py-3 mb-4"
    >
      Catalog is managed by the controller — import there instead. This
      kiosk receives item updates over JetStream as the central catalog changes.
    </div>

    <template v-if="!(managed && !isController)">
      <div class="flex gap-1 border-b border-slate-800 mb-4">
        <button
          v-for="k in availableKinds"
          :key="k"
          type="button"
          :class="[
            'px-4 py-2 -mb-px border-b-2 text-sm font-medium transition-colors',
            kind === k
              ? 'border-brand-primary text-slate-100'
              : 'border-transparent text-slate-400 hover:text-slate-200',
          ]"
          @click="kind = k"
        >
          {{ KIND_META[k].label }}
        </button>
      </div>

      <p class="text-sm text-slate-400 mb-6">
        Upload a CSV with columns:
        <code class="font-mono text-slate-300">{{ KIND_META[kind].columns }}</code>.
        Required: {{ KIND_META[kind].required }}. Rows match existing records by
        <code class="font-mono text-slate-300">code</code> (upsert); records not in the CSV are left alone.
      </p>

      <form
        class="bg-slate-900 border border-slate-800 rounded-2xl p-6 flex flex-col gap-4"
        @submit.prevent="onSubmit"
      >
        <input
          type="file"
          accept=".csv,text/csv"
          class="block w-full text-slate-300 file:mr-4 file:py-2 file:px-4 file:rounded-lg file:border-0 file:bg-brand-primary file:hover:bg-brand-primary-hover file:text-white file:cursor-pointer"
          @change="onFileChange"
        />

        <label class="flex items-center gap-2 text-slate-300">
          <input v-model="dryRun" type="checkbox" class="w-4 h-4" />
          Dry run — validate only, don't write changes
        </label>

        <div class="flex justify-between items-center gap-3 flex-wrap">
          <button
            type="button"
            class="px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm font-medium"
            @click="onDownloadTemplate"
          >
            Download {{ KIND_META[kind].label.toLowerCase() }} template
          </button>
          <button
            type="submit"
            :disabled="!file || submitting"
            class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover disabled:bg-slate-700 text-white font-medium"
          >
            {{ submitting ? 'Processing…' : dryRun ? 'Validate' : 'Import' }}
          </button>
        </div>

        <p v-if="error" class="text-red-400">{{ error }}</p>
      </form>

      <section
        v-if="result"
        class="mt-6 bg-slate-900 border border-slate-800 rounded-2xl p-6"
      >
        <h2 class="text-xl font-semibold mb-3">
          {{ result.dry_run ? 'Validation results' : 'Import results' }}
        </h2>

        <ul class="grid grid-cols-2 sm:grid-cols-4 gap-3 mb-4">
          <li class="rounded-lg bg-slate-800 p-3 text-center">
            <p class="text-2xl font-semibold">{{ result.rows_total }}</p>
            <p class="text-xs text-slate-400 uppercase tracking-wide">Total rows</p>
          </li>
          <li v-if="!result.dry_run" class="rounded-lg bg-slate-800 p-3 text-center">
            <p class="text-2xl font-semibold text-emerald-400">{{ result.rows_inserted }}</p>
            <p class="text-xs text-slate-400 uppercase tracking-wide">Inserted</p>
          </li>
          <li v-if="!result.dry_run" class="rounded-lg bg-slate-800 p-3 text-center">
            <p class="text-2xl font-semibold text-sky-400">{{ result.rows_updated }}</p>
            <p class="text-xs text-slate-400 uppercase tracking-wide">Updated</p>
          </li>
          <li class="rounded-lg bg-slate-800 p-3 text-center">
            <p
              class="text-2xl font-semibold"
              :class="result.errors.length > 0 ? 'text-red-400' : 'text-slate-500'"
            >
              {{ result.errors.length }}
            </p>
            <p class="text-xs text-slate-400 uppercase tracking-wide">Errors</p>
          </li>
        </ul>

        <div
          v-if="result.errors.length === 0 && !result.dry_run"
          class="rounded-lg bg-emerald-900/40 border border-emerald-700/60 text-emerald-200 px-3 py-2"
        >
          Import complete — no errors.
        </div>
        <div
          v-else-if="result.errors.length === 0 && result.dry_run"
          class="rounded-lg bg-sky-900/40 border border-sky-700/60 text-sky-200 px-3 py-2"
        >
          Validation passed — uncheck "Dry run" and submit to write.
        </div>

        <div
          v-if="result.errors.length > 0"
          class="mt-3 rounded-lg overflow-hidden border border-slate-800 max-h-96 overflow-y-auto"
        >
          <table class="w-full text-sm">
            <thead class="bg-slate-800 text-slate-300 text-left sticky top-0">
              <tr>
                <th class="px-3 py-2 font-medium w-16">Row</th>
                <th class="px-3 py-2 font-medium w-48">Code</th>
                <th class="px-3 py-2 font-medium">Message</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-800">
              <tr
                v-for="(e, i) in result.errors"
                :key="i"
                class="bg-slate-900"
              >
                <td class="px-3 py-2 font-mono text-slate-300">{{ e.row }}</td>
                <td class="px-3 py-2 font-mono text-amber-400">{{ e.code }}</td>
                <td class="px-3 py-2 text-slate-300">{{ e.message }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>
    </template>
  </main>
</template>
