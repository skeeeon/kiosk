<!-- TransactionDetailDialog shows the line items for one transaction, loaded
     fresh from the transaction_lines collection. Mirrors the kiosk receipt
     visually but pulls from PB rather than a snapshotted in-memory cart, so
     the data shapes intentionally differ: this one shows uncorrelated flags
     and original_checkout_user, both of which the kiosk side never has. -->
<script setup lang="ts">
import { ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import { pb } from '../lib/pb'

export interface TxLineRow {
  id: string
  action: 'checkout' | 'return' | 'consume'
  qty: number
  serial: string
  uncorrelated: boolean
  expand?: {
    item?: { id: string; code: string; name: string; type: string }
    original_checkout_user?: { id: string; code: string; name: string }
  }
}

export interface TxSummary {
  id: string
  completedAt: string
  userName: string
  userCode: string
  kioskCode: string
  locationCode: string
}

const props = defineProps<{
  open: boolean
  transaction: TxSummary | null
}>()
const emit = defineEmits<{ 'update:open': [value: boolean] }>()

const lines = ref<TxLineRow[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

watch(
  () => [props.open, props.transaction?.id] as const,
  async ([open, id]) => {
    if (!open || !id) return
    lines.value = []
    error.value = null
    loading.value = true
    try {
      lines.value = await pb.collection('transaction_lines').getFullList<TxLineRow>({
        filter: pb.filter('transaction = {:tx}', { tx: id }),
        expand: 'item,original_checkout_user',
        sort: '+created',
      })
    } catch (e) {
      error.value = (e as Error).message
    } finally {
      loading.value = false
    }
  },
)

const ACTION_LABEL: Record<TxLineRow['action'], string> = {
  checkout: 'Checked out',
  return: 'Returned',
  consume: 'Consumed',
}
const ACTION_TONE: Record<TxLineRow['action'], string> = {
  checkout: 'text-emerald-400',
  return: 'text-amber-400',
  consume: 'text-sky-400',
}

function formatDateTime(iso: string): string {
  return new Date(iso).toLocaleString()
}
</script>

<template>
  <AppDialog
    :open="open"
    variant="sheet"
    title="Transaction detail"
    size="lg"
    @update:open="emit('update:open', $event)"
  >
    <div v-if="transaction" class="flex flex-col gap-4">
      <div class="grid grid-cols-2 gap-3 text-sm">
        <div>
          <p class="text-slate-400">Worker</p>
          <p class="text-slate-100">{{ transaction.userName }}</p>
          <p class="text-xs text-slate-500 font-mono">{{ transaction.userCode }}</p>
        </div>
        <div>
          <p class="text-slate-400">Completed</p>
          <p class="text-slate-100">{{ formatDateTime(transaction.completedAt) }}</p>
          <p class="text-xs text-slate-500 font-mono">
            {{ transaction.kioskCode }} · {{ transaction.locationCode }}
          </p>
        </div>
      </div>

      <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 text-sm">
        {{ error }}
      </p>

      <p v-if="loading" class="text-center text-slate-500 py-6 text-sm">Loading…</p>

      <ul
        v-else-if="lines.length > 0"
        class="rounded-xl bg-slate-950/40 border border-slate-800 divide-y divide-slate-800 max-h-[55vh] overflow-y-auto"
      >
        <li v-for="line in lines" :key="line.id" class="flex items-center gap-4 px-4 py-3">
          <span
            class="text-xs font-medium uppercase tracking-wide w-24 shrink-0"
            :class="ACTION_TONE[line.action]"
          >
            {{ ACTION_LABEL[line.action] }}
          </span>
          <div class="min-w-0 flex-1">
            <p class="text-base text-slate-100 truncate">
              {{ line.expand?.item?.name ?? '(deleted item)' }}
            </p>
            <p class="text-xs text-slate-500 truncate">
              {{ line.expand?.item?.code }}<span v-if="line.serial"> · SN {{ line.serial }}</span>
            </p>
            <div class="flex flex-wrap gap-2 mt-1">
              <span
                v-if="line.uncorrelated"
                class="inline-block text-[10px] uppercase tracking-wide px-1.5 py-0.5 rounded bg-amber-900/60 text-amber-200 border border-amber-800/60"
                title="No matching open checkout was found when this return was processed"
              >
                uncorrelated
              </span>
              <span
                v-if="line.expand?.original_checkout_user"
                class="text-xs text-slate-500"
              >
                originally checked out by {{ line.expand.original_checkout_user.name }}
              </span>
            </div>
          </div>
          <span class="text-xl font-semibold tabular-nums text-slate-200 shrink-0">×{{ line.qty }}</span>
        </li>
      </ul>
      <p v-else class="text-center text-slate-500 py-6 text-sm">No lines on this transaction.</p>

      <div class="flex justify-end pt-2">
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          @click="emit('update:open', false)"
        >
          Close
        </button>
      </div>
    </div>
  </AppDialog>
</template>
