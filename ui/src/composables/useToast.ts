import { ref } from 'vue'

// Module-level (singleton) state so toasts can fire from anywhere and the
// single <AppToast /> mounted in App.vue renders them. One toast visible at
// a time — a newer call clears the old timer and replaces the content.
export type ToastKind = 'success' | 'error' | 'warn' | 'info'
export type ToastPosition = 'top' | 'bottom-right'

interface Toast {
  kind: ToastKind
  text: string
  position: ToastPosition
  // Increment-only seed used as a Vue list key; Date.now() can collide if
  // two toasts fire in the same tick.
  seq: number
}

interface ShowOptions {
  position?: ToastPosition
  duration?: number
}

const DEFAULT_DURATION: Record<ToastKind, number> = {
  success: 2500,
  info: 2500,
  warn: 3500,
  error: 6000,
}

const current = ref<Toast | null>(null)
let nextSeq = 1
let timer: ReturnType<typeof setTimeout> | null = null

function show(kind: ToastKind, text: string, opts: ShowOptions = {}) {
  if (timer) clearTimeout(timer)
  current.value = {
    kind,
    text,
    position: opts.position ?? 'bottom-right',
    seq: nextSeq++,
  }
  const ms = opts.duration ?? DEFAULT_DURATION[kind]
  timer = setTimeout(() => {
    current.value = null
    timer = null
  }, ms)
}

export function useToast() {
  return {
    current,
    success: (text: string, opts?: ShowOptions) => show('success', text, opts),
    error: (text: string, opts?: ShowOptions) => show('error', text, opts),
    warn: (text: string, opts?: ShowOptions) => show('warn', text, opts),
    info: (text: string, opts?: ShowOptions) => show('info', text, opts),
    dismiss: () => {
      if (timer) clearTimeout(timer)
      current.value = null
      timer = null
    },
  }
}
