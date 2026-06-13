<!-- TimeclockPunchDialog records an admin manual/corrective punch:
     backdating (occurred_at) and force clock-out are admin-only powers and
     a reason is always required — the funnel rejects without one. On the
     kiosk it posts to the local admin-punch endpoint; on the controller it
     proxies a timeclock.punch command to the selected kiosk (kiosks are the
     only punch writers). -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import { api, ApiError, isKioskOfflineError } from '../lib/api'
import type { KioskRecord, PunchConflict, PunchResult } from '../types'

const props = defineProps<{
  open: boolean
  isController: boolean
  // Controller-only: pickable target kiosks. Preselected from the reports
  // view's kiosk filter when set.
  kiosks?: KioskRecord[]
  defaultKioskCode?: string
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  recorded: [result: PunchResult]
}>()

const userCode = ref('')
const direction = ref<'in' | 'out'>('in')
const reason = ref('')
const occurredAt = ref('') // datetime-local; empty = now
const force = ref(false)
const kioskCode = ref('')
const submitting = ref(false)
const errorMsg = ref('')

watch(
  () => props.open,
  (open) => {
    if (!open) return
    userCode.value = ''
    direction.value = 'in'
    reason.value = ''
    occurredAt.value = ''
    force.value = false
    kioskCode.value = props.defaultKioskCode ?? ''
    errorMsg.value = ''
  },
)

const canSubmit = computed(
  () =>
    userCode.value.trim() !== '' &&
    reason.value.trim() !== '' &&
    (!props.isController || kioskCode.value !== '') &&
    !submitting.value,
)

async function submit() {
  if (!canSubmit.value) return
  submitting.value = true
  errorMsg.value = ''
  const body: Record<string, unknown> = {
    user_code: userCode.value.trim(),
    direction: direction.value,
    reason: reason.value.trim(),
    force: force.value,
  }
  if (occurredAt.value) {
    const t = new Date(occurredAt.value)
    if (!Number.isFinite(t.getTime())) {
      errorMsg.value = 'Invalid date/time.'
      submitting.value = false
      return
    }
    body.occurred_at = t.toISOString()
  }
  const path = props.isController
    ? `/api/controller/kiosks/${encodeURIComponent(kioskCode.value)}/timeclock/punch`
    : '/api/kiosk/timeclock/admin-punch'
  try {
    const res = await api.post<PunchResult>(path, body)
    emit('recorded', res)
    emit('update:open', false)
  } catch (e) {
    if (isKioskOfflineError(e)) {
      errorMsg.value = `Kiosk ${kioskCode.value} is offline — manual punches need the kiosk reachable.`
    } else if (e instanceof ApiError && e.status === 409) {
      errorMsg.value = (e.data as PunchConflict | null)?.message ?? e.message
    } else {
      errorMsg.value = e instanceof ApiError ? e.message : (e as Error).message
    }
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <AppDialog
    :open="open"
    title="Record punch"
    description="Manual/corrective punch. Reason is required; leave the time empty to punch now."
    size="sm"
    @update:open="emit('update:open', $event)"
  >
    <form class="flex flex-col gap-4" @submit.prevent="submit">
      <label v-if="isController" class="flex flex-col gap-1 text-sm text-slate-300">
        Kiosk
        <select
          v-model="kioskCode"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
        >
          <option value="" disabled>Select a kiosk…</option>
          <option v-for="k in kiosks ?? []" :key="k.id" :value="k.kiosk_code">
            {{ k.kiosk_code }}{{ k.location_code ? ` — ${k.location_code}` : '' }}
          </option>
        </select>
      </label>

      <label class="flex flex-col gap-1 text-sm text-slate-300">
        Worker code
        <input
          v-model="userCode"
          type="text"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100 font-mono"
          autocomplete="off"
          spellcheck="false"
        />
      </label>

      <div class="flex gap-2">
        <button
          type="button"
          class="flex-1 px-4 py-2.5 rounded-lg text-sm font-medium transition-colors"
          :class="direction === 'in' ? 'bg-emerald-700 text-white' : 'bg-slate-800 text-slate-300 hover:bg-slate-700'"
          @click="direction = 'in'"
        >
          Clock in
        </button>
        <button
          type="button"
          class="flex-1 px-4 py-2.5 rounded-lg text-sm font-medium transition-colors"
          :class="direction === 'out' ? 'bg-amber-700 text-white' : 'bg-slate-800 text-slate-300 hover:bg-slate-700'"
          @click="direction = 'out'"
        >
          Clock out
        </button>
      </div>

      <label class="flex flex-col gap-1 text-sm text-slate-300">
        Time (leave empty for now)
        <input
          v-model="occurredAt"
          type="datetime-local"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
        />
      </label>

      <label class="flex flex-col gap-1 text-sm text-slate-300">
        Reason
        <input
          v-model="reason"
          type="text"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          placeholder="e.g. forgot to clock out at end of shift"
        />
      </label>

      <label v-if="direction === 'out'" class="flex items-center gap-2 text-sm text-slate-300">
        <input v-model="force" type="checkbox" class="rounded border-slate-600 bg-slate-800" />
        Force — bypass the open-tools block
      </label>

      <p
        v-if="errorMsg"
        class="rounded-lg bg-red-900/40 border border-red-700/60 text-red-200 text-sm px-3 py-2"
      >
        {{ errorMsg }}
      </p>

      <div class="flex justify-end gap-3 pt-2">
        <button
          type="button"
          class="px-5 py-2.5 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          @click="emit('update:open', false)"
        >
          Cancel
        </button>
        <button
          type="submit"
          class="px-5 py-2.5 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium disabled:bg-slate-700 disabled:text-slate-500"
          :disabled="!canSubmit"
        >
          <template v-if="submitting">Recording…</template>
          <template v-else>Record punch</template>
        </button>
      </div>
    </form>
  </AppDialog>
</template>
