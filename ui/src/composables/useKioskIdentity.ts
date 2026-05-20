import { onMounted, ref } from 'vue'
import { api } from '../lib/api'
import type { KioskIdentity } from '../types'

// Module-level cache so multiple components share the single fetch.
const identity = ref<KioskIdentity | null>(null)
let fetched = false

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

export function useKioskIdentity() {
  onMounted(async () => {
    if (fetched) return
    fetched = true
    try {
      const fresh = await api.get<KioskIdentity>('/api/kiosk/identity')
      identity.value = fresh
      applyBranding(fresh)
    } catch {
      fetched = false // allow retry on remount
    }
  })
  return { identity }
}
