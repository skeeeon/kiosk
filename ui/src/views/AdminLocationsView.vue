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
import { statusBadgeClass, statusLabel, type InstanceStatus } from '../lib/instanceStatus'
import AppDialog from '../components/AppDialog.vue'
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
// The unit whose detail sheet is open (null = closed). Rows are click-to-open.
const selected = ref<LocationRow | null>(null)

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

function absoluteTime(iso?: string): string {
  if (!iso) return '—'
  const t = new Date(iso)
  if (Number.isNaN(t.getTime())) return '—'
  return t.toLocaleString()
}

// Staleness tiers — high-signal for an asset tracker: a unit seen minutes ago is
// trustworthy, one unseen for days is suspect. Advisory color only, mirrors the
// fresh→stale intuition (green → red). Matches no business rule; purely a cue.
function ageClass(iso?: string): string {
  if (!iso) return 'text-slate-500'
  const diff = Date.now() - new Date(iso).getTime()
  if (!Number.isFinite(diff)) return 'text-slate-500'
  if (diff < 3_600_000) return 'text-emerald-400' // < 1h
  if (diff < 86_400_000) return 'text-slate-300' // < 1d
  if (diff < 604_800_000) return 'text-amber-400' // < 7d
  return 'text-red-400' // ≥ 7d
}

// Coordinates worth showing: a zone-only sighting stores 0,0 (PB number columns
// are NOT NULL DEFAULT 0), which means "no GPS", not "Null Island".
function hasGps(r: LocationRow): boolean {
  return !!(r.lat || r.lon)
}
function coords(r: LocationRow): string {
  return `${r.lat?.toFixed(5)}, ${r.lon?.toFixed(5)}`
}
// Coarse location, so we just deep-link the operator's browser map
// (OpenStreetMap — no embed, no dependency, no API key) with a pin at the
// last-seen point. The product is zone/last-seen, not an RTLS map canvas.
function mapUrl(r: LocationRow): string {
  return `https://www.openstreetmap.org/?mlat=${r.lat}&mlon=${r.lon}#map=16/${r.lat}/${r.lon}`
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
                <th class="px-3 py-2 font-medium">Location</th>
                <th class="px-3 py-2 font-medium">Holder</th>
                <th class="px-3 py-2 font-medium">Last seen</th>
                <th class="px-3 py-2 w-8" aria-label="Open detail"></th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-800">
              <tr
                v-for="(r, i) in visible"
                :key="r.kiosk_code + r.instance_code + i"
                class="hover:bg-slate-900/50 cursor-pointer focus:outline-none focus:bg-slate-900/70"
                role="button"
                tabindex="0"
                @click="selected = r"
                @keydown.enter="selected = r"
                @keydown.space.prevent="selected = r"
              >
                <td class="px-3 py-2 align-top">
                  <div class="font-mono text-slate-200">{{ r.instance_code }}</div>
                  <span
                    v-if="r.status"
                    :class="['inline-block mt-1 px-1.5 py-0.5 rounded text-[10px] font-medium', statusBadgeClass(r.status as InstanceStatus)]"
                  >
                    {{ statusLabel(r.status as InstanceStatus) }}
                  </span>
                </td>
                <td class="px-3 py-2 align-top text-slate-300">{{ r.item_name || r.item_code || '—' }}</td>
                <td v-if="isController" class="px-3 py-2 align-top font-mono text-slate-400">{{ r.kiosk_code }}</td>
                <td class="px-3 py-2 align-top">
                  <span v-if="r.zone" class="text-slate-300">{{ r.zone }}</span>
                  <a
                    v-else-if="hasGps(r)"
                    :href="mapUrl(r)"
                    target="_blank"
                    rel="noopener"
                    class="inline-flex items-center gap-1 font-mono text-xs text-sky-400 hover:text-sky-300"
                    @click.stop
                  >
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="h-3.5 w-3.5 shrink-0">
                      <path stroke-linecap="round" stroke-linejoin="round" d="M15 10.5a3 3 0 11-6 0 3 3 0 016 0z" />
                      <path stroke-linecap="round" stroke-linejoin="round" d="M19.5 10.5c0 7.142-7.5 11.25-7.5 11.25S4.5 17.642 4.5 10.5a7.5 7.5 0 1115 0z" />
                    </svg>
                    {{ coords(r) }}
                  </a>
                  <span v-else class="text-slate-500">—</span>
                </td>
                <td class="px-3 py-2 align-top" :class="r.holder ? 'text-slate-300' : 'text-slate-500'">
                  {{ r.holder || '—' }}
                </td>
                <td class="px-3 py-2 align-top whitespace-nowrap">
                  <span :class="ageClass(r.observed_at)">{{ relativeAge(r.observed_at) }}</span>
                </td>
                <td class="px-3 py-2 align-top text-slate-600">›</td>
              </tr>
            </tbody>
          </table>
        </div>

        <p class="text-xs text-slate-600 mt-3">
          Generated {{ relativeAge(result.generated_at) }} · most recently seen first
        </p>
      </template>
    </template>

    <!-- Per-unit detail sheet: the row click opens this with every field the
         table can't fit (exact timestamp, gateway, full coordinates + map). -->
    <AppDialog
      :open="!!selected"
      variant="sheet"
      size="md"
      :title="selected?.item_name || selected?.item_code || selected?.instance_code || 'Location'"
      :description="selected ? `Unit ${selected.instance_code}` : ''"
      @update:open="(v) => { if (!v) selected = null }"
    >
      <dl v-if="selected" class="flex flex-col divide-y divide-slate-800 text-sm">
        <div class="flex items-start justify-between gap-4 py-2.5">
          <dt class="text-slate-400">Last seen</dt>
          <dd class="text-right">
            <span :class="ageClass(selected.observed_at)">{{ relativeAge(selected.observed_at) }}</span>
            <div class="text-xs text-slate-500">{{ absoluteTime(selected.observed_at) }}</div>
          </dd>
        </div>

        <div v-if="selected.status" class="flex items-center justify-between gap-4 py-2.5">
          <dt class="text-slate-400">Status</dt>
          <dd>
            <span
              :class="['px-2 py-0.5 rounded text-xs font-medium', statusBadgeClass(selected.status as InstanceStatus)]"
            >
              {{ statusLabel(selected.status as InstanceStatus) }}
            </span>
          </dd>
        </div>

        <div class="flex items-center justify-between gap-4 py-2.5">
          <dt class="text-slate-400">Holder</dt>
          <dd :class="selected.holder ? 'text-slate-200' : 'text-slate-500'">
            {{ selected.holder || 'Not checked out' }}
          </dd>
        </div>

        <div v-if="isController" class="flex items-center justify-between gap-4 py-2.5">
          <dt class="text-slate-400">Kiosk</dt>
          <dd class="font-mono text-slate-300">{{ selected.kiosk_code }}</dd>
        </div>

        <div class="flex items-center justify-between gap-4 py-2.5">
          <dt class="text-slate-400">Zone</dt>
          <dd :class="selected.zone ? 'text-slate-200' : 'text-slate-500'">{{ selected.zone || '—' }}</dd>
        </div>

        <div class="flex items-center justify-between gap-4 py-2.5">
          <dt class="text-slate-400">Gateway</dt>
          <dd :class="selected.gateway ? 'font-mono text-slate-300' : 'text-slate-500'">
            {{ selected.gateway || '—' }}
          </dd>
        </div>

        <div class="flex items-start justify-between gap-4 py-2.5">
          <dt class="text-slate-400">Coordinates</dt>
          <dd class="text-right">
            <template v-if="hasGps(selected)">
              <div class="font-mono text-slate-200">{{ coords(selected) }}</div>
              <a
                :href="mapUrl(selected)"
                target="_blank"
                rel="noopener"
                class="inline-flex items-center gap-1 text-xs text-sky-400 hover:text-sky-300"
              >
                View on map
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="h-3.5 w-3.5">
                  <path stroke-linecap="round" stroke-linejoin="round" d="M13.5 6H5.25A2.25 2.25 0 003 8.25v10.5A2.25 2.25 0 005.25 21h10.5A2.25 2.25 0 0018 18.75V10.5m-10.5 6L21 3m0 0h-5.25M21 3v5.25" />
                </svg>
              </a>
            </template>
            <span v-else class="text-slate-500">No GPS fix</span>
          </dd>
        </div>
      </dl>

      <p class="text-xs text-slate-600 mt-4">
        Advisory last-seen — lossy and never authoritative. It never gates custody.
      </p>
    </AppDialog>
  </main>
</template>
