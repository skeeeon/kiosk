<!-- ItemBrowseDialog lets a worker search the catalog and tap items to add
     them to the active cart. Used when a scan isn't practical (torn label,
     no barcode, consumable picked up by hand). Stays open after each add so
     the worker can grab several items in a row without retyping a query.

     Note: the kiosk's window-level scan listener pauses while an <input> in
     this dialog is focused (see useScan), so scanning works after closing. -->
<script setup lang="ts">
import { ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import { api, ApiError } from '../lib/api'
import type { Item } from '../types'

const props = defineProps<{
  open: boolean
  pending: boolean
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  pick: [code: string]
  error: [message: string]
}>()

type TypeFilter = 'all' | 'tool' | 'consumable'

const query = ref('')
const typeFilter = ref<TypeFilter>('all')
const items = ref<Item[]>([])
const loading = ref(false)
const page = ref(1)
const hasMore = ref(false)
const pickingCode = ref<string | null>(null)

let searchSeq = 0
let debounceHandle: ReturnType<typeof setTimeout> | null = null

async function load(reset: boolean) {
  const seq = ++searchSeq
  loading.value = true
  if (reset) page.value = 1
  try {
    const params = new URLSearchParams()
    if (query.value.trim()) params.set('q', query.value.trim())
    if (typeFilter.value !== 'all') params.set('type', typeFilter.value)
    params.set('page', String(page.value))
    const res = await api.get<{ items: Item[]; has_more: boolean }>(
      `/api/kiosk/items?${params.toString()}`,
    )
    if (seq !== searchSeq) return // a newer search superseded us
    items.value = reset ? res.items : items.value.concat(res.items)
    hasMore.value = res.has_more
  } catch (e) {
    if (seq !== searchSeq) return
    emit('error', e instanceof ApiError ? e.message : (e as Error).message)
  } finally {
    if (seq === searchSeq) loading.value = false
  }
}

watch(
  () => props.open,
  (open) => {
    if (!open) return
    query.value = ''
    typeFilter.value = 'all'
    items.value = []
    page.value = 1
    void load(true)
  },
)

watch([query, typeFilter], () => {
  if (!props.open) return
  if (debounceHandle) clearTimeout(debounceHandle)
  debounceHandle = setTimeout(() => void load(true), 180)
})

async function loadMore() {
  page.value += 1
  await load(false)
}

async function pick(item: Item) {
  if (props.pending || pickingCode.value) return
  pickingCode.value = item.code
  emit('pick', item.code)
}

// CheckoutView toggles `pending` while the cart/add call is in flight.
// When it flips back to false, clear our local picking marker.
watch(
  () => props.pending,
  (p) => {
    if (!p) pickingCode.value = null
  },
)

const TYPE_FILTERS: { value: TypeFilter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'tool', label: 'Tools' },
  { value: 'consumable', label: 'Consumables' },
]
</script>

<template>
  <AppDialog
    :open="open"
    title="Browse items"
    description="Tap an item to add it to the cart."
    @update:open="emit('update:open', $event)"
  >
    <div class="flex flex-col gap-3">
      <input
        v-model="query"
        type="search"
        placeholder="Search by name or code"
        autocomplete="off"
        class="w-full rounded-lg bg-slate-800 border border-slate-700 px-3 py-3 text-slate-100 text-base"
      />

      <div class="inline-flex rounded-xl overflow-hidden border border-slate-700 self-start">
        <button
          v-for="opt in TYPE_FILTERS"
          :key="opt.value"
          type="button"
          class="px-3 py-2 text-sm transition-colors"
          :class="
            typeFilter === opt.value
              ? 'bg-sky-600 text-white'
              : 'bg-slate-800 text-slate-300 hover:bg-slate-700'
          "
          @click="typeFilter = opt.value"
        >
          {{ opt.label }}
        </button>
      </div>

      <ul
        v-if="items.length > 0"
        class="flex flex-col gap-2 max-h-[55vh] overflow-y-auto -mx-1 px-1"
      >
        <li v-for="item in items" :key="item.id">
          <button
            type="button"
            class="w-full text-left rounded-xl bg-slate-800 hover:bg-slate-700 border border-slate-700 px-4 py-3 flex items-center gap-3 transition-colors disabled:opacity-50"
            :disabled="pending && pickingCode === item.code"
            @click="pick(item)"
          >
            <div class="min-w-0 flex-1">
              <p class="text-base font-medium text-slate-100 truncate">{{ item.name }}</p>
              <p class="text-xs text-slate-400 truncate">
                {{ item.code }}
                <span v-if="item.serial"> · SN {{ item.serial }}</span>
                <span v-if="item.category"> · {{ item.category }}</span>
              </p>
            </div>
            <span
              class="text-xs rounded-full px-2 py-0.5 shrink-0"
              :class="
                item.type === 'tool'
                  ? 'bg-emerald-900/60 text-emerald-200'
                  : 'bg-sky-900/60 text-sky-200'
              "
            >
              {{ item.type }}
            </span>
          </button>
        </li>
      </ul>
      <p
        v-else-if="!loading"
        class="text-center text-slate-500 py-8"
      >
        {{ query ? 'No items match that search.' : 'No items available.' }}
      </p>

      <div class="flex items-center justify-between gap-3 pt-1">
        <span class="text-xs text-slate-500">
          <template v-if="loading">Loading…</template>
          <template v-else>{{ items.length }} shown</template>
        </span>
        <button
          v-if="hasMore && !loading"
          type="button"
          class="text-sm text-sky-400 hover:text-sky-300"
          @click="loadMore"
        >
          Load more
        </button>
      </div>

      <div class="flex justify-end pt-2">
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          @click="emit('update:open', false)"
        >
          Done
        </button>
      </div>
    </div>
  </AppDialog>
</template>
