<!-- AdminLocationsView is the advisory asset-location report
     (docs/location-sightings-plan.md, L4) — the inverse of Reconciliation: it
     lists *everything that's been seen*, not just custody-vs-location
     discrepancies. Observability only — last-seen is lossy, advisory, never
     authoritative. Works on both binaries: the controller reads its fleet-wide
     instance_location view, a node reads its own item_instances. Empty until a
     gateway or a zoned custody reader reports. -->
<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api } from '../lib/api'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import type { LocationReport, LocationRow } from '../types'

const { identity } = useKioskIdentity()
const isController = computed(() => identity.value?.role === 'controller')
const endpoint = computed(() =>
  isController.value ? '/api/controller/locations' : '/api/kiosk/locations',
)

const result = ref<LocationReport | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)
const filter = ref('')

async function load() {
  loading.value = true
  error.value = null
  try {
    result.value = await api.get<LocationReport>(endpoint.value)
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}
onMounted(load)

// The API's `locations` is a Go slice that marshals to `null` (not `[]`) when
// empty, so funnel reads through one null-safe accessor (same lesson as the
// reconciliation view) — a bare `.length`/`.filter` on null would crash.
const locations = computed(() => result.value?.locations ?? [])

const total = computed(() => locations.value.length)

const visible = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return locations.value
  return locations.value.filter((r) =>
    [r.instance_code, r.item_name, r.item_code, r.zone, r.holder, r.kiosk_code]
      .some((v) => v?.toLowerCase().includes(q)),
  )
})

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

// Coordinates worth showing: a zone-only sighting stores 0,0 (PB number columns
// are NOT NULL DEFAULT 0), which means "no GPS", not "Null Island".
function hasGps(r: LocationRow): boolean {
  return !!(r.lat || r.lon)
}
function coords(r: LocationRow): string {
  return `${r.lat?.toFixed(5)}, ${r.lon?.toFixed(5)}`
}
</script>

<template>
  <main class="p-4 sm:p-6 max-w-5xl mx-auto w-full">
    <header class="flex items-center justify-between mb-5 gap-3">
      <div>
        <h1 class="text-2xl font-semibold">Locations</h1>
        <p class="text-sm text-slate-400">
          Where each tracked unit was last seen. Advisory only — last-seen is
          lossy and never gates custody.
        </p>
      </div>
      <button
        type="button"
        class="text-sm px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 disabled:opacity-50 shrink-0"
        :disabled="loading"
        @click="load"
      >
        {{ loading ? 'Loading…' : 'Refresh' }}
      </button>
    </header>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 text-sm px-3 py-2 mb-3">
      {{ error }}
    </p>

    <p v-else-if="loading && !result" class="text-slate-500 text-sm">Loading…</p>

    <template v-else-if="result">
      <p v-if="total === 0" class="rounded-lg bg-slate-900/60 border border-slate-800 text-slate-300 text-sm px-4 py-3">
        Nothing has been observed yet. Units appear here once a gateway or a
        zoned RFID reader reports a sighting.
      </p>

      <template v-else>
        <div class="flex items-center gap-3 mb-3">
          <input
            v-model="filter"
            type="text"
            placeholder="Filter by unit, item, zone, holder…"
            class="flex-1 rounded-lg bg-slate-900 border border-slate-700 px-3 py-1.5 text-sm"
          />
          <span class="text-xs text-slate-500 whitespace-nowrap">{{ visible.length }} of {{ total }}</span>
        </div>

        <div class="overflow-x-auto rounded-xl border border-slate-800">
          <table class="w-full text-left text-sm">
            <thead class="text-slate-500 text-xs bg-slate-900/60">
              <tr>
                <th class="px-3 py-2 font-medium">Unit</th>
                <th class="px-3 py-2 font-medium">Item</th>
                <th v-if="isController" class="px-3 py-2 font-medium">Kiosk</th>
                <th class="px-3 py-2 font-medium">Zone</th>
                <th class="px-3 py-2 font-medium">Holder</th>
                <th class="px-3 py-2 font-medium">Last seen</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-800">
              <tr v-for="(r, i) in visible" :key="r.kiosk_code + r.instance_code + i" class="hover:bg-slate-900/50">
                <td class="px-3 py-2 font-mono text-slate-200">{{ r.instance_code }}</td>
                <td class="px-3 py-2 text-slate-300">{{ r.item_name || r.item_code || '—' }}</td>
                <td v-if="isController" class="px-3 py-2 font-mono text-slate-400">{{ r.kiosk_code }}</td>
                <td class="px-3 py-2 text-slate-300">
                  {{ r.zone || '—' }}
                  <span v-if="hasGps(r)" class="text-slate-500 text-xs font-mono" :title="coords(r)"> · gps</span>
                </td>
                <td class="px-3 py-2 text-slate-300">{{ r.holder || '—' }}</td>
                <td class="px-3 py-2 text-slate-400 whitespace-nowrap" :title="r.observed_at">
                  {{ relativeAge(r.observed_at) }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <p class="text-xs text-slate-600 mt-3">
          Generated {{ relativeAge(result.generated_at) }} · most recently seen first
        </p>
      </template>
    </template>
  </main>
</template>
