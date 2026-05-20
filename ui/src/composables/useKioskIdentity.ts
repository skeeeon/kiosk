import { onMounted, ref } from 'vue'
import { api } from '../lib/api'
import type { KioskIdentity } from '../types'

// Module-level cache so multiple components share the single fetch.
const identity = ref<KioskIdentity | null>(null)
let fetched = false

export function useKioskIdentity() {
  onMounted(async () => {
    if (fetched) return
    fetched = true
    try {
      identity.value = await api.get<KioskIdentity>('/api/kiosk/identity')
    } catch {
      fetched = false // allow retry on remount
    }
  })
  return { identity }
}
