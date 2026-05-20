import { ref } from 'vue'

// Module-level (singleton) state so toasts can fire from anywhere and any
// component that mounts <AdminToast /> can render them. Admin views are all
// inside one <RouterView> so only one mount happens at a time.
export type ToastKind = 'success' | 'error'

interface Toast {
  kind: ToastKind
  text: string
  // Increment-only seed used as a Vue list key; Date.now() can collide if
  // two toasts fire in the same tick.
  seq: number
}

const current = ref<Toast | null>(null)
let nextSeq = 1
let timer: ReturnType<typeof setTimeout> | null = null

function show(kind: ToastKind, text: string, ms = 2500) {
  if (timer) clearTimeout(timer)
  current.value = { kind, text, seq: nextSeq++ }
  timer = setTimeout(() => {
    current.value = null
    timer = null
  }, ms)
}

export function useAdminToast() {
  return {
    current,
    success: (text: string) => show('success', text),
    error: (text: string) => show('error', text, 4000),
    dismiss: () => {
      if (timer) clearTimeout(timer)
      current.value = null
      timer = null
    },
  }
}
