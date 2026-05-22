import { onMounted, onUnmounted } from 'vue'

// useScan installs a window-level keydown listener that buffers characters
// and fires `onScan(buffer)` on Enter. Designed for a USB-HID barcode scanner
// emulating a keyboard.
//
// The listener is suppressed when a real input/textarea/select has focus
// so admin login forms and search boxes behave normally. For the kiosk
// checkout flow nothing is focused, so every scanner keystroke is captured.
//
// Single-mount guard: only one useScan call may be active at a time. A
// second mount logs a warning and becomes a no-op rather than installing a
// duplicate window listener (which would fire each scan into two handlers).
// Today only ScanInput uses useScan; the guard exists so a future view that
// forgets the contract fails loud instead of silently double-dispatching.
let scanActive = false

export function useScan(onScan: (value: string) => void) {
  let buffer = ''
  let installed = false

  function handleKey(e: KeyboardEvent) {
    const target = e.target as HTMLElement | null
    if (target) {
      const tag = target.tagName
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || target.isContentEditable) {
        return
      }
    }
    if (e.key === 'Enter') {
      const value = buffer
      buffer = ''
      if (value) onScan(value)
      e.preventDefault()
      return
    }
    if (e.key.length === 1) {
      buffer += e.key
    }
  }

  onMounted(() => {
    if (scanActive) {
      console.warn('useScan: a second mount was attempted; ignoring. Only one component may own the window scan listener.')
      return
    }
    scanActive = true
    installed = true
    window.addEventListener('keydown', handleKey)
  })
  onUnmounted(() => {
    if (!installed) return
    window.removeEventListener('keydown', handleKey)
    scanActive = false
    installed = false
  })
}
