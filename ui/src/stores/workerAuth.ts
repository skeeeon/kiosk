import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { pbWorker } from '../lib/pb'

// useWorkerAuthStore wraps the `users` auth collection for the public virtual
// timeclock terminal (cmd/timeclock). Mirrors the admin auth store's reactive
// pattern (bump a tick on every authStore change) but talks to pbWorker — the
// persistent client — so a worker's session survives a phone reload.
//
// Two login paths, both against pb.collection('users'):
//   - OAuth2 SSO (authWithOAuth2 all-in-one popup flow), the recommended path.
//     The server-side match-only guard rejects any IdP account that isn't a
//     pre-provisioned worker.
//   - Password (authWithPassword by email), for orgs without SSO. Workers set
//     a password via PocketBase's reset-by-email flow (the catalog watcher
//     seeds a random one nobody knows).

// AuthMethods is the subset of pb.collection('users').listAuthMethods() the
// login screen needs: which providers to show and whether to offer password.
export interface OAuth2ProviderInfo {
  name: string
  displayName: string
}
export interface AuthMethods {
  passwordEnabled: boolean
  oauth2Providers: OAuth2ProviderInfo[]
}

export const useWorkerAuthStore = defineStore('workerAuth', () => {
  const tick = ref(0)
  pbWorker.authStore.onChange(() => {
    tick.value++
  })

  const isAuthenticated = computed(() => {
    void tick.value
    return pbWorker.authStore.isValid
  })

  const worker = computed(() => {
    void tick.value
    return pbWorker.authStore.record
  })

  async function listMethods(): Promise<AuthMethods> {
    const m = await pbWorker.collection('users').listAuthMethods()
    return {
      passwordEnabled: !!m.password?.enabled,
      oauth2Providers: (m.oauth2?.providers ?? []).map((p) => ({
        name: p.name,
        displayName: p.displayName || p.name,
      })),
    }
  }

  async function loginPassword(email: string, password: string) {
    await pbWorker.collection('users').authWithPassword(email, password)
  }

  async function loginOAuth2(provider: string) {
    await pbWorker.collection('users').authWithOAuth2({ provider })
  }

  async function requestPasswordReset(email: string) {
    await pbWorker.collection('users').requestPasswordReset(email)
  }

  function logout() {
    pbWorker.authStore.clear()
  }

  return { isAuthenticated, worker, listMethods, loginPassword, loginOAuth2, requestPasswordReset, logout }
})
