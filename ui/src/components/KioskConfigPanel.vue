<!-- KioskConfigPanel surfaces a managed kiosk's RFID reader/enclosure topology
     from the controller. On mount (and when kioskCode changes) it fetches a
     read-only snapshot via the config.snapshot NATS command, proxied by
     GET /api/controller/kiosks/:code/config, and renders one row per reader
     with its live LLRP connection status.

     Observability only — reader config is edited in the kiosk's own YAML, not
     here. 503 {error: "kiosk_offline"} renders an amber banner; other errors
     surface in the usual red box. Mirrors KioskMetricsPanel. -->
<script setup lang="ts">
import { ref, watch } from 'vue'
import { api, isKioskOfflineError as isOfflineError } from '../lib/api'

interface ReaderConfig {
  reader_id: string
  mode: string
  host: string
  port: number
  enclosure_id?: string
  antennas: number
  connected: boolean
}
interface RFIDConfig {
  enabled: boolean
  read_window_ms: number
  readers: ReaderConfig[]
}

const props = defineProps<{ kioskCode: string }>()

const config = ref<RFIDConfig | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)
const offline = ref(false)

async function load() {
  if (!props.kioskCode) return
  loading.value = true
  error.value = null
  offline.value = false
  try {
    config.value = await api.get<RFIDConfig>(
      `/api/controller/kiosks/${encodeURIComponent(props.kioskCode)}/config`,
    )
  } catch (e) {
    if (isOfflineError(e)) {
      offline.value = true
      config.value = null
    } else {
      error.value = (e as Error).message
    }
  } finally {
    loading.value = false
  }
}

watch(() => props.kioskCode, (c) => { if (c) void load() }, { immediate: true })
</script>

<template>
  <section class="space-y-3">
    <header class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2">
      <div>
        <h3 class="text-sm font-medium text-slate-200">RFID readers</h3>
        <p class="text-xs text-slate-500">
          Reader / enclosure topology fetched from the kiosk over NATS. Read-only
          — edit it in the kiosk's own config and restart.
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

    <div
      v-if="offline"
      class="rounded-lg bg-amber-900/40 border border-amber-800 text-amber-100 text-sm px-3 py-2"
    >
      This kiosk hasn't sent a heartbeat recently. Reader config is unavailable
      until it reconnects.
    </div>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 text-sm px-3 py-2">
      {{ error }}
    </p>

    <div v-if="loading && !config" class="text-slate-500 text-center py-8">Loading…</div>

    <div v-if="config" class="space-y-3">
      <p v-if="!config.enabled" class="text-slate-500 text-sm">RFID is disabled on this kiosk.</p>
      <template v-else>
        <p class="text-xs text-slate-500">
          Read window: <span class="text-slate-300">{{ config.read_window_ms }} ms</span> (shared across readers)
        </p>
        <p v-if="config.readers.length === 0" class="text-slate-500 text-sm">No readers configured.</p>
        <div v-else class="overflow-x-auto rounded-xl border border-slate-800">
          <table class="w-full text-sm text-left">
            <thead class="text-xs text-slate-500 bg-slate-900/60">
              <tr>
                <th class="px-3 py-2 font-medium">Reader</th>
                <th class="px-3 py-2 font-medium">Mode</th>
                <th class="px-3 py-2 font-medium">Enclosure</th>
                <th class="px-3 py-2 font-medium">Endpoint</th>
                <th class="px-3 py-2 font-medium">Antennas</th>
                <th class="px-3 py-2 font-medium">Status</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-800">
              <tr v-for="r in config.readers" :key="r.reader_id">
                <td class="px-3 py-2 text-slate-200 font-mono text-xs">{{ r.reader_id }}</td>
                <td class="px-3 py-2 text-slate-300">{{ r.mode }}</td>
                <td class="px-3 py-2 text-slate-300">{{ r.enclosure_id || '—' }}</td>
                <td class="px-3 py-2 text-slate-400 font-mono text-xs">{{ r.host }}:{{ r.port }}</td>
                <td class="px-3 py-2 text-slate-400">{{ r.antennas > 0 ? r.antennas : 'baseline' }}</td>
                <td class="px-3 py-2">
                  <span
                    :class="r.connected
                      ? 'text-emerald-300 bg-emerald-900/40 border-emerald-800'
                      : 'text-slate-400 bg-slate-800 border-slate-700'"
                    class="inline-block rounded px-2 py-0.5 text-xs border"
                  >
                    {{ r.connected ? 'connected' : 'offline' }}
                  </span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </div>
  </section>
</template>
