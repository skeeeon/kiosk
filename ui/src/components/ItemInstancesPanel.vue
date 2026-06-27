<!-- ItemInstancesPanel lists and edits per-unit instances of a serialized
     item. One instance row = one physical thing in the world. Embedded inside
     ItemDialog when the open item has tracking_mode='serialized' and already
     exists (we need an item id to FK against).

     Lifecycle is a status enum (in_service / maintenance / retired), never a
     hard delete — units carry transaction history the ledger keeps alive.
     Status transitions go through a PB update of the `status` field; the
     OnRecordUpdateRequest hook audits the change and uses the unit's `notes`
     as the reason, so the transition dialog writes the reason there. -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { pb } from '../lib/pb'
import { useToast } from '../composables/useToast'
import type { ItemInstance } from '../types'
import {
  instanceActions,
  statusLabel,
  statusBadgeClass,
  actionButtonClass,
  type InstanceStatus,
} from '../lib/instanceStatus'

const props = defineProps<{ itemId: string }>()

const toast = useToast()

interface InstanceRow extends ItemInstance {
  out: boolean // true when at least one open_checkouts row references this instance
}

const rows = ref<InstanceRow[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

// Draft row for "new instance" inline form. Null when not adding.
const draft = ref<Partial<ItemInstance> | null>(null)
const editingId = ref<string | null>(null)
const editingDraft = ref<{ code?: string; serial?: string; rfid_epc?: string; notes?: string; enclosure_id?: string } | null>(null)

async function load() {
  loading.value = true
  error.value = null
  try {
    const [instRes, openRes] = await Promise.all([
      pb.collection('item_instances').getFullList<ItemInstance>({
        filter: pb.filter('item = {:item}', { item: props.itemId }),
        sort: '+code',
      }),
      pb.collection('open_checkouts').getFullList<{ item_instance: string }>({
        filter: pb.filter('item = {:item}', { item: props.itemId }),
        fields: 'item_instance',
      }),
    ])
    const outIds = new Set(openRes.map((o) => o.item_instance).filter(Boolean))
    rows.value = instRes.map((i) => ({ ...i, out: outIds.has(i.id) }))
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

watch(() => props.itemId, (id) => { if (id) void load() }, { immediate: true })

function startAdd() {
  draft.value = { code: '', serial: '', rfid_epc: '', status: 'in_service', notes: '', enclosure_id: '' }
}

async function saveDraft() {
  if (!draft.value?.code) {
    toast.error('Instance code is required')
    return
  }
  try {
    await pb.collection('item_instances').create({
      ...draft.value,
      item: props.itemId,
    })
    draft.value = null
    await load()
    toast.success('Instance added')
  } catch (e) {
    toast.error((e as Error).message)
  }
}

function startEdit(row: InstanceRow) {
  editingId.value = row.id
  // Cosmetic fields only — status changes go through the lifecycle actions, not
  // this form (sending status here would risk an unintended transition).
  editingDraft.value = {
    code: row.code,
    serial: row.serial,
    rfid_epc: row.rfid_epc,
    notes: row.notes,
    enclosure_id: row.enclosure_id,
  }
}

async function saveEdit() {
  if (!editingId.value || !editingDraft.value) return
  try {
    await pb.collection('item_instances').update(editingId.value, editingDraft.value)
    editingId.value = null
    editingDraft.value = null
    await load()
    toast.success('Instance updated')
  } catch (e) {
    toast.error((e as Error).message)
  }
}

// changeStatus transitions a unit to the target status. The reason is required
// and lands in the unit's `notes` field, which the audit hook reads as the
// transition reason. Pre-filled with current notes so the admin sees what
// they're replacing. Cancelling the prompt aborts.
async function changeStatus(row: InstanceRow, target: InstanceStatus) {
  const reason = window.prompt(
    `Reason for "${statusLabel(target)}" on ${row.code} (saved to the unit's notes):`,
    row.notes ?? '',
  )
  if (reason === null) return
  if (!reason.trim()) {
    toast.error('A reason is required')
    return
  }
  try {
    await pb.collection('item_instances').update(row.id, {
      status: target,
      notes: reason.trim(),
    })
    await load()
    toast.success(`${row.code} → ${statusLabel(target)}`)
  } catch (e) {
    toast.error((e as Error).message)
  }
}

const hasRows = computed(() => rows.value.length > 0)

// The "Last seen" column is advisory location (docs/location-sightings-plan.md):
// shown only when at least one unit has been observed, so a node with no
// gateways / no reader zone configured never sees the column (N=1 invisible).
const hasLastSeen = computed(() => rows.value.some((r) => !!r.last_observed_at))

function relativeAge(iso: string): string {
  const t = new Date(iso).getTime()
  if (!Number.isFinite(t)) return ''
  const diffMs = Date.now() - t
  if (diffMs < 60_000) return 'just now'
  const minutes = Math.floor(diffMs / 60_000)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}
</script>

<template>
  <section class="rounded-xl bg-slate-950/40 border border-slate-800 p-4">
    <header class="flex items-center justify-between mb-3">
      <div>
        <h3 class="text-sm font-medium text-slate-200">Instances</h3>
        <p class="text-xs text-slate-500">One row per physical unit of this SKU.</p>
      </div>
      <button
        v-if="!draft"
        type="button"
        class="text-sm px-3 py-1.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
        @click="startAdd"
      >
        + Add instance
      </button>
    </header>

    <p v-if="error" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 text-sm px-3 py-2 mb-2">
      {{ error }}
    </p>

    <table class="w-full text-left text-xs">
      <thead class="text-slate-500">
        <tr>
          <th class="px-2 py-2 font-medium">Code</th>
          <th class="px-2 py-2 font-medium">Serial</th>
          <th class="px-2 py-2 font-medium">RFID</th>
          <th class="px-2 py-2 font-medium">Enclosure</th>
          <th class="px-2 py-2 font-medium">Status</th>
          <th class="px-2 py-2 font-medium">Out?</th>
          <th v-if="hasLastSeen" class="px-2 py-2 font-medium">Last seen</th>
          <th class="px-2 py-2"></th>
        </tr>
      </thead>
      <tbody class="divide-y divide-slate-800">
        <tr v-if="loading">
          <td :colspan="hasLastSeen ? 8 : 7" class="text-center text-slate-500 py-3">Loading…</td>
        </tr>
        <tr v-else-if="!hasRows && !draft">
          <td :colspan="hasLastSeen ? 8 : 7" class="text-center text-slate-500 py-3">
            No instances yet. Add one to enable scanning.
          </td>
        </tr>

        <template v-for="row in rows" :key="row.id">
          <tr v-if="editingId !== row.id" class="hover:bg-slate-900/50" :class="row.status === 'retired' ? 'text-slate-500' : ''">
            <td class="px-2 py-2 font-mono text-slate-200">{{ row.code }}</td>
            <td class="px-2 py-2 font-mono text-slate-400">{{ row.serial || '—' }}</td>
            <td class="px-2 py-2 font-mono text-slate-400 truncate max-w-[10rem]">{{ row.rfid_epc || '—' }}</td>
            <td class="px-2 py-2 font-mono text-slate-400">{{ row.enclosure_id || '—' }}</td>
            <td class="px-2 py-2">
              <span
                class="inline-block px-2 py-0.5 rounded text-[10px]"
                :class="statusBadgeClass(row.status)"
              >{{ statusLabel(row.status) }}</span>
            </td>
            <td class="px-2 py-2">
              <span
                v-if="row.out"
                class="inline-block px-2 py-0.5 rounded text-[10px] bg-amber-900/60 text-amber-200"
              >currently out</span>
              <span v-else class="text-slate-500 text-[10px]">in</span>
            </td>
            <td v-if="hasLastSeen" class="px-2 py-2 text-slate-400">
              <template v-if="row.last_observed_at">
                <span class="text-slate-300">{{ row.last_observed_zone || '—' }}</span>
                <span class="text-slate-500" :title="row.last_observed_at"> · {{ relativeAge(row.last_observed_at) }}</span>
              </template>
              <span v-else class="text-slate-600">—</span>
            </td>
            <td class="px-2 py-2 text-right whitespace-nowrap">
              <div class="inline-flex flex-wrap justify-end gap-1">
                <button
                  type="button"
                  class="text-sky-400 hover:text-sky-300 px-1"
                  @click="startEdit(row)"
                >Edit</button>
                <button
                  v-for="a in instanceActions(row.status)"
                  :key="a.target"
                  type="button"
                  class="px-2 py-0.5 rounded border text-[11px]"
                  :class="actionButtonClass(a.tone)"
                  @click="changeStatus(row, a.target)"
                >{{ a.label }}</button>
              </div>
            </td>
          </tr>
          <tr v-else class="bg-slate-900/70">
            <td class="px-2 py-2">
              <input
                v-model="editingDraft!.code"
                type="text"
                class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm"
              />
            </td>
            <td class="px-2 py-2">
              <input v-model="editingDraft!.serial" type="text" class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm" />
            </td>
            <td class="px-2 py-2">
              <input v-model="editingDraft!.rfid_epc" type="text" class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm" />
            </td>
            <td class="px-2 py-2">
              <input v-model="editingDraft!.enclosure_id" type="text" placeholder="cabinet" class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm" />
            </td>
            <td class="px-2 py-2 text-slate-500 text-[10px]" :colspan="hasLastSeen ? 3 : 2">
              Status changes via the row actions.
            </td>
            <td class="px-2 py-2 text-right whitespace-nowrap">
              <button
                type="button"
                class="text-emerald-400 hover:text-emerald-300 px-1 mr-1"
                @click="saveEdit"
              >Save</button>
              <button
                type="button"
                class="text-slate-400 hover:text-slate-300 px-1"
                @click="editingId = null; editingDraft = null"
              >Cancel</button>
            </td>
          </tr>
        </template>

        <tr v-if="draft" class="bg-slate-900/70">
          <td class="px-2 py-2">
            <input
              v-model="draft.code"
              type="text"
              placeholder="DR-042-A"
              class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm"
            />
          </td>
          <td class="px-2 py-2">
            <input v-model="draft.serial" type="text" class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm" />
          </td>
          <td class="px-2 py-2">
            <input v-model="draft.rfid_epc" type="text" class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm" />
          </td>
          <td class="px-2 py-2">
            <input v-model="draft.enclosure_id" type="text" placeholder="cabinet" class="w-full rounded bg-slate-800 border border-slate-700 px-2 py-1 text-sm" />
          </td>
          <td class="px-2 py-2 text-slate-400 text-[10px]" :colspan="hasLastSeen ? 3 : 2">new — in service</td>
          <td class="px-2 py-2 text-right whitespace-nowrap">
            <button
              type="button"
              class="text-emerald-400 hover:text-emerald-300 px-1 mr-1"
              @click="saveDraft"
            >Save</button>
            <button
              type="button"
              class="text-slate-400 hover:text-slate-300 px-1"
              @click="draft = null"
            >Cancel</button>
          </td>
        </tr>
      </tbody>
    </table>
  </section>
</template>
