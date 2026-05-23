<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, ApiError } from '../lib/api'
import { pb } from '../lib/pb'
import { useAdminToast } from '../composables/useAdminToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import AppDialog from '../components/AppDialog.vue'
import NotificationsTabs from '../components/NotificationsTabs.vue'

interface Recipients {
  worker_email: boolean
  all_admins: boolean
  extras: string[]
}

interface NotificationTemplate {
  id: string
  event_type: string
  name: string
  enabled: boolean
  subject: string
  body: string
  updated: string
  updated_by: string
  recipients: Recipients
  supports_worker: boolean
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

// The controller now owns notification authoring for the fleet: it has its
// own notification_templates rows and serves the same CRUD endpoints under
// /api/controller/notifications. Managed kiosks send context events over
// NATS and the controller renders + SMTPs. Standalone kiosks keep the
// pre-managed flow on /api/kiosk/notifications.
const apiBase = computed(() =>
  isController.value ? '/api/controller/notifications' : '/api/kiosk/notifications',
)

const templates = ref<NotificationTemplate[]>([])
// Working drafts keyed by event_type — initialized from the loaded row and
// mutated freely by the textareas; Save patches the server with whatever
// is in the draft.
const drafts = ref<Record<string, NotificationTemplate>>({})
const loading = ref(false)
const saving = ref<Record<string, boolean>>({})
const errors = ref<Record<string, string>>({})
// Per-template "last edited by" email, resolved once per admin id and cached
// here so the footer doesn't N+1 against pb.collection('admins').getOne
// every render. Empty string for rows that haven't been saved since phase 2
// added the column.
const editorEmails = ref<Record<string, string>>({})
// Recipients.extras is a string[] on the wire but a textarea on the UI —
// store the joined-by-newlines text per event_type so the textarea binds
// to a plain string. Parsed back into a string[] at save time.
const extrasText = ref<Record<string, string>>({})

function extrasToText(list: string[]): string {
  return (list ?? []).join('\n')
}

function parseExtras(text: string): string[] {
  // Split on newlines OR commas so a copy-paste of "ops@…, ml@…" still works.
  return text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter((s) => s.length > 0)
}

async function load() {
  // Managed kiosks no longer authoritate their own templates — the
  // controller's notification_templates rows are what the controller will
  // actually send from. Skip the local load to avoid presenting stale
  // rows the operator might mistake for live state; the banner below
  // points them to the controller's admin SPA.
  if (managed.value) {
    templates.value = []
    drafts.value = {}
    return
  }
  loading.value = true
  try {
    const res = await api.get<TemplatesResponse>(apiBase.value)
    const list = res?.templates ?? []
    templates.value = list
    drafts.value = Object.fromEntries(
      list.map((t) => [t.event_type, { ...t, recipients: { ...t.recipients, extras: [...t.recipients.extras] } }]),
    )
    extrasText.value = Object.fromEntries(list.map((t) => [t.event_type, extrasToText(t.recipients.extras)]))
    errors.value = {}
    await resolveEditorEmails(list)
  } catch (e) {
    toast.error(`Load failed: ${(e as Error).message}`)
  } finally {
    loading.value = false
  }
}

async function resolveEditorEmails(list: NotificationTemplate[]) {
  const need = new Set<string>()
  for (const t of list) {
    if (t.updated_by && editorEmails.value[t.updated_by] === undefined) {
      need.add(t.updated_by)
    }
  }
  if (need.size === 0) return
  // One getOne per distinct id. The admin pool is small (a handful of rows
  // in practice); no need for a batched-fetch endpoint yet.
  const next = { ...editorEmails.value }
  await Promise.all(
    Array.from(need).map(async (id) => {
      try {
        const rec = await pb.collection('admins').getOne<{ email: string }>(id)
        next[id] = rec.email
      } catch {
        next[id] = '' // failed lookup; show nothing rather than retry-loop
      }
    }),
  )
  editorEmails.value = next
}

function editorLabel(t: NotificationTemplate): string {
  if (!t.updated_by) return ''
  const email = editorEmails.value[t.updated_by]
  return email ? `by ${email}` : ''
}

function dirty(eventType: string): boolean {
  const orig = templates.value.find((t) => t.event_type === eventType)
  const draft = drafts.value[eventType]
  if (!orig || !draft) return false
  if (orig.subject !== draft.subject) return true
  if (orig.body !== draft.body) return true
  if (orig.enabled !== draft.enabled) return true
  if (orig.recipients.worker_email !== draft.recipients.worker_email) return true
  if (orig.recipients.all_admins !== draft.recipients.all_admins) return true
  // Compare extras as the user-typed text so trailing-whitespace tweaks
  // surface as dirty (matches how the textarea actually looks).
  if (extrasText.value[eventType] !== extrasToText(orig.recipients.extras)) return true
  return false
}

async function save(eventType: string) {
  const draft = drafts.value[eventType]
  if (!draft) return
  saving.value = { ...saving.value, [eventType]: true }
  errors.value = { ...errors.value, [eventType]: '' }
  const extras = parseExtras(extrasText.value[eventType] ?? '')
  try {
    const updated = await api.patch<NotificationTemplate>(
      `${apiBase.value}/${encodeURIComponent(eventType)}`,
      {
        subject: draft.subject,
        body: draft.body,
        enabled: draft.enabled,
        recipients: {
          worker_email: draft.recipients.worker_email,
          all_admins: draft.recipients.all_admins,
          extras,
        },
      },
    )
    templates.value = templates.value.map((t) => (t.event_type === eventType ? updated : t))
    drafts.value = {
      ...drafts.value,
      [eventType]: { ...updated, recipients: { ...updated.recipients, extras: [...updated.recipients.extras] } },
    }
    extrasText.value = { ...extrasText.value, [eventType]: extrasToText(updated.recipients.extras) }
    await resolveEditorEmails([updated])
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
      `${apiBase.value}/${encodeURIComponent(eventType)}/defaults`,
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
  drafts.value = {
    ...drafts.value,
    [eventType]: { ...orig, recipients: { ...orig.recipients, extras: [...orig.recipients.extras] } },
  }
  extrasText.value = { ...extrasText.value, [eventType]: extrasToText(orig.recipients.extras) }
}

onMounted(load)
</script>

<template>
  <main class="p-6 max-w-4xl mx-auto w-full">
    <header class="mb-4">
      <h1 class="text-2xl font-semibold">Notifications</h1>
      <p class="text-sm text-slate-400 mt-1">
        Email events the kiosk sends. SMTP credentials live in PocketBase&rsquo;s
        superuser UI (<code class="text-slate-300">/_/</code> &rarr; Settings &rarr; Mail).
      </p>
    </header>

    <NotificationsTabs />

    <div class="mb-4 flex items-center justify-between gap-4">
      <p class="text-sm text-slate-400">
        Customize the email sent for each kiosk event.
      </p>
      <button
        type="button"
        class="shrink-0 px-3 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200 text-sm"
        @click="showHelp = true"
      >
        Template syntax & variables
      </button>
    </div>

    <div
      v-if="managed"
      class="rounded-lg bg-sky-950/60 border border-sky-800 text-sky-200 px-4 py-3 mb-4 text-sm"
    >
      Templates are edited on the controller for this managed kiosk — all
      receipt, low-stock, and digest emails render against the controller&rsquo;s
      template rows and send via its SMTP. The Scheduled tab below stays
      editable here: cron timing and recipient lists are kiosk-local, but
      the actual sends fire from the controller.
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

      <fieldset class="mt-4 rounded-lg border border-slate-800 p-3">
        <legend class="px-1 text-sm text-slate-400">Recipients</legend>
        <label class="flex items-start gap-2 text-sm text-slate-300 mb-2">
          <input
            type="checkbox"
            :checked="drafts[t.event_type].recipients.worker_email"
            :disabled="managed || !t.supports_worker"
            class="mt-1 disabled:opacity-50"
            @change="drafts[t.event_type].recipients.worker_email = ($event.target as HTMLInputElement).checked"
          />
          <span class="flex flex-col">
            <span>Send to the worker who scanned</span>
            <span v-if="!t.supports_worker" class="text-xs text-slate-500">
              Not available — this event type doesn&rsquo;t carry a worker identity.
            </span>
          </span>
        </label>
        <label class="flex items-start gap-2 text-sm text-slate-300 mb-2">
          <input
            type="checkbox"
            :checked="drafts[t.event_type].recipients.all_admins"
            :disabled="managed"
            class="mt-1"
            @change="drafts[t.event_type].recipients.all_admins = ($event.target as HTMLInputElement).checked"
          />
          Send to every active admin
        </label>
        <label class="block text-sm text-slate-300 mt-3">
          <span class="block text-slate-400 mb-1">Additional recipients</span>
          <textarea
            v-model="extrasText[t.event_type]"
            :disabled="managed"
            rows="3"
            placeholder="One email per line — commas also work."
            class="w-full rounded-lg bg-slate-950 border border-slate-800 px-3 py-2 text-slate-100 text-sm disabled:opacity-60"
          ></textarea>
        </label>
      </fieldset>

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
          Last updated {{ t.updated }}<span v-if="editorLabel(t)"> {{ editorLabel(t) }}</span>
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
