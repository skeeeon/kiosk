<script setup lang="ts">
import { computed, reactive, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import type { ItemRecord } from '../types'

const props = defineProps<{
  open: boolean
  item: Partial<ItemRecord> | null
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  save: [data: Partial<ItemRecord>]
}>()

const form = reactive<Partial<ItemRecord>>({
  code: '',
  name: '',
  type: 'tool',
  unit: 'each',
  tracking_mode: 'quantity',
  serial: '',
  category: '',
  rfid_epc: '',
  active: true,
  notes: '',
})

watch(
  () => props.open,
  (open) => {
    if (!open) return
    Object.assign(form, {
      code: '',
      name: '',
      type: 'tool',
      unit: 'each',
      tracking_mode: 'quantity',
      serial: '',
      category: '',
      rfid_epc: '',
      active: true,
      notes: '',
      ...(props.item ?? {}),
    })
  },
  { immediate: true },
)

const isEdit = computed(() => !!props.item?.id)
const isSerialized = computed(() => form.tracking_mode === 'serialized')

function onSubmit() {
  emit('save', { ...form })
}
</script>

<template>
  <AppDialog
    :open="open"
    :title="isEdit ? 'Edit item' : 'New item'"
    @update:open="emit('update:open', $event)"
  >
    <form class="flex flex-col gap-4" @submit.prevent="onSubmit">
      <div class="grid grid-cols-2 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Code</span>
          <input
            v-model="form.code"
            type="text"
            required
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Name</span>
          <input
            v-model="form.name"
            type="text"
            required
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
      </div>

      <div class="grid grid-cols-3 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Type</span>
          <select
            v-model="form.type"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          >
            <option value="tool">Tool</option>
            <option value="consumable">Consumable</option>
          </select>
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Tracking</span>
          <select
            v-model="form.tracking_mode"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          >
            <option value="quantity">Quantity</option>
            <option value="serialized">Serialized</option>
          </select>
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Unit</span>
          <input
            v-model="form.unit"
            type="text"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
      </div>

      <label v-if="isSerialized" class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Serial</span>
        <input
          v-model="form.serial"
          type="text"
          required
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
        />
      </label>

      <div class="grid grid-cols-2 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Category</span>
          <input
            v-model="form.category"
            type="text"
            placeholder="e.g. Power Tools"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">RFID EPC</span>
          <input
            v-model="form.rfid_epc"
            type="text"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          />
        </label>
      </div>

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Notes</span>
        <textarea
          v-model="form.notes"
          rows="2"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 resize-none"
        ></textarea>
      </label>

      <label class="flex items-center gap-2 text-slate-300">
        <input v-model="form.active" type="checkbox" class="w-4 h-4" />
        Active
      </label>

      <div class="flex justify-end gap-3 mt-2">
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          @click="emit('update:open', false)"
        >
          Cancel
        </button>
        <button
          type="submit"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        >
          {{ isEdit ? 'Save changes' : 'Create item' }}
        </button>
      </div>
    </form>
  </AppDialog>
</template>
