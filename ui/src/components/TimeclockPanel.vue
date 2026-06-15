<!-- TimeclockPanel is the splash-screen time clock. Opened by the "Time
     clock" button on CheckoutView's splash; the view routes badge scans
     here (via the userCode prop) while the panel is open, so the worker
     flow is: tap Time clock → scan badge → one big Clock in/out button.

     Layout is three zones: a header (panel identity + a live wall clock,
     so the worker can see the minute their punch will record), a body
     that renders exactly one state (waiting / loading / status+action /
     post-punch confirmation), and a footer holding the secondary actions
     (foreman crew punch, Close).

     Punch conflicts are doors, not walls: a clock-out blocked by open
     tools renders the tool list (from the 409 payload) with guidance,
     and the foreman affordance opens the crew punch dialog. The panel
     auto-closes a few seconds after a successful punch — kiosk-idiomatic,
     ready for the next worker — with a draining countdown bar matching
     the checkout receipt's; tapping the panel extends it, same as the
     receipt.

     standalone mode (timeclock-only kiosks): the panel IS the splash.
     "Close" becomes "Done", the waiting state is the resting state (no
     idle timer — there's nothing behind the panel to return to), and the
     parent handles the close event by remounting via :key so every reset
     starts from a fresh waiting panel. -->
<script setup lang="ts">
import { computed, onScopeDispose, ref, watch } from 'vue'
import TimeclockCrewDialog from './TimeclockCrewDialog.vue'
import { api, ApiError } from '../lib/api'
import type { OpenCheckoutDetail, PunchConflict, PunchResult, PunchStatus } from '../types'

const props = defineProps<{
  userCode: string | null
  standalone?: boolean
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
// Optional job/work-order tag, supplied on clock-in. Not autofocused — a
// focused input would swallow the window-level badge scan (see useScan).
const jobCode = ref('')

// Live wall clock in the header. A ticking clock is timeclock idiom — it
// tells the worker the kiosk is alive and what time their punch records.
const now = ref(new Date())
const clockHandle = setInterval(() => {
  now.value = new Date()
}, 1000)
const clockTime = computed(() =>
  now.value.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }),
)
const clockDate = computed(() =>
  now.value.toLocaleDateString([], { weekday: 'long', month: 'short', day: 'numeric' }),
)

// "X today" readout. The status payload carries today's CLOSED seconds (this
// kiosk's ledger); the running session is added live off the wall clock so the
// total ticks while clocked in — no extra timer, no double-count (an open
// interval contributes nothing to the server's closed total).
const liveSeconds = computed(() => {
  if (!status.value?.clocked_in || !status.value.since) return 0
  const since = new Date(status.value.since).getTime()
  if (!Number.isFinite(since)) return 0
  return Math.max(0, Math.floor((now.value.getTime() - since) / 1000))
})
const todaySeconds = computed(() => (status.value?.today_seconds ?? 0) + liveSeconds.value)

// Whole-minute readout (e.g. "4h 28m"). Display truncation, NOT payroll
// rounding — the raw-punch CSV stays the contract.
function formatDuration(totalSeconds: number): string {
  const s = Math.max(0, Math.floor(totalSeconds))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  return h > 0 ? `${h}h ${m}m` : `${m}m`
}

// Auto-close keeps a walked-away panel from confusing the next worker:
// 60s idle, shortened to 5s after a successful punch. Any state change
// re-arms the idle timer. The success close draws a draining countdown
// bar (same visual as the receipt screen); the idle close stays
// invisible — a 60s drain would just be noise.
const IDLE_CLOSE_MS = 60_000
const SUCCESS_CLOSE_MS = 5_000
const closeProgress = ref(1)
const showCloseBar = ref(false)
let closeHandle: ReturnType<typeof setTimeout> | null = null
let closeBarHandle: ReturnType<typeof setInterval> | null = null

function stopCloseBar() {
  if (closeBarHandle) {
    clearInterval(closeBarHandle)
    closeBarHandle = null
  }
  showCloseBar.value = false
}

function armClose(ms: number, withBar = false) {
  if (closeHandle) clearTimeout(closeHandle)
  stopCloseBar()
  closeHandle = setTimeout(() => emit('close'), ms)
  if (withBar) {
    showCloseBar.value = true
    closeProgress.value = 1
    const deadline = Date.now() + ms
    closeBarHandle = setInterval(() => {
      closeProgress.value = Math.max(0, (deadline - Date.now()) / ms)
    }, 100)
  }
}
// Standalone panels rest in the waiting state — no timer until a badge
// lands (loadStatus arms one).
if (!props.standalone) armClose(IDLE_CLOSE_MS)

// Tap-to-extend, consistent with the receipt screen: interacting with the
// panel pushes the auto-close deadline back to full.
function onPanelClick() {
  if (crewOpen.value) return
  if (props.standalone && !status.value && !loading.value) return
  armClose(lastPunch.value ? SUCCESS_CLOSE_MS : IDLE_CLOSE_MS, !!lastPunch.value)
}

// The crew dialog suspends auto-close entirely — a foreman punching a long
// crew list must not have the panel (and the dialog with it) torn down
// underneath them at the 60s mark.
watch(crewOpen, (open) => {
  if (open) {
    if (closeHandle) {
      clearTimeout(closeHandle)
      closeHandle = null
    }
    stopCloseBar()
  } else {
    armClose(IDLE_CLOSE_MS)
  }
})

onScopeDispose(() => {
  if (closeHandle) clearTimeout(closeHandle)
  if (closeBarHandle) clearInterval(closeBarHandle)
  clearInterval(clockHandle)
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
  jobCode.value = ''
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

// After a punch the closed-interval total changes (a clock-out just closed a
// session), so re-read today_seconds — without going through loadStatus, which
// would clear the confirmation screen and re-arm the idle close. Best-effort:
// the displayed total just keeps its prior value if this blips.
async function refreshTodaySeconds(code: string) {
  try {
    const s = await api.get<PunchStatus>(
      `/api/kiosk/timeclock/status?user_code=${encodeURIComponent(code)}`,
    )
    if (status.value) status.value = { ...status.value, today_seconds: s.today_seconds }
  } catch {
    // Supplementary readout; ignore.
  }
}

async function punch(direction: 'in' | 'out', acknowledge = false) {
  if (!status.value || punching.value) return
  punching.value = true
  errorMsg.value = ''
  blockedRows.value = []
  try {
    const res = await api.post<PunchResult>('/api/kiosk/timeclock/punch', {
      user_code: status.value.user_code,
      direction,
      // "Clock out anyway" — acknowledges the open tools and bypasses the
      // gate (recorded on the punch as a self/foreman acknowledgment).
      ...(acknowledge ? { acknowledge: true } : {}),
      // Job tag is only meaningful on a clock-in; pairing reads it off the "in".
      ...(direction === 'in' && jobCode.value.trim() ? { job_code: jobCode.value.trim() } : {}),
    })
    lastPunch.value = res
    if (direction === 'in') jobCode.value = ''
    status.value = { ...status.value, clocked_in: res.clocked_in, since: res.occurred_at }
    void refreshTodaySeconds(status.value.user_code)
    armClose(SUCCESS_CLOSE_MS, true)
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
  <div
    class="relative w-full max-w-2xl rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden text-center"
    @click="onPanelClick"
  >
    <!-- Post-punch auto-close countdown — same draining-bar shape as the
         receipt screen, tinted to match the punch direction. -->
    <div
      v-if="showCloseBar"
      class="absolute left-0 top-0 h-1 transition-[width] duration-100 ease-linear"
      :class="lastPunch?.direction === 'out' ? 'bg-amber-500' : 'bg-emerald-500'"
      :style="{ width: `${Math.round(closeProgress * 100)}%` }"
      aria-hidden="true"
    ></div>

    <div class="flex items-center justify-between gap-4 px-6 py-4 border-b border-slate-800 text-left">
      <p class="text-2xl font-bold tracking-tight">Time clock</p>
      <div class="text-right">
        <p class="text-2xl font-semibold tabular-nums text-slate-100">{{ clockTime }}</p>
        <p class="text-xs text-slate-500">{{ clockDate }}</p>
      </div>
    </div>

    <!-- min-h keeps the card from jumping between the waiting / status /
         confirmation states. -->
    <div class="px-8 py-10 min-h-72 flex flex-col items-center justify-center gap-6">
      <!-- Waiting for a badge. -->
      <template v-if="!status && !loading">
        <svg class="w-16 h-16 text-slate-600" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
          <!-- identification card -->
          <path d="M4 4a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V6a2 2 0 0 0-2-2H4Zm3.5 3a1.5 1.5 0 1 1 0 3 1.5 1.5 0 0 1 0-3ZM5 13.2c0-1.2 1.3-2.2 2.5-2.2s2.5 1 2.5 2.2v.3H5v-.3ZM12 8h3a.5.5 0 0 1 0 1h-3a.5.5 0 0 1 0-1Zm0 3h3a.5.5 0 0 1 0 1h-3a.5.5 0 0 1 0-1Z" />
        </svg>
        <div>
          <p class="text-3xl font-semibold text-slate-100">Scan your badge</p>
          <p class="text-lg text-slate-400 mt-1">to clock in or out</p>
        </div>
        <p v-if="errorMsg" class="rounded-lg bg-red-900/40 border border-red-700/60 text-red-200 text-base px-4 py-2">
          {{ errorMsg }}
        </p>
      </template>

      <p v-else-if="loading" class="text-xl text-slate-400">Loading…</p>

      <template v-else-if="status">
        <div>
          <p class="text-4xl font-bold text-slate-100">{{ status.user_name }}</p>
          <p class="text-sm text-slate-500 font-mono mt-1">{{ status.user_code }}</p>
        </div>

        <!-- Post-punch confirmation; the countdown bar up top drains while
             the panel waits to auto-close. -->
        <div v-if="lastPunch" class="flex flex-col items-center gap-2">
          <p
            class="text-5xl font-bold"
            :class="lastPunch.direction === 'in' ? 'text-emerald-400' : 'text-amber-400'"
          >
            {{ lastPunch.direction === 'in' ? 'Clocked in' : 'Clocked out' }}
          </p>
          <p class="text-lg text-slate-400">{{ formatClock(lastPunch.occurred_at) }}</p>
          <p v-if="todaySeconds > 0" class="text-base text-slate-500 tabular-nums">
            {{ formatDuration(todaySeconds) }} today
          </p>
        </div>

        <template v-else>
          <div class="flex flex-col items-center gap-2">
            <span
              class="inline-flex items-center gap-2.5 rounded-full px-5 py-2 text-lg"
              :class="status.clocked_in
                ? 'bg-emerald-900/40 border border-emerald-700/50 text-emerald-200'
                : 'bg-slate-800/80 border border-slate-700/60 text-slate-400'"
            >
              <span class="size-2.5 rounded-full shrink-0" :class="status.clocked_in ? 'bg-emerald-400' : 'bg-slate-500'"></span>
              <template v-if="status.clocked_in">
                Clocked in since {{ formatClock(status.since) }}
                <span v-if="status.origin === 'fleet'" class="text-emerald-200/60 text-base">(at another kiosk)</span>
              </template>
              <template v-else>Not clocked in</template>
            </span>
            <p v-if="todaySeconds > 0" class="text-base text-slate-400 tabular-nums">
              {{ formatDuration(todaySeconds) }} today
            </p>
          </div>

          <p
            v-if="errorMsg"
            class="rounded-lg bg-red-900/40 border border-red-700/60 text-red-200 text-base px-4 py-2"
          >
            {{ errorMsg }}
          </p>

          <!-- Blocked clock-out: list the open tools — including which building
               to return each to — so the worker knows exactly what to do. -->
          <ul
            v-if="blockedRows.length > 0"
            class="w-full max-w-md rounded-xl bg-slate-800/60 border border-slate-700/60 divide-y divide-slate-800 text-left"
          >
            <li v-for="row in blockedRows" :key="row.id || `${row.item_code}-${row.kiosk_code}`" class="px-4 py-2.5">
              <div class="text-slate-100 truncate">{{ row.item_name }}</div>
              <div class="text-xs text-slate-500 font-mono">
                {{ row.item_instance_code || row.item_code }}<span v-if="row.instance_serial"> · SN {{ row.instance_serial }}</span>
              </div>
              <div v-if="row.kiosk_code" class="text-xs text-amber-400/90 mt-0.5">
                Return at {{ row.kiosk_code }}
              </div>
            </li>
          </ul>

          <!-- Optional job / work-order tag, offered only when clocking in.
               Not autofocused — keeps the window-level badge scan alive. -->
          <input
            v-if="blockedRows.length === 0 && !status.clocked_in"
            v-model="jobCode"
            type="text"
            placeholder="Job # (optional)"
            class="w-full max-w-md px-5 py-4 rounded-xl bg-slate-800 border border-slate-700 text-slate-100 text-xl text-center placeholder-slate-500 focus:outline-none focus:border-slate-500"
          />

          <!-- Normal punch button, hidden once a clock-out is blocked — the
               acknowledge/refresh pair below takes over. -->
          <button
            v-if="blockedRows.length === 0"
            type="button"
            class="w-full max-w-md py-6 rounded-2xl text-white text-3xl font-semibold transition-transform active:scale-95 disabled:bg-slate-700 disabled:text-slate-500"
            :class="status.clocked_in ? 'bg-amber-700/90 hover:bg-amber-700' : 'bg-emerald-700/90 hover:bg-emerald-700'"
            :disabled="punching"
            @click="punch(status.clocked_in ? 'out' : 'in')"
          >
            <template v-if="punching">Punching…</template>
            <template v-else>{{ status.clocked_in ? 'Clock out' : 'Clock in' }}</template>
          </button>

          <!-- Blocked: let the worker re-check (a fresh return may not have
               propagated yet) or clock out anyway with acknowledgment. -->
          <div v-else class="w-full max-w-md flex flex-col gap-2">
            <button
              type="button"
              class="w-full py-4 rounded-2xl bg-slate-800 hover:bg-slate-700 text-slate-200 text-xl transition-transform active:scale-95 disabled:opacity-60"
              :disabled="punching"
              @click="loadStatus(status.user_code)"
            >
              I've returned these — re-check
            </button>
            <button
              type="button"
              class="w-full py-4 rounded-2xl bg-amber-800/80 hover:bg-amber-800 text-amber-100 text-xl transition-transform active:scale-95 disabled:opacity-60"
              :disabled="punching"
              @click="punch('out', true)"
            >
              <template v-if="punching">Punching…</template>
              <template v-else>Clock out anyway</template>
            </button>
          </div>
        </template>
      </template>
    </div>

    <!-- In standalone mode the resting (waiting) state has nothing to close,
         so the footer only appears once a badge has landed; "Done" resets
         for the next worker instead of dismissing the panel. -->
    <div
      v-if="!standalone || status || loading"
      class="flex items-center justify-between gap-3 px-6 py-4 border-t border-slate-800 bg-slate-950/40"
    >
      <button
        v-if="status?.user_role === 'foreman'"
        type="button"
        class="px-6 py-3 rounded-xl bg-slate-800 hover:bg-slate-700 text-slate-200 text-base transition-transform active:scale-95"
        @click="crewOpen = true"
      >
        Punch a crew member…
      </button>
      <button
        type="button"
        class="ml-auto px-8 py-3 rounded-xl bg-slate-800 hover:bg-slate-700 text-slate-200 text-base transition-transform active:scale-95"
        @click="emit('close')"
      >
        {{ standalone ? 'Done' : 'Close' }}
      </button>
    </div>

    <TimeclockCrewDialog
      v-if="status"
      :open="crewOpen"
      :user-code="status.user_code"
      @update:open="crewOpen = $event"
    />
  </div>
</template>
