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

// Single = one directional punch (optionally backdated). Shift = a full
// in→out correction recorded as two backdated punches in one submit — the
// common "worker forgot to clock out" fix.
const mode = ref<'single' | 'shift'>('single')

const userCode = ref('')
const direction = ref<'in' | 'out'>('in')
const reason = ref('')
const occurredAt = ref('') // single mode; datetime-local; empty = now
const startAt = ref('') // shift mode; datetime-local, required
const endAt = ref('') // shift mode; datetime-local, required
const force = ref(false)
const kioskCode = ref('')
const submitting = ref(false)
const errorMsg = ref('')

watch(
  () => props.open,
  (open) => {
    if (!open) return
    mode.value = 'single'
    userCode.value = ''
    direction.value = 'in'
    reason.value = ''
    occurredAt.value = ''
    startAt.value = ''
    endAt.value = ''
    force.value = false
    kioskCode.value = props.defaultKioskCode ?? ''
    errorMsg.value = ''
  },
)

const canSubmit = computed(() => {
  if (submitting.value) return false
  if (userCode.value.trim() === '' || reason.value.trim() === '') return false
  if (props.isController && kioskCode.value === '') return false
  if (mode.value === 'shift') return startAt.value !== '' && endAt.value !== ''
  return true
})

const path = computed(() =>
  props.isController
    ? `/api/controller/kiosks/${encodeURIComponent(kioskCode.value)}/timeclock/punch`
    : '/api/kiosk/timeclock/admin-punch',
)

// datetime-local (local wall time) → UTC ISO, or null if unparseable.
function toISO(local: string): string | null {
  const t = new Date(local)
  return Number.isFinite(t.getTime()) ? t.toISOString() : null
}

function describeErr(e: unknown): string {
  if (isKioskOfflineError(e)) {
    return `Kiosk ${kioskCode.value} is offline — manual punches need the kiosk reachable.`
  }
  if (e instanceof ApiError && e.status === 409) {
    return (e.data as PunchConflict | null)?.message ?? e.message
  }
  return e instanceof ApiError ? e.message : (e as Error).message
}

async function postPunch(dir: 'in' | 'out', occurredISO: string | null, useForce: boolean) {
  const body: Record<string, unknown> = {
    user_code: userCode.value.trim(),
    direction: dir,
    reason: reason.value.trim(),
    force: useForce,
  }
  if (occurredISO) body.occurred_at = occurredISO
  return api.post<PunchResult>(path.value, body)
}

async function submit() {
  if (!canSubmit.value) return
  submitting.value = true
  errorMsg.value = ''
  try {
    if (mode.value === 'shift') {
      const startISO = toISO(startAt.value)
      const endISO = toISO(endAt.value)
      if (!startISO || !endISO) {
        errorMsg.value = 'Invalid date/time.'
        return
      }
      if (new Date(endISO) <= new Date(startISO)) {
        errorMsg.value = 'End must be after start.'
        return
      }
      // Record the clock-in first; only if it lands do we add the matching
      // clock-out. Admin punches bypass alternation, so ordering is safe.
      await postPunch('in', startISO, false)
      let outRes: PunchResult
      try {
        // Force applies to the clock-out (the direction the open-tools block
        // can reject).
        outRes = await postPunch('out', endISO, force.value)
      } catch (e) {
        // The in-punch already committed; surface the partial state so the
        // admin finishes with a single clock-out rather than re-adding both.
        errorMsg.value = `Clock-in recorded, but the clock-out failed: ${describeErr(e)} Add the clock-out as a single punch.`
        return
      }
      emit('recorded', outRes)
      emit('update:open', false)
      return
    }

    // Single punch.
    let occurredISO: string | null = null
    if (occurredAt.value) {
      occurredISO = toISO(occurredAt.value)
      if (!occurredISO) {
        errorMsg.value = 'Invalid date/time.'
        return
      }
    }
    const res = await postPunch(direction.value, occurredISO, force.value)
    emit('recorded', res)
    emit('update:open', false)
  } catch (e) {
    errorMsg.value = describeErr(e)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <AppDialog
    :open="open"
    title="Record punch"
    :description="mode === 'shift'
      ? 'Record a full shift as an in→out pair. Reason is required.'
      : 'Manual/corrective punch. Reason is required; leave the time empty to punch now.'"
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

      <!-- Single directional punch vs. a full in→out shift correction. -->
      <div class="flex gap-2">
        <button
          type="button"
          class="flex-1 px-4 py-2.5 rounded-lg text-sm font-medium transition-colors"
          :class="mode === 'single' ? 'bg-brand-primary text-white' : 'bg-slate-800 text-slate-300 hover:bg-slate-700'"
          @click="mode = 'single'"
        >
          Single punch
        </button>
        <button
          type="button"
          class="flex-1 px-4 py-2.5 rounded-lg text-sm font-medium transition-colors"
          :class="mode === 'shift' ? 'bg-brand-primary text-white' : 'bg-slate-800 text-slate-300 hover:bg-slate-700'"
          @click="mode = 'shift'"
        >
          Full shift
        </button>
      </div>

      <!-- SINGLE: pick a direction and an optional backdated time. -->
      <template v-if="mode === 'single'">
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
      </template>

      <!-- SHIFT: start + end become two backdated punches (in, then out). -->
      <template v-else>
        <div class="flex gap-3">
          <label class="flex-1 flex flex-col gap-1 text-sm text-slate-300">
            Start (clock in)
            <input
              v-model="startAt"
              type="datetime-local"
              class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
            />
          </label>
          <label class="flex-1 flex flex-col gap-1 text-sm text-slate-300">
            End (clock out)
            <input
              v-model="endAt"
              type="datetime-local"
              class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
            />
          </label>
        </div>
      </template>

      <label class="flex flex-col gap-1 text-sm text-slate-300">
        Reason
        <input
          v-model="reason"
          type="text"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
          placeholder="e.g. forgot to clock out at end of shift"
        />
      </label>

      <!-- Force bypasses the open-tools clock-out block — relevant to a
           clock-out (single mode) or the out leg of a shift. -->
      <label
        v-if="mode === 'shift' || direction === 'out'"
        class="flex items-center gap-2 text-sm text-slate-300"
      >
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
