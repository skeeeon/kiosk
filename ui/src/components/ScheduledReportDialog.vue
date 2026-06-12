<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import { pb } from '../lib/pb'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import type { ScheduledReportRecord } from '../types'

const props = defineProps<{
  open: boolean
  report: Partial<ScheduledReportRecord> | null
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  save: [data: Partial<ScheduledReportRecord>]
  'save-and-add-another': [data: Partial<ScheduledReportRecord>]
}>()

const { identity } = useKioskIdentity()
const isController = computed(() => identity.value?.role === 'controller')

// Kiosks list for the controller-only scope dropdown. Loaded once on
// mount; the dialog opens/closes against the same module-level cache.
const kiosks = ref<Array<{ kiosk_code: string }>>([])
async function loadKiosks() {
  if (!isController.value) return
  try {
    kiosks.value = await pb.collection('kiosks').getFullList<{ kiosk_code: string }>({
      sort: '+kiosk_code',
    })
  } catch {
    kiosks.value = []
  }
}
onMounted(loadKiosks)
watch(isController, loadKiosks)

// Working draft. Two pieces of state outside it: extrasText (textarea →
// string[] conversion) and validation error string for inline display.
const form = reactive<Partial<ScheduledReportRecord>>({
  report_key: 'open_checkouts',
  cadence: 'daily',
  hour: 8,
  weekday: 1,
  day_of_month: 1,
  enabled: true,
  recipients: { worker_email: false, all_admins: true, extras: [] },
  subject_override: '',
  kiosk_code: '',
})

// Local textarea text for recipients.extras — same pattern as
// AdminNotificationsView. Mirrored into the saved payload at submit time.
const extrasText = reactive({ value: '' })

const error = reactive({ message: '' })

const initialSnapshot = ref('')

watch(
  () => [props.open, props.report] as const,
  ([open]) => {
    if (!open) return
    Object.assign(form, {
      report_key: 'open_checkouts',
      cadence: 'daily',
      hour: 8,
      weekday: 1,
      day_of_month: 1,
      enabled: true,
      recipients: { worker_email: false, all_admins: true, extras: [] },
      subject_override: '',
      kiosk_code: '',
      ...(props.report ?? {}),
    })
    // Defensive: the prop may have a partial recipients object.
    form.recipients = {
      worker_email: form.recipients?.worker_email ?? false,
      all_admins: form.recipients?.all_admins ?? true,
      extras: form.recipients?.extras ?? [],
    }
    extrasText.value = (form.recipients.extras ?? []).join('\n')
    error.message = ''
    initialSnapshot.value = JSON.stringify({ form, extras: extrasText.value })
  },
  { immediate: true },
)

const isEdit = computed(() => !!props.report?.id)
const dirty = computed(
  () => JSON.stringify({ form, extras: extrasText.value }) !== initialSnapshot.value,
)

const weekdayOptions = [
  { value: 0, label: 'Sunday' },
  { value: 1, label: 'Monday' },
  { value: 2, label: 'Tuesday' },
  { value: 3, label: 'Wednesday' },
  { value: 4, label: 'Thursday' },
  { value: 5, label: 'Friday' },
  { value: 6, label: 'Saturday' },
]

const hourOptions = computed(() => {
  return Array.from({ length: 24 }, (_, i) => ({
    value: i,
    label: i === 0 ? '12 AM' : i < 12 ? `${i} AM` : i === 12 ? '12 PM' : `${i - 12} PM`,
  }))
})

function parseExtras(text: string): string[] {
  return text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter((s) => s.length > 0)
}

function isValidEmail(addr: string): boolean {
  // Loose check; the server runs net/mail.ParseAddress for the strict pass.
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(addr)
}

function buildPayload(): Partial<ScheduledReportRecord> | null {
  const extras = parseExtras(extrasText.value)
  for (const e of extras) {
    if (!isValidEmail(e)) {
      error.message = `Not a valid email: ${e}`
      return null
    }
  }
  if (!form.recipients?.worker_email && !form.recipients?.all_admins && extras.length === 0) {
    error.message = 'At least one recipient is required.'
    return null
  }
  return {
    ...form,
    recipients: {
      worker_email: false, // digests don't have a worker context
      all_admins: form.recipients?.all_admins ?? false,
      extras,
    },
  }
}

function onSubmit() {
  const payload = buildPayload()
  if (payload) emit('save', payload)
}

function onSubmitAndAdd() {
  const payload = buildPayload()
  if (payload) emit('save-and-add-another', payload)
}
</script>

<template>
  <AppDialog
    :open="open"
    variant="sheet"
    :title="isEdit ? 'Edit scheduled report' : 'New scheduled report'"
    description="Reports email a digest of the named query on the schedule you pick. Add or remove rows here at any time — the scheduler updates without a kiosk restart."
    size="lg"
    confirm-discard
    :dirty="dirty"
    @update:open="emit('update:open', $event)"
  >
    <form class="flex flex-col gap-4" @submit.prevent="onSubmit">
      <div class="grid grid-cols-2 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Report</span>
          <select
            v-model="form.report_key"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          >
            <option value="open_checkouts">Currently checked out</option>
            <option value="daily_activity">Daily activity</option>
            <option value="maintenance">Items in maintenance</option>
            <option value="timeclock">Timeclock</option>
          </select>
        </label>
        <label class="flex items-center gap-2 text-slate-300 mt-6">
          <input v-model="form.enabled" type="checkbox" class="w-4 h-4" />
          Enabled
        </label>
      </div>

      <div class="grid grid-cols-2 gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Cadence</span>
          <select
            v-model="form.cadence"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          >
            <option value="daily">Daily</option>
            <option value="weekly">Weekly</option>
            <option value="monthly">Monthly</option>
          </select>
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-sm text-slate-400">Hour</span>
          <select
            v-model.number="form.hour"
            class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          >
            <option v-for="h in hourOptions" :key="h.value" :value="h.value">{{ h.label }}</option>
          </select>
        </label>
      </div>

      <label v-if="form.cadence === 'weekly'" class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Day of week</span>
        <select
          v-model.number="form.weekday"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
        >
          <option v-for="d in weekdayOptions" :key="d.value" :value="d.value">{{ d.label }}</option>
        </select>
      </label>

      <label v-if="form.cadence === 'monthly'" class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Day of month (1–28)</span>
        <input
          v-model.number="form.day_of_month"
          type="number"
          min="1"
          max="28"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
        />
        <span class="text-xs text-slate-500">Capped at 28 so the schedule fires every month including February.</span>
      </label>

      <label v-if="isController" class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Scope</span>
        <select
          v-model="form.kiosk_code"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
        >
          <option value="">Fleet-wide (every kiosk)</option>
          <option v-for="k in kiosks" :key="k.kiosk_code" :value="k.kiosk_code">{{ k.kiosk_code }}</option>
        </select>
        <span class="text-xs text-slate-500">Empty rolls every kiosk into one report; a specific kiosk scopes the data to just that one.</span>
      </label>

      <fieldset class="rounded-lg border border-slate-700 p-3">
        <legend class="px-1 text-sm text-slate-400">Recipients</legend>
        <label class="flex items-start gap-2 text-sm text-slate-300 mb-2">
          <input v-model="form.recipients!.all_admins" type="checkbox" class="mt-1" />
          Send to every active admin
        </label>
        <label class="block text-sm text-slate-300 mt-3">
          <span class="block text-slate-400 mb-1">Additional recipients</span>
          <textarea
            v-model="extrasText.value"
            rows="3"
            placeholder="One email per line — commas also work."
            class="w-full rounded-lg bg-slate-950 border border-slate-800 px-3 py-2 text-slate-100 text-sm"
          ></textarea>
        </label>
      </fieldset>

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Subject override <span class="text-slate-500">(optional)</span></span>
        <input
          v-model="form.subject_override"
          type="text"
          placeholder="Leave blank to use the template subject"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
        />
      </label>

      <p v-if="error.message" class="rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 text-sm">
        {{ error.message }}
      </p>

      <div class="flex justify-end gap-3 mt-2">
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          @click="emit('update:open', false)"
        >
          Cancel
        </button>
        <button
          v-if="!isEdit"
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-700 hover:bg-slate-600 text-slate-100 text-sm font-medium"
          @click="onSubmitAndAdd"
        >
          Save &amp; add another
        </button>
        <button
          type="submit"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
        >
          {{ isEdit ? 'Save changes' : 'Create schedule' }}
        </button>
      </div>
    </form>
  </AppDialog>
</template>
