import { onMounted, ref } from 'vue'
import { api } from '../lib/api'
import type { KioskIdentity } from '../types'

// Module-level cache so multiple components share the single fetch.
const identity = ref<KioskIdentity | null>(null)
let loadPromise: Promise<KioskIdentity | null> | null = null

// applyBranding writes the runtime overrides from the kiosk's config into
// CSS variables on :root. Tailwind's @theme block in style.css declares the
// default values; writing here at runtime overrides them everywhere the
// theme variable is referenced. No-op when a field is empty.
function applyBranding(id: KioskIdentity) {
  const root = document.documentElement
  const primary = id.branding?.primary_color?.trim()
  if (primary) {
    root.style.setProperty('--color-brand-primary', primary)
    // Without a derived hover (see style.css comment), reuse primary for
    // hover so themed buttons stay visually consistent rather than briefly
    // jumping back to emerald.
    root.style.setProperty('--color-brand-primary-hover', primary)
  }
}

// loadKioskIdentity fetches identity once and caches it. Subsequent callers
// (composable, router guard) share the same in-flight promise. Exposed so
// the router's beforeEach can await identity before deciding where to land.
export function loadKioskIdentity(): Promise<KioskIdentity | null> {
  if (identity.value) return Promise.resolve(identity.value)
  if (loadPromise) return loadPromise
  loadPromise = (async () => {
    try {
      const fresh = await api.get<KioskIdentity>('/api/kiosk/identity')
      identity.value = fresh
      applyBranding(fresh)
      return fresh
    } catch {
      loadPromise = null // allow retry on next caller
      return null
    }
  })()
  return loadPromise
}

export function useKioskIdentity() {
  onMounted(() => {
    void loadKioskIdentity()
  })
  return { identity }
}
