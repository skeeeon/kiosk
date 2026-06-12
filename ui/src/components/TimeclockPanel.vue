<!-- TimeclockPanel is the splash-screen time clock. Opened by the "Time
     clock" button on CheckoutView's splash; the view routes badge scans
     here (via the userCode prop) while the panel is open, so the worker
     flow is: tap Time clock → scan badge → one big Clock in/out button.

     Punch conflicts are doors, not walls: a clock-out blocked by open
     tools renders the tool list (from the 409 payload) with guidance,
     and the foreman affordance opens the crew punch dialog. The panel
     auto-closes a few seconds after a successful punch — kiosk-idiomatic,
     ready for the next worker. -->
<script setup lang="ts">
import { onScopeDispose, ref, watch } from 'vue'
import TimeclockCrewDialog from './TimeclockCrewDialog.vue'
import { api, ApiError } from '../lib/api'
import type { OpenCheckoutDetail, PunchConflict, PunchResult, PunchStatus } from '../types'

const props = defineProps<{
  userCode: string | null
}>()

const emit = defineEmits<{
  close: []
}>()

const status = ref<PunchStatus | null>(null)
const loading = ref(false)
const punching = ref(false)
const errorMsg = ref('')
const blockedRows = ref<OpenCheckoutDetail[]>([])
const lastPunch = ref<PunchResult | null>(null)
const crewOpen = ref(false)

// Auto-close keeps a walked-away panel from confusing the next worker:
// 60s idle, shortened to 5s after a successful punch. Any state change
// re-arms the idle timer.
const IDLE_CLOSE_MS = 60_000
const SUCCESS_CLOSE_MS = 5_000
let closeHandle: ReturnType<typeof setTimeout> | null = null
function armClose(ms: number) {
  if (closeHandle) clearTimeout(closeHandle)
  closeHandle = setTimeout(() => emit('close'), ms)
}
armClose(IDLE_CLOSE_MS)
onScopeDispose(() => {
  if (closeHandle) clearTimeout(closeHandle)
})

watch(
  () => props.userCode,
  (code) => {
    if (code) void loadStatus(code)
  },
  { immediate: true },
)

async function loadStatus(code: string) {
  loading.value = true
  errorMsg.value = ''
  blockedRows.value = []
  lastPunch.value = null
  armClose(IDLE_CLOSE_MS)
  try {
    status.value = await api.get<PunchStatus>(
      `/api/kiosk/timeclock/status?user_code=${encodeURIComponent(code)}`,
    )
  } catch (e) {
    status.value = null
    errorMsg.value = e instanceof ApiError ? e.message : (e as Error).message
  } finally {
    loading.value = false
  }
}

async function punch(direction: 'in' | 'out') {
  if (!status.value || punching.value) return
  punching.value = true
  errorMsg.value = ''
  blockedRows.value = []
  try {
    const res = await api.post<PunchResult>('/api/kiosk/timeclock/punch', {
      user_code: status.value.user_code,
      direction,
    })
    lastPunch.value = res
    status.value = { ...status.value, clocked_in: res.clocked_in, since: res.occurred_at }
    armClose(SUCCESS_CLOSE_MS)
  } catch (e) {
    armClose(IDLE_CLOSE_MS)
    if (e instanceof ApiError && e.status === 409) {
      const data = e.data as PunchConflict | null
      if (data?.error === 'open_checkouts') {
        blockedRows.value = data.open_checkouts ?? []
        errorMsg.value = 'Return these tools before clocking out.'
        return
      }
      // already_clocked_in / not_clocked_in — stale panel state (e.g. the
      // worker punched at another kiosk). Refresh so the button matches.
      errorMsg.value = data?.message ?? e.message
      if (status.value) void loadStatus(status.value.user_code)
      return
    }
    errorMsg.value = e instanceof ApiError ? e.message : (e as Error).message
  } finally {
    punching.value = false
  }
}

function formatClock(iso?: string): string {
  if (!iso) return ''
  const t = new Date(iso)
  if (!Number.isFinite(t.getTime())) return ''
  return t.toLocaleString([], { weekday: 'short', hour: 'numeric', minute: '2-digit' })
}
</script>

<template>
  <div class="w-full max-w-2xl rounded-2xl bg-slate-900 border border-slate-800 px-8 py-10 flex flex-col items-center gap-8 text-center">
    <p class="text-3xl font-bold tracking-tight">Time clock</p>

    <!-- Waiting for a badge. -->
    <template v-if="!status && !loading">
      <p class="text-2xl text-slate-300">Scan your badge to clock in or out</p>
      <p v-if="errorMsg" class="rounded-lg bg-red-900/40 border border-red-700/60 text-red-200 text-base px-4 py-2">
        {{ errorMsg }}
      </p>
    </template>

    <p v-else-if="loading" class="text-xl text-slate-400">Loading…</p>

    <template v-else-if="status">
      <div>
        <p class="text-3xl text-slate-100 font-semibold">{{ status.user_name }}</p>
        <p class="text-sm text-slate-500 font-mono">{{ status.user_code }}</p>
      </div>

      <!-- Post-punch confirmation; panel auto-closes shortly after. -->
      <div v-if="lastPunch" class="flex flex-col items-center gap-2">
        <p
          class="text-4xl font-bold"
          :class="lastPunch.direction === 'in' ? 'text-emerald-400' : 'text-amber-400'"
        >
          {{ lastPunch.direction === 'in' ? 'Clocked in' : 'Clocked out' }}
        </p>
        <p class="text-slate-400">{{ formatClock(lastPunch.occurred_at) }}</p>
      </div>

      <template v-else>
        <p class="text-xl" :class="status.clocked_in ? 'text-emerald-300' : 'text-slate-400'">
          <template v-if="status.clocked_in">
            Clocked in since {{ formatClock(status.since) }}
            <span v-if="status.origin === 'fleet'" class="text-slate-500 text-base">(at another kiosk)</span>
          </template>
          <template v-else>Not clocked in</template>
        </p>

        <p
          v-if="errorMsg"
          class="rounded-lg bg-red-900/40 border border-red-700/60 text-red-200 text-base px-4 py-2"
        >
          {{ errorMsg }}
        </p>

        <!-- Blocked clock-out: list the open tools so the worker knows
             exactly what to return. -->
        <ul
          v-if="blockedRows.length > 0"
          class="w-full max-w-md rounded-xl bg-slate-800/60 border border-slate-700/60 divide-y divide-slate-800 text-left"
        >
          <li v-for="row in blockedRows" :key="row.id" class="px-4 py-2.5">
            <div class="text-slate-100 truncate">{{ row.item_name }}</div>
            <div class="text-xs text-slate-500 font-mono">
              {{ row.item_instance_code || row.item_code }}<span v-if="row.instance_serial"> · SN {{ row.instance_serial }}</span>
            </div>
          </li>
        </ul>

        <button
          type="button"
          class="px-12 py-5 rounded-xl text-white text-2xl font-semibold transition-transform active:scale-95 disabled:bg-slate-700 disabled:text-slate-500"
          :class="status.clocked_in ? 'bg-amber-700/90 hover:bg-amber-700' : 'bg-emerald-700/90 hover:bg-emerald-700'"
          :disabled="punching"
          @click="punch(status.clocked_in ? 'out' : 'in')"
        >
          <template v-if="punching">Punching…</template>
          <template v-else>{{ status.clocked_in ? 'Clock out' : 'Clock in' }}</template>
        </button>

        <button
          v-if="status.user_role === 'foreman'"
          type="button"
          class="px-6 py-3 rounded-xl bg-slate-800 hover:bg-slate-700 text-slate-200 text-base transition-transform active:scale-95"
          @click="crewOpen = true"
        >
          Punch a crew member…
        </button>
      </template>
    </template>

    <button
      type="button"
      class="px-8 py-3 rounded-xl bg-slate-800 hover:bg-slate-700 text-slate-200 text-base transition-transform active:scale-95"
      @click="emit('close')"
    >
      Close
    </button>

    <TimeclockCrewDialog
      v-if="status"
      :open="crewOpen"
      :user-code="status.user_code"
      @update:open="crewOpen = $event"
    />
  </div>
</template>
