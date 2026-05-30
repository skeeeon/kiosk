<!-- MetricsCards is the presentational half of the metrics feature: given a
     KioskMetrics snapshot it renders the operational + activity tiles. Both
     the kiosk-local AdminMetricsView and the controller's KioskMetricsPanel
     wrap it with their own fetch/offline logic so the two render identically. -->
<script setup lang="ts">
import { computed } from 'vue'
import type { KioskMetrics } from '../types'

const props = defineProps<{ metrics: KioskMetrics }>()

// Human-readable uptime: "3d 4h", "4h 12m", "12m", "<1m". Coarse on purpose —
// this is a glanceable gauge, not a stopwatch.
const uptimeDisplay = computed(() => {
  const total = Math.max(0, Math.floor(props.metrics.operational.uptime_seconds))
  const d = Math.floor(total / 86400)
  const h = Math.floor((total % 86400) / 3600)
  const m = Math.floor((total % 3600) / 60)
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}m`
  if (m > 0) return `${m}m`
  return '<1m'
})

const rfidLabel = computed(() => {
  const op = props.metrics.operational
  if (!op.rfid_enabled) return 'Disabled'
  const mode = op.rfid_mode ? ` · ${op.rfid_mode}` : ''
  return (op.rfid_connected ? 'Connected' : 'Disconnected') + mode
})

// Chip color: green = good, red = bad, slate = not applicable.
function chipClass(state: 'ok' | 'bad' | 'na'): string {
  switch (state) {
    case 'ok':
      return 'bg-emerald-900/60 text-emerald-200'
    case 'bad':
      return 'bg-red-900/60 text-red-200'
    default:
      return 'bg-slate-800 text-slate-400'
  }
}

const natsState = computed<'ok' | 'bad'>(() =>
  props.metrics.operational.nats_connected ? 'ok' : 'bad',
)
const rfidState = computed<'ok' | 'bad' | 'na'>(() => {
  const op = props.metrics.operational
  if (!op.rfid_enabled) return 'na'
  return op.rfid_connected ? 'ok' : 'bad'
})

const generatedDisplay = computed(() => {
  const d = new Date(props.metrics.generated_at)
  return Number.isNaN(d.getTime()) ? props.metrics.generated_at : d.toLocaleString()
})
</script>

<template>
  <div class="space-y-6">
    <!-- Operational -->
    <section>
      <h3 class="text-sm font-medium text-slate-300 mb-3">Operational</h3>
      <div class="grid grid-cols-2 sm:grid-cols-4 gap-3">
        <div class="rounded-xl bg-slate-900 border border-slate-800 p-4">
          <p class="text-xs text-slate-500">Uptime</p>
          <p class="mt-1 text-xl font-semibold text-slate-100 tabular-nums">{{ uptimeDisplay }}</p>
        </div>
        <div class="rounded-xl bg-slate-900 border border-slate-800 p-4">
          <p class="text-xs text-slate-500">Active sessions</p>
          <p class="mt-1 text-xl font-semibold text-slate-100 tabular-nums">
            {{ metrics.operational.active_carts }}
          </p>
        </div>
        <div class="rounded-xl bg-slate-900 border border-slate-800 p-4">
          <p class="text-xs text-slate-500 mb-1.5">NATS</p>
          <span
            class="inline-block px-2 py-0.5 rounded text-xs font-medium"
            :class="chipClass(natsState)"
          >
            {{ metrics.operational.nats_connected ? 'Connected' : 'Disconnected' }}
          </span>
        </div>
        <div class="rounded-xl bg-slate-900 border border-slate-800 p-4">
          <p class="text-xs text-slate-500 mb-1.5">RFID reader</p>
          <span
            class="inline-block px-2 py-0.5 rounded text-xs font-medium"
            :class="chipClass(rfidState)"
          >
            {{ rfidLabel }}
          </span>
        </div>
      </div>
    </section>

    <!-- Activity -->
    <section>
      <h3 class="text-sm font-medium text-slate-300 mb-3">Activity</h3>
      <div class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-3">
        <div class="rounded-xl bg-slate-900 border border-slate-800 p-4">
          <p class="text-xs text-slate-500">Items out</p>
          <p class="mt-1 text-xl font-semibold text-slate-100 tabular-nums">{{ metrics.ledger.items_out }}</p>
        </div>
        <div class="rounded-xl bg-slate-900 border border-slate-800 p-4">
          <p class="text-xs text-slate-500">Holders</p>
          <p class="mt-1 text-xl font-semibold text-slate-100 tabular-nums">
            {{ metrics.ledger.users_with_items_out }}
          </p>
        </div>
        <div class="rounded-xl bg-slate-900 border border-slate-800 p-4">
          <p class="text-xs text-slate-500">Low stock</p>
          <p
            class="mt-1 text-xl font-semibold tabular-nums"
            :class="metrics.ledger.low_stock_skus > 0 ? 'text-amber-300' : 'text-slate-100'"
          >
            {{ metrics.ledger.low_stock_skus }}
          </p>
        </div>
        <div class="rounded-xl bg-slate-900 border border-slate-800 p-4">
          <p class="text-xs text-slate-500">Transactions today</p>
          <p class="mt-1 text-xl font-semibold text-slate-100 tabular-nums">
            {{ metrics.ledger.transactions_today }}
          </p>
        </div>
        <div class="rounded-xl bg-slate-900 border border-slate-800 p-4">
          <p class="text-xs text-slate-500">Transactions (7d)</p>
          <p class="mt-1 text-xl font-semibold text-slate-100 tabular-nums">
            {{ metrics.ledger.transactions_week }}
          </p>
        </div>
      </div>
    </section>

    <p class="text-xs text-slate-600">Snapshot generated {{ generatedDisplay }}</p>
  </div>
</template>
