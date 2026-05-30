<!-- KioskMetricsPanel surfaces a managed kiosk's live metrics from the
     controller. On mount (and when kioskCode changes) it fetches a snapshot
     via the metrics.snapshot NATS command, proxied by
     GET /api/controller/kiosks/:code/metrics, and renders it with MetricsCards.

     503 with body {error: "kiosk_offline"} renders an inline amber banner; any
     other error surfaces in the usual red box. Mirrors KioskInventoryPanel. -->
<script setup lang="ts">
import { ref, watch } from 'vue'
import MetricsCards from './MetricsCards.vue'
import { api, ApiError } from '../lib/api'
import type { KioskMetrics, KioskOfflineError } from '../types'

const props = defineProps<{ kioskCode: string }>()

const metrics = ref<KioskMetrics | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)
const offline = ref(false)

async function load() {
  if (!props.kioskCode) return
  loading.value = true
  error.value = null
  offline.value = false
  try {
    metrics.value = await api.get<KioskMetrics>(
      `/api/controller/kiosks/${encodeURIComponent(props.kioskCode)}/metrics`,
    )
  } catch (e) {
    if (isOfflineError(e)) {
      offline.value = true
      metrics.value = null
    } else {
      error.value = (e as Error).message
    }
  } finally {
    loading.value = false
  }
}

// isOfflineError identifies the 503 + kiosk_offline body so the panel renders a
// banner rather than a red error box. Any other error stays a generic failure.
function isOfflineError(e: unknown): boolean {
  if (e instanceof ApiError && e.status === 503) {
    const data = e.data as KioskOfflineError | null
    return data?.error === 'kiosk_offline'
  }
  return false
}

watch(() => props.kioskCode, (c) => { if (c) void load() }, { immediate: true })
</script>

<template>
  <section class="space-y-3">
    <header class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2">
      <div>
        <h3 class="text-sm font-medium text-slate-200">Live metrics</h3>
        <p class="text-xs text-slate-500">
          Snapshot fetched from the kiosk over NATS. Refresh to re-poll.
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
      This kiosk hasn't sent a heartbeat recently. Metrics are unavailable until
      it reconnects.
    </div>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 text-sm px-3 py-2">
      {{ error }}
    </p>

    <div v-if="loading && !metrics" class="text-slate-500 text-center py-8">Loading…</div>

    <MetricsCards v-if="metrics" :metrics="metrics" />
  </section>
</template>
