<!-- TimeclockCrewDialog is the foreman's "punch a crew member" picker:
     active workers in the foreman's group, each with their current punch
     state and a single toggle action. Punch-NOW only — no backdating from
     the floor; corrections are an admin affordance. The server re-enforces
     the foreman+group gate inside the punch funnel; this dialog's listing
     is UX, not authorization. Copied from ForemanReturnDialog's shape. -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import AppDialog from './AppDialog.vue'
import { api, ApiError } from '../lib/api'
import type { PunchConflict, PunchResult, TimeclockCrewMember, TimeclockCrewOptions } from '../types'

const props = defineProps<{
  open: boolean
  // The acting foreman's user code (server resolves + gates).
  userCode: string
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
}>()

const options = ref<TimeclockCrewOptions | null>(null)
const loading = ref(false)
const errorMsg = ref('')
const submitting = ref<string | null>(null) // user_id of row being punched

watch(
  () => props.open,
  async (open) => {
    if (!open) return
    errorMsg.value = ''
    submitting.value = null
    await loadOptions()
  },
)

async function loadOptions() {
  loading.value = true
  errorMsg.value = ''
  try {
    options.value = await api.get<TimeclockCrewOptions>(
      `/api/kiosk/timeclock/foreman/options?user_code=${encodeURIComponent(props.userCode)}`,
    )
  } catch (e) {
    options.value = null
    errorMsg.value = e instanceof ApiError ? e.message : (e as Error).message
  } finally {
    loading.value = false
  }
}

async function punch(member: TimeclockCrewMember) {
  if (submitting.value) return
  submitting.value = member.user_id
  errorMsg.value = ''
  try {
    const res = await api.post<PunchResult>('/api/kiosk/timeclock/punch', {
      user_code: props.userCode,
      target_user_code: member.user_code,
      direction: member.clocked_in ? 'out' : 'in',
    })
    member.clocked_in = res.clocked_in
    member.since = res.occurred_at
  } catch (e) {
    if (e instanceof ApiError && e.status === 409) {
      const data = e.data as PunchConflict | null
      errorMsg.value =
        data?.error === 'open_checkouts'
          ? `${member.user_name} still has ${data.count ?? 'some'} tool(s) out — return them first.`
          : (data?.message ?? e.message)
      // State may be stale (punched at another kiosk) — refresh the list.
      void loadOptions()
    } else {
      errorMsg.value = e instanceof ApiError ? e.message : (e as Error).message
    }
  } finally {
    submitting.value = null
  }
}

function formatClock(iso?: string): string {
  if (!iso) return ''
  const t = new Date(iso)
  if (!Number.isFinite(t.getTime())) return ''
  return t.toLocaleString([], { weekday: 'short', hour: 'numeric', minute: '2-digit' })
}

const hasWorkers = computed(() => (options.value?.workers.length ?? 0) > 0)
</script>

<template>
  <AppDialog
    :open="open"
    title="Punch a crew member"
    :description="
      options
        ? `Active workers in ${options.group_code}. Punches record now — backdating is admin-only.`
        : 'Active workers in your group. Punches record now — backdating is admin-only.'
    "
    size="md"
    @update:open="emit('update:open', $event)"
  >
    <div class="flex flex-col gap-4">
      <p
        v-if="errorMsg"
        class="rounded-lg bg-red-900/40 border border-red-700/60 text-red-200 text-sm px-3 py-2"
      >
        {{ errorMsg }}
      </p>

      <div class="flex flex-col gap-2 max-h-[55vh] overflow-y-auto">
        <p v-if="loading" class="text-slate-400 text-sm">Loading…</p>
        <p
          v-else-if="!hasWorkers"
          class="rounded-xl bg-slate-800/60 border border-slate-700/60 text-slate-400 text-sm px-4 py-6 text-center"
        >
          No active workers in your group.
        </p>

        <div
          v-for="member in options?.workers ?? []"
          :key="member.user_id"
          class="rounded-xl bg-slate-800/60 border border-slate-700/60 flex items-center justify-between gap-3 px-4 py-3"
        >
          <div class="min-w-0">
            <p class="text-slate-100 font-medium truncate">{{ member.user_name }}</p>
            <p class="text-xs font-mono truncate" :class="member.clocked_in ? 'text-emerald-400' : 'text-slate-500'">
              <template v-if="member.clocked_in">in since {{ formatClock(member.since) }}</template>
              <template v-else>not clocked in</template>
            </p>
          </div>
          <button
            type="button"
            class="px-5 py-2.5 rounded-lg text-white text-sm font-medium transition-transform active:scale-95 disabled:opacity-50 shrink-0"
            :class="member.clocked_in ? 'bg-amber-700/90 hover:bg-amber-700' : 'bg-emerald-700/90 hover:bg-emerald-700'"
            :disabled="submitting !== null"
            @click="punch(member)"
          >
            <template v-if="submitting === member.user_id">Punching…</template>
            <template v-else>{{ member.clocked_in ? 'Clock out' : 'Clock in' }}</template>
          </button>
        </div>
      </div>

      <div class="flex justify-end pt-2">
        <button
          type="button"
          class="px-6 py-3 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 transition-transform active:scale-95"
          @click="emit('update:open', false)"
        >
          Close
        </button>
      </div>
    </div>
  </AppDialog>
</template>
