<!-- VirtualTimeclockView is the public, per-user-authenticated self-service
     time clock (the cmd/timeclock binary). The trust model is inverted from
     the physical kiosk: instead of a badge scan on a trusted box, the worker
     authenticates (OAuth2 SSO or password), and the server reads the punched
     identity from the session — never from the request. This view is mobile-
     first; it never mounts the scan composable or any checkout machinery.

     Two states: a login screen when unauthenticated, and the self-punch panel
     once a `users` session exists. The panel mirrors the physical
     TimeclockPanel's visual language (status chip, one big punch button,
     blocked-clock-out tool list) but is self-only — no foreman crew dialog,
     no badge prop — and talks to /api/self/timeclock/*. -->
<script setup lang="ts">
import { computed, onMounted, onScopeDispose, ref, watch } from 'vue'
import { api, ApiError } from '../lib/api'
import { useWorkerAuthStore, type AuthMethods } from '../stores/workerAuth'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import type {
  OpenCheckoutDetail,
  PunchConflict,
  PunchResult,
  PunchStatus,
  TimeclockHistoryResponse,
} from '../types'

const { identity } = useKioskIdentity()
const auth = useWorkerAuthStore()

const logoUrl = computed(() => identity.value?.branding?.logo_url ?? '')
const tagline = computed(() => identity.value?.branding?.tagline ?? 'Time clock')

// Live wall clock — same timeclock idiom as the physical panel: it shows the
// worker the minute their punch will record.
const now = ref(new Date())
const clockHandle = setInterval(() => {
  now.value = new Date()
}, 1000)
const clockTime = computed(() => now.value.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }))
const clockDate = computed(() =>
  now.value.toLocaleDateString([], { weekday: 'long', month: 'short', day: 'numeric' }),
)
onScopeDispose(() => clearInterval(clockHandle))

// ---------- login ----------
const methods = ref<AuthMethods | null>(null)
const email = ref('')
const password = ref('')
const loginBusy = ref(false)
const loginError = ref('')
const resetMsg = ref('')

async function refreshMethods() {
  try {
    methods.value = await auth.listMethods()
  } catch {
    // If discovery fails, fall back to offering the password form.
    methods.value = { passwordEnabled: true, oauth2Providers: [] }
  }
}

onMounted(() => {
  if (!auth.isAuthenticated) void refreshMethods()
})

async function onPasswordLogin() {
  if (loginBusy.value || !email.value || !password.value) return
  loginBusy.value = true
  loginError.value = ''
  resetMsg.value = ''
  try {
    await auth.loginPassword(email.value.trim(), password.value)
    password.value = ''
  } catch (e) {
    loginError.value = e instanceof ApiError ? e.message : (e as Error).message || 'Sign-in failed'
  } finally {
    loginBusy.value = false
  }
}

async function onOAuth2(provider: string) {
  if (loginBusy.value) return
  loginBusy.value = true
  loginError.value = ''
  resetMsg.value = ''
  try {
    await auth.loginOAuth2(provider)
  } catch (e) {
    // The match-only guard returns 403 for un-provisioned IdP accounts.
    loginError.value =
      e instanceof ApiError && e.status === 403
        ? 'This account is not set up for the time clock. Ask your administrator.'
        : (e as Error)?.message || 'Single sign-on failed'
  } finally {
    loginBusy.value = false
  }
}

async function onReset() {
  if (!email.value) {
    loginError.value = 'Enter your email first, then tap “Forgot password”.'
    return
  }
  loginError.value = ''
  try {
    await auth.requestPasswordReset(email.value.trim())
    resetMsg.value = 'If that email is registered, a reset link is on its way.'
  } catch (e) {
    loginError.value = e instanceof ApiError ? e.message : (e as Error).message
  }
}

// ---------- punch ----------
const status = ref<PunchStatus | null>(null)
const loadingStatus = ref(false)
const punching = ref(false)
const punchError = ref('')
const blockedRows = ref<OpenCheckoutDetail[]>([])
const lastPunch = ref<PunchResult | null>(null)

// ---------- timesheet summary ----------
// A glanceable "Today / This week" total plus today's punch pairs, fetched
// from the worker's own history. day_totals carry CLOSED-interval seconds only
// (the running session contributes 0), so we add the live session on top and
// let it tick off the shared `now` clock — no extra timer. Scope is THIS
// terminal's punch ledger: in a managed deployment, punches a worker made at a
// physical kiosk live in that kiosk's ledger and won't be reflected here.
const summary = ref<TimeclockHistoryResponse | null>(null)

async function loadSummary() {
  // Widen the fetch a day on each side: the server filters occurred_at by UTC
  // instants but buckets day_totals by LOCAL day, so a same-local-day punch
  // near midnight can land on an adjacent UTC day. We pull the wider window and
  // select local-day buckets back out (todayKey / [weekStartKey..todayKey]).
  const from = ymd(addDays(mondayOf(new Date()), -1))
  const to = ymd(addDays(new Date(), 1))
  try {
    summary.value = await api.get<TimeclockHistoryResponse>(
      `/api/self/timeclock/history?from=${from}&to=${to}`,
    )
  } catch {
    // The summary is supplementary; never let a history blip break punching.
    summary.value = null
  }
}

async function loadStatus() {
  loadingStatus.value = true
  punchError.value = ''
  blockedRows.value = []
  lastPunch.value = null
  try {
    status.value = await api.get<PunchStatus>('/api/self/timeclock/status')
    void loadSummary()
  } catch (e) {
    if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
      // Token expired / worker deactivated — drop back to the login screen.
      auth.logout()
      void refreshMethods()
      return
    }
    punchError.value = e instanceof ApiError ? e.message : (e as Error).message
  } finally {
    loadingStatus.value = false
  }
}

async function punch(direction: 'in' | 'out') {
  if (!status.value || punching.value) return
  punching.value = true
  punchError.value = ''
  blockedRows.value = []
  try {
    const res = await api.post<PunchResult>('/api/self/timeclock/punch', { direction })
    lastPunch.value = res
    status.value = { ...status.value, clocked_in: res.clocked_in, since: res.occurred_at }
    void loadSummary()
  } catch (e) {
    if (e instanceof ApiError && e.status === 409) {
      const data = e.data as PunchConflict | null
      if (data?.error === 'open_checkouts') {
        blockedRows.value = data.open_checkouts ?? []
        punchError.value = 'Return these tools before clocking out.'
        return
      }
      // already/not clocked in — fleet state changed elsewhere; refresh.
      punchError.value = data?.message ?? e.message
      void loadStatus()
      return
    }
    if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
      auth.logout()
      void refreshMethods()
      return
    }
    punchError.value = e instanceof ApiError ? e.message : (e as Error).message
  } finally {
    punching.value = false
  }
}

// Load (or reload) status whenever a session appears.
watch(
  () => auth.isAuthenticated,
  (authed) => {
    if (authed) void loadStatus()
    else status.value = null
  },
  { immediate: true },
)

function logout() {
  auth.logout()
  status.value = null
  lastPunch.value = null
  summary.value = null
  void refreshMethods()
}

function formatClock(iso?: string): string {
  if (!iso) return ''
  const t = new Date(iso)
  if (!Number.isFinite(t.getTime())) return ''
  return t.toLocaleString([], { weekday: 'short', hour: 'numeric', minute: '2-digit' })
}

// Time-of-day only (today's punch list — the day is implied).
function formatTime(iso?: string): string {
  if (!iso) return ''
  const t = new Date(iso)
  if (!Number.isFinite(t.getTime())) return ''
  return t.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
}

// Local YYYY-MM-DD — matches the server's day bucketing, which pairs in
// time.Local (see timeclock.Pair / dayTotals).
function ymd(d: Date): string {
  const y = d.getFullYear()
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${y}-${m}-${day}`
}

// Monday 00:00 of the week containing d (the week boundary is Monday).
function mondayOf(d: Date): Date {
  const x = new Date(d)
  x.setHours(0, 0, 0, 0)
  x.setDate(x.getDate() - ((x.getDay() + 6) % 7))
  return x
}

function addDays(d: Date, n: number): Date {
  const x = new Date(d)
  x.setDate(x.getDate() + n)
  return x
}

// Whole-minute readout (e.g. "4h 28m"). Truncation for a glanceable display —
// NOT payroll rounding; the raw-punch CSV stays the contract.
function formatDuration(totalSeconds: number): string {
  const s = Math.max(0, Math.floor(totalSeconds))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  return h > 0 ? `${h}h ${m}m` : `${m}m`
}

const todayKey = computed(() => ymd(now.value))
const weekStartKey = computed(() => ymd(mondayOf(now.value)))

// Seconds in the currently-running session, ticking off `now`; zero when
// clocked out. day_totals never count the open interval, so adding this to the
// closed totals can't double-count.
const liveSeconds = computed(() => {
  if (!status.value?.clocked_in || !status.value.since) return 0
  const since = new Date(status.value.since).getTime()
  if (!Number.isFinite(since)) return 0
  return Math.max(0, Math.floor((now.value.getTime() - since) / 1000))
})

const todaySeconds = computed(() => {
  const closed = summary.value?.day_totals.find((d) => d.date === todayKey.value)?.seconds ?? 0
  return closed + liveSeconds.value
})

const weekSeconds = computed(() => {
  const closed = (summary.value?.day_totals ?? [])
    .filter((d) => d.date >= weekStartKey.value && d.date <= todayKey.value)
    .reduce((sum, d) => sum + d.seconds, 0)
  return closed + liveSeconds.value
})

// Today's intervals, newest first so a running session sits on top.
const todayIntervals = computed(() =>
  (summary.value?.intervals ?? [])
    .filter((iv) => ymd(new Date(iv.in)) === todayKey.value)
    .slice()
    .reverse(),
)
</script>

<template>
  <div class="min-h-full flex flex-col items-center justify-center px-4 py-8 bg-slate-950">
    <div class="w-full max-w-md">
      <!-- Branding header -->
      <div class="flex flex-col items-center gap-3 mb-6 text-center">
        <img v-if="logoUrl" :src="logoUrl" alt="" class="h-14 object-contain" />
        <div>
          <p class="text-xl font-semibold text-slate-100">{{ tagline }}</p>
          <p class="text-sm text-slate-500 tabular-nums">{{ clockTime }} · {{ clockDate }}</p>
        </div>
      </div>

      <!-- ============ LOGIN ============ -->
      <div
        v-if="!auth.isAuthenticated"
        class="rounded-2xl bg-slate-900 border border-slate-800 p-6 flex flex-col gap-4"
      >
        <p class="text-lg font-semibold text-slate-100 text-center">Sign in to clock in or out</p>

        <!-- SSO providers -->
        <div v-if="methods?.oauth2Providers.length" class="flex flex-col gap-2">
          <button
            v-for="p in methods.oauth2Providers"
            :key="p.name"
            type="button"
            class="w-full py-3 rounded-xl bg-slate-100 hover:bg-white text-slate-900 font-medium transition-transform active:scale-95 disabled:opacity-60"
            :disabled="loginBusy"
            @click="onOAuth2(p.name)"
          >
            Continue with {{ p.displayName }}
          </button>
        </div>

        <div
          v-if="methods?.oauth2Providers.length && methods?.passwordEnabled"
          class="flex items-center gap-3 text-slate-600 text-xs"
        >
          <span class="h-px flex-1 bg-slate-800"></span>OR<span class="h-px flex-1 bg-slate-800"></span>
        </div>

        <!-- Password -->
        <form v-if="methods?.passwordEnabled" class="flex flex-col gap-3" @submit.prevent="onPasswordLogin">
          <input
            v-model="email"
            type="email"
            autocomplete="username"
            placeholder="Email"
            class="w-full px-4 py-3 rounded-xl bg-slate-800 border border-slate-700 text-slate-100 placeholder-slate-500 focus:outline-none focus:border-slate-500"
          />
          <input
            v-model="password"
            type="password"
            autocomplete="current-password"
            placeholder="Password"
            class="w-full px-4 py-3 rounded-xl bg-slate-800 border border-slate-700 text-slate-100 placeholder-slate-500 focus:outline-none focus:border-slate-500"
          />
          <button
            type="submit"
            class="w-full py-3 rounded-xl bg-emerald-700 hover:bg-emerald-600 text-white font-semibold transition-transform active:scale-95 disabled:bg-slate-700 disabled:text-slate-500"
            :disabled="loginBusy"
          >
            {{ loginBusy ? 'Signing in…' : 'Sign in' }}
          </button>
          <button
            type="button"
            class="text-sm text-slate-400 hover:text-slate-200"
            @click="onReset"
          >
            Forgot password?
          </button>
        </form>

        <p v-if="resetMsg" class="rounded-lg bg-slate-800 text-slate-300 text-sm px-4 py-2 text-center">
          {{ resetMsg }}
        </p>
        <p
          v-if="loginError"
          class="rounded-lg bg-red-900/40 border border-red-700/60 text-red-200 text-sm px-4 py-2 text-center"
        >
          {{ loginError }}
        </p>
      </div>

      <!-- ============ PUNCH ============ -->
      <template v-else>
      <div
        class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden text-center"
      >
        <div class="px-6 py-10 min-h-72 flex flex-col items-center justify-center gap-6">
          <p v-if="loadingStatus" class="text-lg text-slate-400">Loading…</p>

          <template v-else-if="status">
            <div>
              <p class="text-3xl font-bold text-slate-100">{{ status.user_name }}</p>
              <p class="text-xs text-slate-500 font-mono mt-1">{{ status.user_code }}</p>
            </div>

            <div v-if="lastPunch" class="flex flex-col items-center gap-1">
              <p
                class="text-4xl font-bold"
                :class="lastPunch.direction === 'in' ? 'text-emerald-400' : 'text-amber-400'"
              >
                {{ lastPunch.direction === 'in' ? 'Clocked in' : 'Clocked out' }}
              </p>
              <p class="text-base text-slate-400">{{ formatClock(lastPunch.occurred_at) }}</p>
            </div>

            <span
              v-else
              class="inline-flex items-center gap-2.5 rounded-full px-5 py-2 text-base"
              :class="status.clocked_in
                ? 'bg-emerald-900/40 border border-emerald-700/50 text-emerald-200'
                : 'bg-slate-800/80 border border-slate-700/60 text-slate-400'"
            >
              <span class="size-2.5 rounded-full shrink-0" :class="status.clocked_in ? 'bg-emerald-400' : 'bg-slate-500'"></span>
              <template v-if="status.clocked_in">
                Clocked in since {{ formatClock(status.since) }}
                <span v-if="status.origin === 'fleet'" class="text-emerald-200/60 text-sm">(elsewhere)</span>
              </template>
              <template v-else>Not clocked in</template>
            </span>

            <p
              v-if="punchError"
              class="rounded-lg bg-red-900/40 border border-red-700/60 text-red-200 text-sm px-4 py-2"
            >
              {{ punchError }}
            </p>

            <ul
              v-if="blockedRows.length > 0"
              class="w-full rounded-xl bg-slate-800/60 border border-slate-700/60 divide-y divide-slate-800 text-left"
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
              class="w-full py-6 rounded-2xl text-white text-2xl font-semibold transition-transform active:scale-95 disabled:bg-slate-700 disabled:text-slate-500"
              :class="status.clocked_in ? 'bg-amber-700/90 hover:bg-amber-700' : 'bg-emerald-700/90 hover:bg-emerald-700'"
              :disabled="punching"
              @click="punch(status.clocked_in ? 'out' : 'in')"
            >
              <template v-if="punching">Punching…</template>
              <template v-else>{{ status.clocked_in ? 'Clock out' : 'Clock in' }}</template>
            </button>
          </template>

          <p v-else-if="punchError" class="rounded-lg bg-red-900/40 border border-red-700/60 text-red-200 text-sm px-4 py-2">
            {{ punchError }}
          </p>
        </div>

        <div class="flex items-center justify-end gap-3 px-6 py-4 border-t border-slate-800 bg-slate-950/40">
          <button
            type="button"
            class="px-6 py-2.5 rounded-xl bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm transition-transform active:scale-95"
            @click="logout"
          >
            Log out
          </button>
        </div>
      </div>

      <!-- ============ TIMESHEET SUMMARY ============ -->
      <div
        v-if="status && summary"
        class="mt-4 rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden"
      >
        <div class="grid grid-cols-2 divide-x divide-slate-800 border-b border-slate-800">
          <div class="px-5 py-4 text-center">
            <p class="text-xs uppercase tracking-wide text-slate-500">Today</p>
            <p class="mt-1 text-2xl font-semibold text-slate-100 tabular-nums">
              {{ formatDuration(todaySeconds) }}
              <span
                v-if="status.clocked_in"
                class="inline-block size-2 rounded-full bg-emerald-400 animate-pulse align-middle ml-0.5"
                aria-label="clock running"
              ></span>
            </p>
          </div>
          <div class="px-5 py-4 text-center">
            <p class="text-xs uppercase tracking-wide text-slate-500">This week</p>
            <p class="mt-1 text-2xl font-semibold text-slate-100 tabular-nums">
              {{ formatDuration(weekSeconds) }}
            </p>
          </div>
        </div>

        <div class="px-5 py-4">
          <p class="text-xs uppercase tracking-wide text-slate-500 mb-2">Today's punches</p>
          <ul v-if="todayIntervals.length" class="flex flex-col gap-1.5">
            <li
              v-for="(iv, i) in todayIntervals"
              :key="i"
              class="flex items-center justify-between text-sm"
            >
              <span class="text-slate-300 tabular-nums">
                {{ formatTime(iv.in) }}
                <span class="text-slate-600 px-0.5">→</span>
                <span v-if="iv.open" class="text-emerald-400">in progress</span>
                <template v-else>{{ formatTime(iv.out) }}</template>
              </span>
              <span class="text-slate-400 tabular-nums">
                {{ formatDuration(iv.open ? liveSeconds : iv.seconds) }}
              </span>
            </li>
          </ul>
          <p v-else class="text-sm text-slate-500">No punches yet today.</p>
        </div>
      </div>
      </template>
    </div>
  </div>
</template>
