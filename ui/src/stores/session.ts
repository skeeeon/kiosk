import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { Cart, CartLine } from '../types'

// Session store: the active cart (if any) and a transient flash message used
// to surface scan errors ("Unknown code", "Scan badge first") and warnings.
export const useSessionStore = defineStore('session', () => {
  const cart = ref<Cart | null>(null)
  const flash = ref<{ kind: 'info' | 'warn' | 'error'; text: string } | null>(null)

  let flashTimer: ReturnType<typeof setTimeout> | null = null

  function setCart(c: Cart | null) {
    cart.value = c
  }

  function setFlash(kind: 'info' | 'warn' | 'error', text: string, ms = 3500) {
    if (flashTimer) clearTimeout(flashTimer)
    flash.value = { kind, text }
    flashTimer = setTimeout(() => {
      flash.value = null
      flashTimer = null
    }, ms)
  }

  function clearFlash() {
    if (flashTimer) clearTimeout(flashTimer)
    flashTimer = null
    flash.value = null
  }

  function findLine(id: string): CartLine | undefined {
    return cart.value?.lines.find((l) => l.id === id)
  }

  return { cart, flash, setCart, setFlash, clearFlash, findLine }
})
