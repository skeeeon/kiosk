import { onMounted, onUnmounted } from 'vue'

// useScan installs a window-level keydown listener that buffers characters
// and fires `onScan(buffer)` on Enter. Designed for a USB-HID barcode scanner
// emulating a keyboard.
//
// The listener is suppressed when a real input/textarea/select has focus
// so admin login forms and search boxes behave normally. For the kiosk
// checkout flow nothing is focused, so every scanner keystroke is captured.
export function useScan(onScan: (value: string) => void) {
  let buffer = ''

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

  onMounted(() => window.addEventListener('keydown', handleKey))
  onUnmounted(() => window.removeEventListener('keydown', handleKey))
}
