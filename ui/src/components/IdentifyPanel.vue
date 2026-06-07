<!-- IdentifyPanel renders the result of an item scan when no cart is active —
     the splash promise "scan an item code to identify it". Shows the scanned
     item's identity (code, name, type, optional serial / instance code) and
     stock context (on-hand, currently out + holder for tools). Inactive items
     get a visible "retired" badge so a worker knows the kiosk recognizes the
     barcode but it's been taken out of rotation.

     The parent passes either a top-level Item (SKU scan) or an InstanceMatch
     (a serialized unit's own barcode). The shape difference is small enough
     that one component handles both. -->
<script setup lang="ts">
import { computed } from 'vue'
import type { Item, InstanceMatch } from '../types'

const props = defineProps<{
  // The scan returned either an item (SKU) or an item_instance.
  item?: Item | null
  instance?: InstanceMatch | null
}>()

defineEmits<{ dismiss: [] }>()

const it = computed<Item | null>(() => {
  if (props.instance) return props.instance.item
  return props.item ?? null
})

const instanceCode = computed(() => props.instance?.instance.code ?? '')
const instanceSerial = computed(() => props.instance?.instance.serial ?? '')

const isTool = computed(() => it.value?.type === 'tool')

// Tools surface "available now"; consumables show stock level. Same number
// underneath, framed differently per type.
const available = computed(() => {
  if (!it.value) return 0
  if (isTool.value) return Math.max(0, it.value.quantity_on_hand - it.value.open_count)
  return it.value.quantity_on_hand
})
</script>

<template>
  <section
    v-if="it"
    class="w-full max-w-2xl rounded-2xl bg-slate-900 border border-slate-800 p-6 shadow-2xl"
  >
    <div class="flex items-start justify-between gap-4">
      <div class="min-w-0">
        <p class="text-xs uppercase tracking-wide text-slate-500 mb-1">Identified</p>
        <h2 class="text-3xl font-semibold text-slate-100 truncate">{{ it.name }}</h2>
        <p class="text-sm text-slate-400 font-mono mt-1">{{ it.code }}</p>
      </div>
      <div class="flex flex-col items-end gap-2 shrink-0">
        <span
          class="inline-block px-2 py-0.5 rounded text-xs capitalize"
          :class="isTool ? 'bg-amber-900/60 text-amber-200' : 'bg-sky-900/60 text-sky-200'"
        >
          {{ it.type }}
        </span>
        <span
          v-if="!it.active"
          class="inline-block px-2 py-0.5 rounded text-xs bg-red-900/60 text-red-200"
        >
          retired
        </span>
      </div>
    </div>

    <div v-if="instance" class="mt-4 rounded-lg bg-slate-950/50 border border-slate-800 px-4 py-3 text-sm">
      <p class="text-slate-400">
        Unit
        <span class="font-mono text-slate-200">{{ instanceCode }}</span>
        <span v-if="instanceSerial"> · SN <span class="font-mono text-slate-200">{{ instanceSerial }}</span></span>
      </p>
    </div>

    <dl class="mt-5 grid grid-cols-2 gap-3 text-sm">
      <div class="rounded-lg bg-slate-950/40 border border-slate-800 p-3">
        <dt class="text-slate-500 text-xs uppercase tracking-wide">On hand</dt>
        <dd class="text-2xl font-semibold text-slate-100 tabular-nums">{{ it.quantity_on_hand }}</dd>
      </div>
      <div class="rounded-lg bg-slate-950/40 border border-slate-800 p-3">
        <dt class="text-slate-500 text-xs uppercase tracking-wide">
          {{ isTool ? 'Available now' : 'In stock' }}
        </dt>
        <dd
          class="text-2xl font-semibold tabular-nums"
          :class="available > 0 ? 'text-emerald-400' : 'text-red-400'"
        >
          {{ available }}
        </dd>
      </div>
    </dl>

    <p
      v-if="isTool && it.open_count > 0"
      class="mt-4 rounded-lg bg-amber-900/30 border border-amber-700/50 text-amber-100 px-4 py-3 text-sm"
    >
      <template v-if="instance">
        Currently checked out to <span class="font-semibold">{{ it.holder || 'someone' }}</span>.
      </template>
      <template v-else-if="it.open_count === 1">
        1 unit currently out
        <span v-if="it.holder"> to <span class="font-semibold">{{ it.holder }}</span></span>.
      </template>
      <template v-else>
        {{ it.open_count }} units currently out (including <span class="font-semibold">{{ it.holder || 'someone' }}</span>).
      </template>
    </p>

    <div class="mt-6 flex items-center justify-between text-sm">
      <p class="text-slate-500">Scan your badge to begin a checkout.</p>
      <button
        type="button"
        class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 transition-transform active:scale-95"
        @click="$emit('dismiss')"
      >
        Dismiss
      </button>
    </div>
  </section>
</template>
