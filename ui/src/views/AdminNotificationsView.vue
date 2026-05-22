<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, ApiError } from '../lib/api'
import { useAdminToast } from '../composables/useAdminToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import AppDialog from '../components/AppDialog.vue'

interface NotificationTemplate {
  id: string
  event_type: string
  name: string
  enabled: boolean
  subject: string
  body: string
  updated: string
}

interface TemplatesResponse {
  templates: NotificationTemplate[]
}

interface DefaultsResponse {
  event_type: string
  subject: string
  body: string
}

// Reference data for the help modal — kept in script so the template
// compiler doesn't choke on the nested {{...}} tokens. Field list mirrors
// internal/notifications/context.go's ReceiptContext shape; keep them in
// sync when fields are added or removed.
interface FieldRef {
  name: string
  desc: string
}
const receiptFields: FieldRef[] = [
  { name: '.Kiosk.Code', desc: 'Kiosk identifier (e.g. "BAY-01")' },
  { name: '.Kiosk.LocationCode', desc: 'Kiosk location (e.g. "WAREHOUSE-A")' },
  { name: '.User.Code', desc: 'Worker badge code' },
  { name: '.User.Name', desc: 'Worker display name' },
  { name: '.User.Email', desc: 'Worker email (also the recipient address)' },
  { name: '.Transaction.ID', desc: 'Transaction id' },
  { name: '.Transaction.StartedAt', desc: 'When the cart was opened (time.Time)' },
  { name: '.Transaction.CompletedAt', desc: 'When the commit succeeded (time.Time)' },
  { name: '.Transaction.LinesCount', desc: 'Total line count' },
  { name: '.Transaction.CheckedOut', desc: 'Number of checkout lines' },
  { name: '.Transaction.Returned', desc: 'Number of return lines' },
  { name: '.Transaction.Consumed', desc: 'Number of consume lines' },
  { name: '.Lines', desc: 'Slice of cart lines — use with {{range .Lines}}…{{end}}' },
]
const lineFields: FieldRef[] = [
  { name: '.ItemCode', desc: 'Item code (or instance code for serialized lines)' },
  { name: '.ItemName', desc: 'Item display name' },
  { name: '.Action', desc: '"checkout", "return", or "consume"' },
  { name: '.Qty', desc: 'Quantity for this line' },
  { name: '.Serial', desc: 'Optional serial number (empty for non-serialized lines)' },
]
const helpers: FieldRef[] = [
  { name: 'formatTime', desc: 'formatTime .Transaction.CompletedAt → "Jan 2, 2026 3:04 PM"' },
  { name: 'actionVerb', desc: 'actionVerb .Action → "checked out", "returned", "consumed"' },
  { name: 'pluralize', desc: 'pluralize .Transaction.LinesCount "item" → "1 item" / "2 items"' },
]
const showHelp = ref(false)

const toast = useAdminToast()
const { identity } = useKioskIdentity()
const managed = computed(() => identity.value?.managed ?? false)
const isController = computed(() => identity.value?.role === 'controller')

const templates = ref<NotificationTemplate[]>([])
// Working drafts keyed by event_type — initialized from the loaded row and
// mutated freely by the textareas; Save patches the server with whatever
// is in the draft.
const drafts = ref<Record<string, NotificationTemplate>>({})
const loading = ref(false)
const saving = ref<Record<string, boolean>>({})
const errors = ref<Record<string, string>>({})

async function load() {
  // The controller binary doesn't register the notifications routes —
  // hitting them falls through to the static SPA fallback (HTML body,
  // 200 status), which would crash the templates parse. Short-circuit
  // here and let the "not available on controller" banner explain.
  if (isController.value) {
    templates.value = []
    drafts.value = {}
    return
  }
  loading.value = true
  try {
    const res = await api.get<TemplatesResponse>('/api/kiosk/notifications')
    const list = res?.templates ?? []
    templates.value = list
    drafts.value = Object.fromEntries(list.map((t) => [t.event_type, { ...t }]))
    errors.value = {}
  } catch (e) {
    toast.error(`Load failed: ${(e as Error).message}`)
  } finally {
    loading.value = false
  }
}

function dirty(eventType: string): boolean {
  const orig = templates.value.find((t) => t.event_type === eventType)
  const draft = drafts.value[eventType]
  if (!orig || !draft) return false
  return (
    orig.subject !== draft.subject ||
    orig.body !== draft.body ||
    orig.enabled !== draft.enabled
  )
}

async function save(eventType: string) {
  const draft = drafts.value[eventType]
  if (!draft) return
  saving.value = { ...saving.value, [eventType]: true }
  errors.value = { ...errors.value, [eventType]: '' }
  try {
    const updated = await api.patch<NotificationTemplate>(
      `/api/kiosk/notifications/${encodeURIComponent(eventType)}`,
      { subject: draft.subject, body: draft.body, enabled: draft.enabled },
    )
    templates.value = templates.value.map((t) => (t.event_type === eventType ? updated : t))
    drafts.value = { ...drafts.value, [eventType]: { ...updated } }
    toast.success(`Saved ${updated.name}`)
  } catch (e) {
    const msg = e instanceof ApiError ? e.message : (e as Error).message
    errors.value = { ...errors.value, [eventType]: msg }
    toast.error(msg)
  } finally {
    saving.value = { ...saving.value, [eventType]: false }
  }
}

async function resetToDefaults(eventType: string) {
  try {
    const res = await api.get<DefaultsResponse>(
      `/api/kiosk/notifications/${encodeURIComponent(eventType)}/defaults`,
    )
    const draft = drafts.value[eventType]
    if (!draft) return
    drafts.value = {
      ...drafts.value,
      [eventType]: { ...draft, subject: res.subject, body: res.body },
    }
    toast.success('Defaults loaded — click Save to apply.')
  } catch (e) {
    toast.error(`Reset failed: ${(e as Error).message}`)
  }
}

function revert(eventType: string) {
  const orig = templates.value.find((t) => t.event_type === eventType)
  if (!orig) return
  drafts.value = { ...drafts.value, [eventType]: { ...orig } }
}

onMounted(load)
</script>

<template>
  <main class="p-6 max-w-4xl mx-auto w-full">
    <header class="mb-4 flex items-start justify-between gap-4">
      <div>
        <h1 class="text-2xl font-semibold">Notifications</h1>
        <p class="text-sm text-slate-400 mt-1">
          Customize the email sent for each kiosk event. SMTP credentials live in
          PocketBase&rsquo;s superuser UI (<code class="text-slate-300">/_/</code> &rarr; Settings &rarr; Mail).
        </p>
      </div>
      <button
        type="button"
        class="shrink-0 px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
        @click="showHelp = true"
      >
        Template syntax & variables
      </button>
    </header>

    <div
      v-if="isController"
      class="rounded-lg bg-slate-900 border border-slate-800 text-slate-300 px-4 py-3 mb-4 text-sm"
    >
      Notifications are sent by kiosks, not by the controller. Edit each kiosk&rsquo;s
      templates from its own admin SPA.
    </div>
    <div
      v-else-if="managed"
      class="rounded-lg bg-sky-950/60 border border-sky-800 text-sky-200 px-4 py-3 mb-4 text-sm"
    >
      Notifications are managed by the controller — this view is read-only on managed kiosks.
    </div>

    <p v-if="loading" class="text-slate-500">Loading…</p>
    <p v-else-if="templates.length === 0" class="text-slate-500">No templates yet.</p>

    <section
      v-for="t in templates"
      :key="t.event_type"
      class="rounded-2xl bg-slate-900 border border-slate-800 p-5 mb-5"
    >
      <header class="flex items-baseline justify-between mb-3">
        <div>
          <h2 class="text-lg font-semibold">{{ t.name }}</h2>
          <p class="text-xs text-slate-500 font-mono">{{ t.event_type }}</p>
        </div>
        <label class="inline-flex items-center gap-2 text-sm text-slate-300">
          <input
            type="checkbox"
            :checked="drafts[t.event_type]?.enabled ?? false"
            :disabled="managed"
            @change="drafts[t.event_type].enabled = ($event.target as HTMLInputElement).checked"
          />
          Enabled
        </label>
      </header>

      <label class="block text-sm text-slate-400 mb-1">Subject</label>
      <input
        v-model="drafts[t.event_type].subject"
        type="text"
        :disabled="managed"
        class="w-full rounded-lg bg-slate-950 border border-slate-800 px-3 py-2 text-slate-100 font-mono text-sm mb-3 disabled:opacity-60"
      />

      <label class="block text-sm text-slate-400 mb-1">Body</label>
      <textarea
        v-model="drafts[t.event_type].body"
        :disabled="managed"
        rows="14"
        class="w-full rounded-lg bg-slate-950 border border-slate-800 px-3 py-2 text-slate-100 font-mono text-sm disabled:opacity-60"
      ></textarea>

      <p
        v-if="errors[t.event_type]"
        class="mt-2 rounded-lg bg-red-900/40 border border-red-700 text-red-200 px-3 py-2 text-sm"
      >
        {{ errors[t.event_type] }}
      </p>

      <footer class="mt-4 flex items-center gap-2">
        <button
          v-if="!managed"
          type="button"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium disabled:opacity-50"
          :disabled="!dirty(t.event_type) || saving[t.event_type]"
          @click="save(t.event_type)"
        >
          {{ saving[t.event_type] ? 'Saving…' : 'Save' }}
        </button>
        <button
          v-if="!managed"
          type="button"
          class="px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
          :disabled="!dirty(t.event_type) || saving[t.event_type]"
          @click="revert(t.event_type)"
        >
          Revert changes
        </button>
        <button
          v-if="!managed"
          type="button"
          class="ml-auto px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
          @click="resetToDefaults(t.event_type)"
        >
          Reset to defaults
        </button>
        <span class="text-xs text-slate-500">
          Last updated {{ t.updated }}
        </span>
      </footer>
    </section>

    <AppDialog
      :open="showHelp"
      title="Template syntax & variables"
      size="lg"
      @update:open="showHelp = $event"
    >
      <div class="space-y-5 text-sm text-slate-300">
        <section>
          <p>
            Templates use Go&rsquo;s
            <code class="text-slate-100">text/template</code> syntax. The common forms are:
          </p>
          <ul class="list-disc list-inside text-slate-400 mt-2 space-y-1">
            <li v-pre><code class="text-slate-100">{{ .FieldName }}</code> — interpolate a value</li>
            <li v-pre><code class="text-slate-100">{{ range .Slice }}…{{ end }}</code> — loop over a slice</li>
            <li v-pre><code class="text-slate-100">{{ if .Cond }}…{{ end }}</code> — conditional block</li>
            <li v-pre><code class="text-slate-100">{{ helper .Arg }}</code> — call a helper function</li>
          </ul>
          <p class="text-xs text-slate-500 mt-2">
            Malformed templates are rejected on Save with a parse-error message.
            Bad field references (typos like
            <code v-pre class="text-slate-300">{{ .Bogus }}</code>) only surface at
            send time and end up in the server log — sends silently drop until
            fixed, so test by editing then triggering a real event.
          </p>
        </section>

        <section>
          <h3 class="text-base font-semibold text-slate-100">
            Receipt &mdash; <code class="text-slate-300 text-sm font-mono">receipt.transaction</code>
          </h3>
          <p class="text-xs text-slate-500 mt-1">
            Sent to the scanning worker&rsquo;s email after a commit. Skipped silently if the worker has no email on file.
          </p>

          <h4 class="text-sm font-medium text-slate-200 mt-3 mb-1">Top-level fields</h4>
          <table class="w-full text-left text-xs">
            <tbody class="divide-y divide-slate-800">
              <tr v-for="f in receiptFields" :key="f.name">
                <td class="py-1 pr-3 font-mono text-slate-100 whitespace-nowrap align-top">{{ f.name }}</td>
                <td class="py-1 text-slate-400">{{ f.desc }}</td>
              </tr>
            </tbody>
          </table>

          <h4 class="text-sm font-medium text-slate-200 mt-3 mb-1">Per-line fields (inside <code v-pre>{{ range .Lines }}</code>)</h4>
          <table class="w-full text-left text-xs">
            <tbody class="divide-y divide-slate-800">
              <tr v-for="f in lineFields" :key="f.name">
                <td class="py-1 pr-3 font-mono text-slate-100 whitespace-nowrap align-top">{{ f.name }}</td>
                <td class="py-1 text-slate-400">{{ f.desc }}</td>
              </tr>
            </tbody>
          </table>

          <h4 class="text-sm font-medium text-slate-200 mt-3 mb-1">Helper functions</h4>
          <table class="w-full text-left text-xs">
            <tbody class="divide-y divide-slate-800">
              <tr v-for="h in helpers" :key="h.name">
                <td class="py-1 pr-3 font-mono text-slate-100 whitespace-nowrap align-top">{{ h.name }}</td>
                <td class="py-1 text-slate-400">{{ h.desc }}</td>
              </tr>
            </tbody>
          </table>
        </section>

        <section>
          <h3 class="text-base font-semibold text-slate-100 mb-2">Example body</h3>
          <pre
            v-pre
            class="rounded-lg bg-slate-950 border border-slate-800 p-3 text-xs text-slate-200 font-mono overflow-x-auto whitespace-pre-wrap"
>Hi {{ .User.Name }},

Receipt from {{ .Kiosk.Code }} on {{ formatTime .Transaction.CompletedAt }}:

{{ range .Lines }}- {{ actionVerb .Action }} {{ .Qty }} × {{ .ItemName }}{{ if .Serial }} [serial: {{ .Serial }}]{{ end }}
{{ end }}
Transaction id: {{ .Transaction.ID }}
</pre>
        </section>

        <div class="flex justify-end pt-2">
          <button
            type="button"
            class="px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
            @click="showHelp = false"
          >
            Close
          </button>
        </div>
      </div>
    </AppDialog>
  </main>
</template>
