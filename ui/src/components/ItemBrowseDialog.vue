<!-- ItemBrowseDialog lets a worker search the catalog and tap items to add
     them to the active cart. Used when a scan isn't practical (torn label,
     no barcode, consumable picked up by hand). Stays open after each add so
     the worker can grab several items in a row without retyping a query.

     Note: the kiosk's window-level scan listener pauses while an <input> in
     this dialog is focused (see useScan), so scanning works after closing. -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
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
const pickingCode = ref<string | null>(null)

let searchSeq = 0
let debounceHandle: ReturnType<typeof setTimeout> | null = null

async function load() {
  const seq = ++searchSeq
  loading.value = true
  try {
    const params = new URLSearchParams()
    if (query.value.trim()) params.set('q', query.value.trim())
    if (typeFilter.value !== 'all') params.set('type', typeFilter.value)
    const qs = params.toString()
    const res = await api.get<{ items: Item[] }>(
      qs ? `/api/kiosk/items?${qs}` : `/api/kiosk/items`,
    )
    if (seq !== searchSeq) return // a newer search superseded us
    items.value = res.items
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
    void load()
  },
)

watch([query, typeFilter], () => {
  if (!props.open) return
  if (debounceHandle) clearTimeout(debounceHandle)
  debounceHandle = setTimeout(() => void load(), 180)
})

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

const UNCATEGORIZED = 'Uncategorized'

interface ItemGroup {
  label: string | null // null = no header (search results, flat)
  items: Item[]
}

// Group items by category when browsing the full catalog. When the user is
// searching, a flat ranked list reads better than tiny groups of 1-2 items.
const groups = computed<ItemGroup[]>(() => {
  if (query.value.trim()) {
    return [{ label: null, items: items.value }]
  }
  const byCat = new Map<string, Item[]>()
  for (const it of items.value) {
    const cat = it.category?.trim() || UNCATEGORIZED
    let arr = byCat.get(cat)
    if (!arr) {
      arr = []
      byCat.set(cat, arr)
    }
    arr.push(it)
  }
  // Alphabetical category order, with UNCATEGORIZED pinned last.
  const cats = Array.from(byCat.keys()).sort((a, b) => {
    if (a === UNCATEGORIZED) return 1
    if (b === UNCATEGORIZED) return -1
    return a.localeCompare(b)
  })
  return cats.map((label) => ({ label, items: byCat.get(label)! }))
})

// Matches IdentifyPanel + AdminItemsView.availableFor: tools track a fleet
// count in quantity_on_hand (unchanged on checkout), so availability is the
// fleet minus what's currently out. Consumables decrement quantity_on_hand
// on consume, so it already reflects what's left.
function availableFor(item: Item): number {
  if (item.type === 'tool') return Math.max(0, item.quantity_on_hand - item.open_count)
  return item.quantity_on_hand
}

function isSerialized(item: Item): boolean {
  return item.tracking_mode === 'serialized'
}
</script>

<template>
  <AppDialog
    :open="open"
    size="xl"
    title="Browse items"
    description="Tap an item to add it to the cart."
    @update:open="emit('update:open', $event)"
  >
    <!-- flex-1 min-h-0 lets the items list inside use overflow-y-auto without
         the dialog also showing its outer scrollbar. AppDialog's DialogContent
         is flex-col + max-h-[90vh], so this fills the available dialog height
         and the items grid claims what's left after the search / filter chrome. -->
    <div class="flex-1 min-h-0 flex flex-col gap-3">
      <input
        v-model="query"
        type="search"
        placeholder="Search by name, code, category, or notes"
        autocomplete="off"
        class="w-full rounded-lg bg-slate-800 border border-slate-700 px-4 py-4 text-slate-100 text-lg shrink-0"
      />

      <div class="inline-flex rounded-xl overflow-hidden border border-slate-700 self-start shrink-0">
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

      <div
        v-if="items.length > 0"
        class="flex-1 min-h-0 overflow-y-auto -mx-1 px-1"
      >
        <template v-for="(group, gi) in groups" :key="group.label ?? `flat-${gi}`">
          <div
            v-if="group.label"
            class="sticky top-0 z-10 bg-slate-900 -mx-1 px-1 py-2 text-xs uppercase tracking-wider text-slate-400 border-b border-slate-800"
          >
            {{ group.label }}
            <span class="ml-2 text-slate-500 normal-case tracking-normal">
              {{ group.items.length }}
            </span>
          </div>
          <ul
            class="grid grid-cols-2 gap-3"
            :class="group.label ? 'mt-3 mb-4' : ''"
          >
            <li v-for="item in group.items" :key="item.id">
              <button
                type="button"
                class="w-full h-full text-left rounded-2xl bg-slate-800 hover:bg-slate-700 border border-slate-700 p-4 flex flex-col gap-2 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                :disabled="(pending && pickingCode === item.code) || isSerialized(item)"
                :title="isSerialized(item) ? 'Scan the unit barcode — serialized items need a specific instance' : ''"
                @click="pick(item)"
              >
                <p class="text-base font-medium text-slate-100 line-clamp-2">{{ item.name }}</p>
                <p class="text-xs text-slate-400 font-mono">{{ item.code }}</p>
                <div class="mt-auto pt-1 flex items-center justify-between gap-2">
                  <span
                    class="text-xs rounded-full px-2 py-0.5"
                    :class="
                      item.type === 'tool'
                        ? 'bg-emerald-900/60 text-emerald-200'
                        : 'bg-sky-900/60 text-sky-200'
                    "
                  >
                    {{ item.type }}
                  </span>
                  <span
                    v-if="isSerialized(item)"
                    class="text-xs text-amber-300"
                  >
                    scan unit barcode
                  </span>
                  <span
                    v-else
                    class="text-xs tabular-nums"
                    :class="availableFor(item) > 0 ? 'text-emerald-300' : 'text-slate-500'"
                  >
                    {{ availableFor(item) }}
                    {{ item.type === 'tool' ? 'available' : 'in stock' }}
                  </span>
                </div>
              </button>
            </li>
          </ul>
        </template>
      </div>
      <p
        v-else-if="!loading"
        class="flex-1 min-h-0 text-center text-slate-500 py-8"
      >
        {{ query ? 'No items match that search.' : 'No items available.' }}
      </p>
      <p
        v-else
        class="flex-1 min-h-0 text-center text-slate-500 py-8"
      >
        Loading…
      </p>

      <div class="flex justify-end pt-2 shrink-0">
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
