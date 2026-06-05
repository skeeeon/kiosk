<script setup lang="ts">
import { computed } from 'vue'
import type { CartAction, CartLine } from '../types'
import { useKioskIdentity } from '../composables/useKioskIdentity'

defineProps<{ lines: CartLine[] }>()

const emit = defineEmits<{
  update: [id: string, patch: { qty?: number; action?: CartAction; request_maintenance?: boolean }]
  remove: [id: string]
}>()

// MaxQty comes from /api/kiosk/identity so client and server share one source
// of truth (cart.MaxQty in Go). 99 is the bootstrap fallback used before the
// identity payload lands.
const { identity } = useKioskIdentity()
const maxQty = computed(() => identity.value?.max_qty ?? 99)

// Tools can be checked out or returned; consumables can only be consumed.
// Source of truth for this rule lives in cart.ValidActionForType (Go).
const ACTIONS_BY_TYPE: Record<CartLine['item_type'], { value: CartAction; label: string }[]> = {
  tool: [
    { value: 'checkout', label: 'Check out' },
    { value: 'return', label: 'Return' },
  ],
  consumable: [
    { value: 'consume', label: 'Consume' },
  ],
}

function actionsFor(line: CartLine): { value: CartAction; label: string }[] {
  return ACTIONS_BY_TYPE[line.item_type] ?? []
}

function actionClasses(line: CartLine, action: CartAction): string {
  if (line.action === action) {
    if (action === 'checkout') return 'bg-emerald-600 text-white'
    if (action === 'return') return 'bg-amber-600 text-white'
    return 'bg-sky-600 text-white'
  }
  return 'bg-slate-800 text-slate-300 hover:bg-slate-700'
}

function warningLabel(w: string): string {
  if (w.startsWith('low_stock:available=')) {
    const n = w.slice('low_stock:available='.length)
    return n === '0'
      ? 'None currently available — proceeding anyway'
      : `Only ${n} available — proceeding anyway`
  }
  return w
}

function warningClasses(w: string): string {
  if (w.startsWith('low_stock:')) {
    return 'rounded-lg bg-red-900/40 border border-red-700/60 text-red-200 text-sm px-3 py-2'
  }
  return 'rounded-lg bg-amber-900/40 border border-amber-700/60 text-amber-200 text-sm px-3 py-2'
}
</script>

<template>
  <ul class="flex flex-col gap-2">
    <li
      v-for="line in lines"
      :key="line.id"
      class="rounded-xl bg-slate-900 border border-slate-800 p-3 flex flex-col gap-2"
    >
      <!-- Primary row: item info on the left, controls inline on the right.
           Wraps when narrower than ~640px so qty/delete can drop to a new
           line rather than crushing the item name. -->
      <div class="flex items-center gap-3 flex-wrap">
        <div class="min-w-0 flex-1">
          <p class="text-xl font-semibold truncate leading-tight">{{ line.item_name }}</p>
          <p class="text-xs text-slate-400 truncate font-mono">
            {{ line.item_code }}<span v-if="line.serial"> · SN {{ line.serial }}</span>
          </p>
        </div>

        <div
          v-if="actionsFor(line).length > 1"
          class="inline-flex rounded-lg overflow-hidden border border-slate-700 shrink-0"
        >
          <button
            v-for="opt in actionsFor(line)"
            :key="opt.value"
            type="button"
            class="px-3 py-2 text-sm font-medium transition-colors"
            :class="actionClasses(line, opt.value)"
            @click="emit('update', line.id, { action: opt.value })"
          >
            {{ opt.label }}
          </button>
        </div>
        <span
          v-else-if="actionsFor(line).length === 1"
          class="px-3 py-2 rounded-lg text-sm font-medium shrink-0"
          :class="actionClasses(line, line.action)"
        >
          {{ actionsFor(line)[0].label }}
        </span>

        <div v-if="line.tracking_mode !== 'serialized'" class="inline-flex items-center gap-1 shrink-0">
          <button
            type="button"
            class="w-11 h-11 rounded-lg bg-slate-800 text-xl hover:bg-slate-700 disabled:opacity-40"
            :disabled="line.qty <= 1"
            aria-label="Decrease quantity"
            @click="emit('update', line.id, { qty: line.qty - 1 })"
          >−</button>
          <span class="w-10 text-center text-xl tabular-nums">{{ line.qty }}</span>
          <button
            type="button"
            class="w-11 h-11 rounded-lg bg-slate-800 text-xl hover:bg-slate-700 disabled:opacity-40"
            :disabled="line.qty >= maxQty"
            aria-label="Increase quantity"
            @click="emit('update', line.id, { qty: line.qty + 1 })"
          >+</button>
        </div>

        <button
          type="button"
          class="shrink-0 w-11 h-11 rounded-lg bg-slate-800 text-slate-300 hover:bg-red-700 hover:text-white text-2xl leading-none"
          aria-label="Remove line"
          @click="emit('remove', line.id)"
        >
          ×
        </button>
      </div>

      <!-- Foreman-return chip: a line carrying original_checkout_user_id is
           an explicit "I'm closing this on behalf of X" action — make that
           visible before commit so the foreman sees what they queued up. -->
      <div
        v-if="line.original_checkout_user_id"
        class="rounded-lg bg-amber-900/40 border border-amber-700/60 text-amber-200 text-sm px-3 py-2"
      >
        Foreman return: {{ line.original_checkout_user_name || 'another worker' }}
      </div>

      <!-- Needs-maintenance toggle: serialized return lines only. Any worker
           can flag the unit so commit routes it into maintenance after the
           return. -->
      <label
        v-if="line.tracking_mode === 'serialized' && line.action === 'return'"
        class="flex items-center gap-2 rounded-lg bg-slate-950/60 border border-slate-800 text-slate-300 text-sm px-3 py-2 cursor-pointer"
      >
        <input
          type="checkbox"
          class="w-4 h-4"
          :checked="line.request_maintenance ?? false"
          @change="emit('update', line.id, { request_maintenance: ($event.target as HTMLInputElement).checked })"
        />
        Needs maintenance — hold this unit out of service after return
      </label>

      <!-- Other warnings stay full-width below. -->
      <div
        v-for="w in line.warnings ?? []"
        :key="w"
        :class="warningClasses(w)"
      >
        {{ warningLabel(w) }}
      </div>
    </li>
  </ul>
</template>
