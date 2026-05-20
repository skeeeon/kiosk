import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { pb } from '../lib/pb'

// useAuthStore exposes pb.authStore as reactive refs by bumping `tick` on
// every authStore change. Vue's reactivity picks up the computed re-reads.
export const useAuthStore = defineStore('auth', () => {
  const tick = ref(0)
  pb.authStore.onChange(() => {
    tick.value++
  })

  const isAuthenticated = computed(() => {
    void tick.value
    return pb.authStore.isValid
  })

  const admin = computed(() => {
    void tick.value
    return pb.authStore.model
  })

  async function login(email: string, password: string) {
    await pb.collection('admins').authWithPassword(email, password)
  }

  function logout() {
    pb.authStore.clear()
  }

  return { isAuthenticated, admin, login, logout }
})
