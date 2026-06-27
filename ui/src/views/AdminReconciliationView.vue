<!-- AdminReconciliationView surfaces custody-vs-location discrepancies
     (docs/location-sightings-plan.md, L4). Observability only — every row is a
     hint, never an action. Works on both binaries: the controller reads its
     fleet-wide view, a kiosk reads its own. Empty/over-quiet is the happy path. -->
<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api } from '../lib/api'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import type { Discrepancy, DiscrepancyKind, ReconciliationResult } from '../types'

const { identity } = useKioskIdentity()
const isController = computed(() => identity.value?.role === 'controller')
const endpoint = computed(() =>
  isController.value ? '/api/controller/reconciliation' : '/api/kiosk/reconciliation',
)

const result = ref<ReconciliationResult | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)

async function load() {
  loading.value = true
  error.value = null
  try {
    result.value = await api.get<ReconciliationResult>(endpoint.value)
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}
onMounted(load)

const KIND_META: Record<DiscrepancyKind, { label: string; hint: string; cls: string }> = {
  not_taken: {
    label: 'Likely not taken',
    hint: 'Checked out, but last seen still in a storage zone.',
    cls: 'bg-amber-900/50 text-amber-200 border-amber-700',
  },
  stale: {
    label: 'Possibly lost',
    hint: 'Checked out, but not seen anywhere past the staleness threshold.',
    cls: 'bg-orange-900/50 text-orange-200 border-orange-700',
  },
  unaccounted: {
    label: 'Unaccounted movement',
    hint: 'Seen outside a storage zone with no custody record.',
    cls: 'bg-red-900/50 text-red-200 border-red-700',
  },
}
const ORDER: DiscrepancyKind[] = ['unaccounted', 'not_taken', 'stale']

const groups = computed(() => {
  const all = result.value?.discrepancies ?? []
  return ORDER.map((kind) => ({
    kind,
    meta: KIND_META[kind],
    rows: all.filter((d) => d.kind === kind),
  })).filter((g) => g.rows.length > 0)
})

const total = computed(() => result.value?.discrepancies.length ?? 0)

function relativeAge(iso?: string): string {
  if (!iso) return '—'
  const t = new Date(iso).getTime()
  if (!Number.isFinite(t)) return '—'
  const diffMs = Date.now() - t
  if (diffMs < 60_000) return 'just now'
  const m = Math.floor(diffMs / 60_000)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  return `${Math.floor(h / 24)}d ago`
}

function rowsOf(kind: DiscrepancyKind): Discrepancy[] {
  return result.value?.discrepancies.filter((d) => d.kind === kind) ?? []
}
</script>

<template>
  <main class="p-4 sm:p-6 max-w-5xl mx-auto w-full">
    <header class="flex items-center justify-between mb-5">
      <div>
        <h1 class="text-2xl font-semibold">Reconciliation</h1>
        <p class="text-sm text-slate-400">
          Where custody and last-seen location disagree. Advisory only — a hint to
          investigate, never an automatic action.
        </p>
      </div>
      <button
        type="button"
        class="text-sm px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 disabled:opacity-50"
        :disabled="loading"
        @click="load"
      >
        {{ loading ? 'Loading…' : 'Refresh' }}
      </button>
    </header>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 text-sm px-3 py-2 mb-3">
      {{ error }}
    </p>

    <p v-else-if="loading" class="text-slate-500 text-sm">Loading…</p>

    <template v-else-if="result">
      <p v-if="total === 0" class="rounded-lg bg-emerald-950/40 border border-emerald-800 text-emerald-200 text-sm px-4 py-3">
        Nothing to reconcile — custody and location agree (or no location data has been
        reported yet).
      </p>

      <section v-for="g in groups" :key="g.kind" class="mb-6">
        <div class="flex items-center gap-2 mb-2">
          <span class="inline-block px-2 py-0.5 rounded text-xs border" :class="g.meta.cls">{{ g.meta.label }}</span>
          <span class="text-xs text-slate-500">{{ g.meta.hint }}</span>
          <span class="text-xs text-slate-600">· {{ g.rows.length }}</span>
        </div>
        <table class="w-full text-left text-sm">
          <thead class="text-slate-500 text-xs">
            <tr>
              <th class="px-2 py-1 font-medium">Unit</th>
              <th class="px-2 py-1 font-medium">Item</th>
              <th v-if="isController" class="px-2 py-1 font-medium">Kiosk</th>
              <th v-if="g.kind !== 'unaccounted'" class="px-2 py-1 font-medium">Holder</th>
              <th class="px-2 py-1 font-medium">Last seen</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-800">
            <tr v-for="(d, i) in rowsOf(g.kind)" :key="g.kind + i" class="hover:bg-slate-900/50">
              <td class="px-2 py-1.5 font-mono text-slate-200">{{ d.instance_code }}</td>
              <td class="px-2 py-1.5 text-slate-300">{{ d.item_name || '—' }}</td>
              <td v-if="isController" class="px-2 py-1.5 font-mono text-slate-400">{{ d.kiosk_code }}</td>
              <td v-if="g.kind !== 'unaccounted'" class="px-2 py-1.5 text-slate-300">{{ d.holder || '—' }}</td>
              <td class="px-2 py-1.5 text-slate-400">
                <span class="text-slate-300">{{ d.zone || '—' }}</span>
                <span class="text-slate-500 text-xs" :title="d.observed_at"> · {{ relativeAge(d.observed_at) }}</span>
              </td>
            </tr>
          </tbody>
        </table>
      </section>

      <p class="text-xs text-slate-600 mt-4">
        Generated {{ relativeAge(result.generated_at) }} · stale threshold
        {{ result.stale_after_hrs }}h
      </p>
    </template>
  </main>
</template>
