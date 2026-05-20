<script setup lang="ts">
import type { CartAction, CartLine } from '../types'

defineProps<{ lines: CartLine[] }>()

const emit = defineEmits<{
  update: [id: string, patch: { qty?: number; action?: CartAction }]
  remove: [id: string]
}>()

// Keep in sync with cart.MaxQty in internal/cart/store.go.
const MAX_QTY = 99

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
  if (w.startsWith('cross_user_return:')) {
    return `Currently checked out to ${w.slice('cross_user_return:'.length)}`
  }
  return w
}
</script>

<template>
  <ul class="flex flex-col gap-3">
    <li
      v-for="line in lines"
      :key="line.id"
      class="rounded-2xl bg-slate-900 border border-slate-800 p-5 flex flex-col gap-3"
    >
      <div class="flex items-baseline justify-between gap-4">
        <div class="min-w-0">
          <p class="text-2xl font-semibold truncate">{{ line.item_name }}</p>
          <p class="text-sm text-slate-400 truncate">
            {{ line.item_code }}<span v-if="line.serial"> · SN {{ line.serial }}</span>
          </p>
        </div>
        <button
          type="button"
          class="shrink-0 w-12 h-12 rounded-full bg-slate-800 text-slate-300 hover:bg-red-700 hover:text-white text-2xl leading-none"
          aria-label="Remove line"
          @click="emit('remove', line.id)"
        >
          ×
        </button>
      </div>

      <div
        v-for="w in line.warnings ?? []"
        :key="w"
        class="rounded-lg bg-amber-900/40 border border-amber-700/60 text-amber-200 text-sm px-3 py-2"
      >
        {{ warningLabel(w) }}
      </div>

      <div class="flex flex-wrap items-center gap-3">
        <div
          v-if="actionsFor(line).length > 1"
          class="inline-flex rounded-xl overflow-hidden border border-slate-700"
        >
          <button
            v-for="opt in actionsFor(line)"
            :key="opt.value"
            type="button"
            class="px-4 py-3 text-base font-medium transition-colors"
            :class="actionClasses(line, opt.value)"
            @click="emit('update', line.id, { action: opt.value })"
          >
            {{ opt.label }}
          </button>
        </div>
        <span
          v-else-if="actionsFor(line).length === 1"
          class="px-4 py-3 rounded-xl text-base font-medium"
          :class="actionClasses(line, line.action)"
        >
          {{ actionsFor(line)[0].label }}
        </span>

        <div v-if="line.tracking_mode !== 'serialized'" class="inline-flex items-center gap-2 ml-auto">
          <button
            type="button"
            class="w-12 h-12 rounded-xl bg-slate-800 text-2xl hover:bg-slate-700 disabled:opacity-40"
            :disabled="line.qty <= 1"
            aria-label="Decrease quantity"
            @click="emit('update', line.id, { qty: line.qty - 1 })"
          >−</button>
          <span class="w-12 text-center text-2xl tabular-nums">{{ line.qty }}</span>
          <button
            type="button"
            class="w-12 h-12 rounded-xl bg-slate-800 text-2xl hover:bg-slate-700 disabled:opacity-40"
            :disabled="line.qty >= MAX_QTY"
            aria-label="Increase quantity"
            @click="emit('update', line.id, { qty: line.qty + 1 })"
          >+</button>
        </div>
      </div>
    </li>
  </ul>
</template>
