<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, ApiError } from '../lib/api'
import { useAdminToast } from '../composables/useAdminToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'

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

// Example template snippets shown in the page help text. Defined in the
// script section so Vue's template compiler doesn't try to parse the
// nested {{ }} as interpolation tokens.
const exampleField = '{{.User.Name}}'
const exampleRange = '{{range .Lines}}…{{end}}'

const toast = useAdminToast()
const { identity } = useKioskIdentity()
const managed = computed(() => identity.value?.managed ?? false)

const templates = ref<NotificationTemplate[]>([])
// Working drafts keyed by event_type — initialized from the loaded row and
// mutated freely by the textareas; Save patches the server with whatever
// is in the draft.
const drafts = ref<Record<string, NotificationTemplate>>({})
const loading = ref(false)
const saving = ref<Record<string, boolean>>({})
const errors = ref<Record<string, string>>({})

async function load() {
  loading.value = true
  try {
    const res = await api.get<TemplatesResponse>('/api/kiosk/notifications')
    templates.value = res.templates
    drafts.value = Object.fromEntries(res.templates.map((t) => [t.event_type, { ...t }]))
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
    <header class="mb-4">
      <h1 class="text-2xl font-semibold">Notifications</h1>
      <p class="text-sm text-slate-400 mt-1">
        Customize the email sent for each kiosk event. Templates use Go
        <code class="text-slate-300">text/template</code> syntax — see
        <code class="text-slate-300">{{ exampleField }}</code>,
        <code class="text-slate-300">{{ exampleRange }}</code>,
        helpers <code class="text-slate-300">formatTime</code>,
        <code class="text-slate-300">actionVerb</code>,
        <code class="text-slate-300">pluralize</code>. SMTP credentials live in
        PocketBase&rsquo;s superuser UI (<code class="text-slate-300">/_/</code> &rarr; Settings &rarr; Mail).
      </p>
    </header>

    <div
      v-if="managed"
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
  </main>
</template>
