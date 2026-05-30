<!-- AdminMetricsView is the kiosk-local metrics dashboard (/admin/metrics).
     It fetches this kiosk's own operational + activity snapshot from
     GET /api/kiosk/metrics and renders it via MetricsCards. Kiosk-only — the
     nav entry is hidden on the controller, where per-kiosk metrics live on the
     kiosk detail page's Metrics tab. -->
<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api } from '../lib/api'
import MetricsCards from '../components/MetricsCards.vue'
import type { KioskMetrics } from '../types'

const metrics = ref<KioskMetrics | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)

async function load() {
  loading.value = true
  error.value = null
  try {
    metrics.value = await api.get<KioskMetrics>('/api/kiosk/metrics')
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <main class="p-4 sm:p-6 max-w-5xl mx-auto w-full">
    <header class="flex items-center justify-between mb-5">
      <div>
        <h1 class="text-2xl font-semibold">Metrics</h1>
        <p class="text-sm text-slate-400">
          A point-in-time snapshot of this kiosk's health and activity.
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

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 text-sm px-3 py-2 mb-4">
      {{ error }}
    </p>

    <div v-if="loading && !metrics" class="text-slate-500 text-center py-8">Loading…</div>

    <MetricsCards v-if="metrics" :metrics="metrics" />
  </main>
</template>
