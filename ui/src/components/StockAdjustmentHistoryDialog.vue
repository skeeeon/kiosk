<!-- StockAdjustmentHistoryDialog shows past adjustments for one item:
     when, who, signed delta, resulting quantity, reason. Read-only. -->
<script setup lang="ts">
import { ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import { pb } from '../lib/pb'
import type { StockAdjustmentRecord } from '../types'

const props = defineProps<{
  open: boolean
  itemId: string
  itemCode: string
}>()

const emit = defineEmits<{ 'update:open': [value: boolean] }>()

const rows = ref<StockAdjustmentRecord[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

async function load() {
  if (!props.itemId) return
  loading.value = true
  error.value = null
  try {
    rows.value = await pb.collection('stock_adjustments').getFullList<StockAdjustmentRecord>({
      filter: `item = "${props.itemId}"`,
      sort: '-created',
      expand: 'admin',
    })
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

watch(() => props.open, (o) => { if (o) void load() })

function fmtDate(iso: string): string {
  return new Date(iso).toLocaleString()
}
</script>

<template>
  <AppDialog
    :open="open"
    title="Adjustment history"
    :description="`${itemCode}`"
    size="lg"
    @update:open="emit('update:open', $event)"
  >
    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 text-sm px-3 py-2 mb-3">
      {{ error }}
    </p>

    <div v-if="loading" class="text-center text-slate-500 py-6">Loading…</div>
    <div v-else-if="rows.length === 0" class="text-center text-slate-500 py-6">
      No adjustments recorded yet.
    </div>
    <table v-else class="w-full text-left text-sm">
      <thead class="text-slate-500 text-xs">
        <tr>
          <th class="px-2 py-2 font-medium">When</th>
          <th class="px-2 py-2 font-medium">Who</th>
          <th class="px-2 py-2 font-medium text-right">Δ</th>
          <th class="px-2 py-2 font-medium text-right">New</th>
          <th class="px-2 py-2 font-medium">Reason</th>
        </tr>
      </thead>
      <tbody class="divide-y divide-slate-800">
        <tr v-for="r in rows" :key="r.id">
          <td class="px-2 py-2 text-slate-400 whitespace-nowrap">{{ fmtDate(r.created) }}</td>
          <td class="px-2 py-2 text-slate-300">
            {{ r.expand?.admin?.name ?? r.expand?.admin?.email ?? '—' }}
          </td>
          <td
            class="px-2 py-2 text-right tabular-nums font-medium"
            :class="r.delta > 0 ? 'text-emerald-400' : r.delta < 0 ? 'text-amber-400' : 'text-slate-400'"
          >
            {{ r.delta > 0 ? '+' : '' }}{{ r.delta }}
          </td>
          <td class="px-2 py-2 text-right tabular-nums text-slate-200">{{ r.new_quantity }}</td>
          <td class="px-2 py-2 text-slate-300">{{ r.reason }}</td>
        </tr>
      </tbody>
    </table>

    <div class="flex justify-end mt-4">
      <button
        type="button"
        class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
        @click="emit('update:open', false)"
      >
        Close
      </button>
    </div>
  </AppDialog>
</template>
