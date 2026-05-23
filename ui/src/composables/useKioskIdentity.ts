import { onMounted, ref } from 'vue'
import { api } from '../lib/api'
import type { KioskIdentity } from '../types'

// Module-level cache so multiple components share the single fetch.
const identity = ref<KioskIdentity | null>(null)
let loadPromise: Promise<KioskIdentity | null> | null = null

// CUSTOM_CSS_LINK_ID identifies our injected <link> so re-applying branding
// (in tests, on a forced reload, etc.) doesn't stack multiple stylesheets
// for the same custom CSS file.
const CUSTOM_CSS_LINK_ID = 'kiosk-branding-custom-css'

// applyBranding writes the runtime overrides from the kiosk's config into
// CSS variables on :root and, when configured, injects the operator's
// custom CSS file as a <link rel="stylesheet"> after Tailwind. Tailwind's
// @theme block in style.css declares the default variables; the runtime
// overrides cascade through every utility that references them.
// No-op when a field is empty.
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

  // Custom CSS escape hatch. The server reports the URL only when a file is
  // configured, so absence here means the operator didn't opt in. We append
  // at the end of <head> so the link lands AFTER Vite's Tailwind stylesheet
  // in source order — equal-specificity rules in the custom file then win
  // the cascade. The link is idempotent via id.
  const customURL = id.branding?.custom_css_url?.trim()
  if (customURL) {
    const existing = document.getElementById(CUSTOM_CSS_LINK_ID) as HTMLLinkElement | null
    if (existing) {
      if (existing.href !== customURL) existing.href = customURL
    } else {
      const link = document.createElement('link')
      link.id = CUSTOM_CSS_LINK_ID
      link.rel = 'stylesheet'
      link.href = customURL
      document.head.appendChild(link)
    }
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
