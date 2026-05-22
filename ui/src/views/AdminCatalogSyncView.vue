<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api } from '../lib/api'
import { useAdminToast } from '../composables/useAdminToast'
import { useKioskIdentity } from '../composables/useKioskIdentity'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import type {
  CatalogIntegrityReport,
  CatalogReconcileReport,
} from '../types'

// AdminCatalogSyncView: controller-only view for diffing the catalog DB
// against the JetStream KV buckets, and one-button reconciliation. Mirrors
// the integrity → rebuild pattern in AdminReportsView's "currently out"
// tab, but for cross-fleet catalog state instead of local open_checkouts.
//
// One-directional: the controller's DB is the source of truth; reconcile
// pushes DB → KV (optionally deleting KV orphans). There is no
// "KV teaches the DB" mode — see the README's "Reconciling catalog drift"
// section for the rationale.

const toast = useAdminToast()
const { identity } = useKioskIdentity()
const isController = computed(() => identity.value?.role === 'controller')

const report = ref<CatalogIntegrityReport | null>(null)
const lastReconcile = ref<CatalogReconcileReport | null>(null)
const loading = ref(false)
const reconciling = ref(false)
const error = ref<string | null>(null)
const confirmDeleteOpen = ref(false)

async function loadIntegrity() {
  loading.value = true
  error.value = null
  try {
    report.value = await api.get<CatalogIntegrityReport>(
      '/api/kiosk/catalog/integrity',
    )
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  if (isController.value) loadIntegrity()
})

async function runReconcile(deleteOrphans: boolean) {
  confirmDeleteOpen.value = false
  reconciling.value = true
  try {
    lastReconcile.value = await api.post<CatalogReconcileReport>(
      '/api/kiosk/catalog/reconcile',
      { delete_orphans: deleteOrphans },
    )
    const i = lastReconcile.value.items
    const u = lastReconcile.value.users
    const g = lastReconcile.value.groups
    const errCount =
      (i.publish_errors?.length ?? 0) +
      (i.delete_errors?.length ?? 0) +
      (u.publish_errors?.length ?? 0) +
      (u.delete_errors?.length ?? 0) +
      (g?.publish_errors?.length ?? 0) +
      (g?.delete_errors?.length ?? 0)
    if (errCount > 0) {
      toast.error(
        `Reconciled with ${errCount} error${errCount === 1 ? '' : 's'} — see details below.`,
      )
    } else {
      const action = deleteOrphans ? 'pushed + cleaned orphans' : 'pushed'
      const groupSummary = g ? `, groups ${g.published}/${g.deleted}` : ''
      toast.success(
        `Catalog ${action}: items ${i.published}/${i.deleted}, users ${u.published}/${u.deleted}${groupSummary}.`,
      )
    }
    // Re-fetch the diff to show that drift is now resolved.
    await loadIntegrity()
  } catch (e) {
    toast.error(`Reconcile failed: ${(e as Error).message}`)
  } finally {
    reconciling.value = false
  }
}

// Belt-and-suspenders nil guards: older controller builds may have emitted
// `null` for empty diff arrays. Newer builds always emit [], but coalescing
// here makes a version-mismatched controller graceful instead of crashing.
function missingLen(b: { missing_in_kv?: string[] | null } | undefined): number {
  return b?.missing_in_kv?.length ?? 0
}
function extraLen(b: { extra_in_kv?: string[] | null } | undefined): number {
  return b?.extra_in_kv?.length ?? 0
}
function safeList(arr: string[] | null | undefined): string[] {
  return arr ?? []
}

const itemsHasDrift = computed(() => {
  const r = report.value?.items
  return r ? missingLen(r) > 0 || extraLen(r) > 0 : false
})
const usersHasDrift = computed(() => {
  const r = report.value?.users
  return r ? missingLen(r) > 0 || extraLen(r) > 0 : false
})
const groupsHasDrift = computed(() => {
  const r = report.value?.groups
  return r ? missingLen(r) > 0 || extraLen(r) > 0 : false
})
const totalExtra = computed(() => {
  const r = report.value
  if (!r) return 0
  return extraLen(r.items) + extraLen(r.users) + extraLen(r.groups)
})
</script>

<template>
  <main class="p-6 max-w-7xl mx-auto w-full">
    <header class="mb-6">
      <h1 class="text-2xl font-semibold">Catalog sync</h1>
      <p class="text-sm text-slate-400 mt-1 max-w-3xl">
        The controller's database is the source of truth for items and workers;
        JetStream KV is the projection that managed kiosks read from. If the
        broker was unreachable mid-save, KV can drift from the DB. This view
        diffs the two and offers one-button reconciliation.
      </p>
    </header>

    <div v-if="!isController" class="rounded-2xl bg-amber-950/40 border border-amber-800 p-4 text-amber-200">
      This view is only meaningful on the kiosk-controller. The current
      binary appears to be a kiosk.
    </div>

    <template v-else>
      <div class="flex gap-3 mb-4">
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-slate-800 hover:bg-slate-700 text-slate-200"
          :disabled="loading"
          @click="loadIntegrity"
        >
          {{ loading ? 'Loading…' : 'Refresh diff' }}
        </button>
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-brand-primary hover:bg-brand-primary-hover text-white font-medium"
          :disabled="reconciling || !report"
          @click="runReconcile(false)"
        >
          {{ reconciling ? 'Reconciling…' : 'Push DB → KV' }}
        </button>
        <button
          type="button"
          class="px-4 py-2 rounded-lg bg-red-700 hover:bg-red-600 disabled:opacity-40 text-white font-medium"
          :disabled="reconciling || !report || totalExtra === 0"
          :title="totalExtra === 0 ? 'No orphan KV keys to delete' : ''"
          @click="confirmDeleteOpen = true"
        >
          Push + delete {{ totalExtra }} orphan{{ totalExtra === 1 ? '' : 's' }}
        </button>
      </div>

      <p v-if="error" class="text-red-300 mb-4">{{ error }}</p>

      <div v-if="report" class="space-y-6">
        <!-- Items bucket -->
        <section class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
          <header class="px-5 py-3 border-b border-slate-800 flex items-baseline gap-3">
            <h2 class="text-lg font-semibold">Items bucket</h2>
            <code class="text-xs text-slate-500 font-mono">{{ report.items.bucket }}</code>
            <span class="ml-auto text-sm text-slate-400 tabular-nums">
              DB {{ report.items.expected_keys }} · KV {{ report.items.actual_keys }}
            </span>
          </header>
          <div v-if="!itemsHasDrift" class="px-5 py-6 text-center text-emerald-400">
            In sync — no drift detected.
          </div>
          <div v-else class="grid grid-cols-1 md:grid-cols-2 divide-y md:divide-y-0 md:divide-x divide-slate-800">
            <div class="p-5">
              <p class="text-sm text-slate-400 mb-2">
                Missing in KV
                <span class="text-amber-400 font-medium tabular-nums">
                  ({{ missingLen(report.items) }})
                </span>
              </p>
              <ul v-if="missingLen(report.items) > 0" class="text-sm font-mono text-slate-300 space-y-1 max-h-80 overflow-auto">
                <li v-for="k in safeList(report.items.missing_in_kv)" :key="`mi-${k}`">{{ k }}</li>
              </ul>
              <p v-else class="text-xs text-slate-500">None.</p>
            </div>
            <div class="p-5">
              <p class="text-sm text-slate-400 mb-2">
                Extra in KV (orphans)
                <span class="text-red-400 font-medium tabular-nums">
                  ({{ extraLen(report.items) }})
                </span>
              </p>
              <ul v-if="extraLen(report.items) > 0" class="text-sm font-mono text-slate-300 space-y-1 max-h-80 overflow-auto">
                <li v-for="k in safeList(report.items.extra_in_kv)" :key="`ei-${k}`">{{ k }}</li>
              </ul>
              <p v-else class="text-xs text-slate-500">None.</p>
            </div>
          </div>
        </section>

        <!-- Users bucket -->
        <section class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
          <header class="px-5 py-3 border-b border-slate-800 flex items-baseline gap-3">
            <h2 class="text-lg font-semibold">Users bucket</h2>
            <code class="text-xs text-slate-500 font-mono">{{ report.users.bucket }}</code>
            <span class="ml-auto text-sm text-slate-400 tabular-nums">
              DB {{ report.users.expected_keys }} · KV {{ report.users.actual_keys }}
            </span>
          </header>
          <div v-if="!usersHasDrift" class="px-5 py-6 text-center text-emerald-400">
            In sync — no drift detected.
          </div>
          <div v-else class="grid grid-cols-1 md:grid-cols-2 divide-y md:divide-y-0 md:divide-x divide-slate-800">
            <div class="p-5">
              <p class="text-sm text-slate-400 mb-2">
                Missing in KV
                <span class="text-amber-400 font-medium tabular-nums">
                  ({{ missingLen(report.users) }})
                </span>
              </p>
              <ul v-if="missingLen(report.users) > 0" class="text-sm font-mono text-slate-300 space-y-1 max-h-80 overflow-auto">
                <li v-for="k in safeList(report.users.missing_in_kv)" :key="`mu-${k}`">{{ k }}</li>
              </ul>
              <p v-else class="text-xs text-slate-500">None.</p>
            </div>
            <div class="p-5">
              <p class="text-sm text-slate-400 mb-2">
                Extra in KV (orphans)
                <span class="text-red-400 font-medium tabular-nums">
                  ({{ extraLen(report.users) }})
                </span>
              </p>
              <ul v-if="extraLen(report.users) > 0" class="text-sm font-mono text-slate-300 space-y-1 max-h-80 overflow-auto">
                <li v-for="k in safeList(report.users.extra_in_kv)" :key="`eu-${k}`">{{ k }}</li>
              </ul>
              <p v-else class="text-xs text-slate-500">None.</p>
            </div>
          </div>
        </section>

        <!-- Groups bucket -->
        <section v-if="report.groups" class="rounded-2xl bg-slate-900 border border-slate-800 overflow-hidden">
          <header class="px-5 py-3 border-b border-slate-800 flex items-baseline gap-3">
            <h2 class="text-lg font-semibold">Groups bucket</h2>
            <code class="text-xs text-slate-500 font-mono">{{ report.groups.bucket }}</code>
            <span class="ml-auto text-sm text-slate-400 tabular-nums">
              DB {{ report.groups.expected_keys }} · KV {{ report.groups.actual_keys }}
            </span>
          </header>
          <div v-if="!groupsHasDrift" class="px-5 py-6 text-center text-emerald-400">
            In sync — no drift detected.
          </div>
          <div v-else class="grid grid-cols-1 md:grid-cols-2 divide-y md:divide-y-0 md:divide-x divide-slate-800">
            <div class="p-5">
              <p class="text-sm text-slate-400 mb-2">
                Missing in KV
                <span class="text-amber-400 font-medium tabular-nums">
                  ({{ missingLen(report.groups) }})
                </span>
              </p>
              <ul v-if="missingLen(report.groups) > 0" class="text-sm font-mono text-slate-300 space-y-1 max-h-80 overflow-auto">
                <li v-for="k in safeList(report.groups.missing_in_kv)" :key="`mg-${k}`">{{ k }}</li>
              </ul>
              <p v-else class="text-xs text-slate-500">None.</p>
            </div>
            <div class="p-5">
              <p class="text-sm text-slate-400 mb-2">
                Extra in KV (orphans)
                <span class="text-red-400 font-medium tabular-nums">
                  ({{ extraLen(report.groups) }})
                </span>
              </p>
              <ul v-if="extraLen(report.groups) > 0" class="text-sm font-mono text-slate-300 space-y-1 max-h-80 overflow-auto">
                <li v-for="k in safeList(report.groups.extra_in_kv)" :key="`eg-${k}`">{{ k }}</li>
              </ul>
              <p v-else class="text-xs text-slate-500">None.</p>
            </div>
          </div>
        </section>

        <!-- Last reconcile errors, if any -->
        <section
          v-if="lastReconcile && (
            (lastReconcile.items.publish_errors?.length ?? 0) +
            (lastReconcile.items.delete_errors?.length ?? 0) +
            (lastReconcile.users.publish_errors?.length ?? 0) +
            (lastReconcile.users.delete_errors?.length ?? 0) +
            (lastReconcile.groups?.publish_errors?.length ?? 0) +
            (lastReconcile.groups?.delete_errors?.length ?? 0) > 0
          )"
          class="rounded-2xl bg-red-950/40 border border-red-800 p-5 text-sm"
        >
          <h2 class="text-base font-semibold text-red-200 mb-3">Last reconcile reported errors</h2>
          <div v-for="bucket in [
            { label: 'Items publish', errs: lastReconcile.items.publish_errors },
            { label: 'Items delete', errs: lastReconcile.items.delete_errors },
            { label: 'Users publish', errs: lastReconcile.users.publish_errors },
            { label: 'Users delete', errs: lastReconcile.users.delete_errors },
            { label: 'Groups publish', errs: lastReconcile.groups?.publish_errors },
            { label: 'Groups delete', errs: lastReconcile.groups?.delete_errors },
          ]" :key="bucket.label">
            <div v-if="bucket.errs && bucket.errs.length > 0" class="mb-2">
              <p class="text-red-300 font-medium">{{ bucket.label }}</p>
              <ul class="text-red-200 font-mono text-xs ml-4 list-disc">
                <li v-for="e in bucket.errs" :key="e">{{ e }}</li>
              </ul>
            </div>
          </div>
          <p class="text-red-300 mt-2 text-xs">
            Re-run reconcile to retry — each KV op is idempotent.
          </p>
        </section>
      </div>
    </template>

    <ConfirmDialog
      :open="confirmDeleteOpen"
      title="Delete KV orphans?"
      :message="`This will push the current DB state to KV AND delete ${totalExtra} KV key${totalExtra === 1 ? '' : 's'} that no longer have a backing DB record. Use after a controller rollback or when adopting a pre-existing bucket. Continue?`"
      confirm-label="Push + delete orphans"
      destructive
      @update:open="confirmDeleteOpen = $event"
      @confirm="runReconcile(true)"
    />
  </main>
</template>
