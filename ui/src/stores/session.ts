import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { Cart, CartLine } from '../types'

// Session store: the active cart (if any). Transient user-facing messages
// now go through useToast (mounted globally in App.vue).
export const useSessionStore = defineStore('session', () => {
  const cart = ref<Cart | null>(null)

  function setCart(c: Cart | null) {
    cart.value = c
  }

  function findLine(id: string): CartLine | undefined {
    return cart.value?.lines.find((l) => l.id === id)
  }

  return { cart, setCart, findLine }
})
