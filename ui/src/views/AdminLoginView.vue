<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()
const router = useRouter()
const route = useRoute()

const email = ref('')
const password = ref('')
const error = ref<string | null>(null)
const submitting = ref(false)

async function onSubmit() {
  if (submitting.value) return
  submitting.value = true
  error.value = null
  try {
    await auth.login(email.value, password.value)
    const target = (route.query.redirect as string) || '/admin/items'
    router.replace(target)
  } catch (e) {
    error.value = (e as Error).message || 'Login failed'
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <main class="flex flex-col items-center justify-center px-8 py-16">
    <form
      class="w-full max-w-md flex flex-col gap-4 bg-slate-900 border border-slate-800 rounded-2xl p-8"
      @submit.prevent="onSubmit"
    >
      <h1 class="text-2xl font-semibold mb-2">Admin login</h1>

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Email</span>
        <input
          v-model="email"
          type="email"
          required
          autocomplete="username"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
        />
      </label>

      <label class="flex flex-col gap-1">
        <span class="text-sm text-slate-400">Password</span>
        <input
          v-model="password"
          type="password"
          required
          autocomplete="current-password"
          class="rounded-lg bg-slate-800 border border-slate-700 px-3 py-2 text-slate-100"
        />
      </label>

      <p v-if="error" class="text-red-400 text-sm">{{ error }}</p>

      <button
        type="submit"
        class="mt-2 px-4 py-3 rounded-lg bg-brand-primary hover:bg-brand-primary-hover disabled:bg-slate-700 text-white font-medium"
        :disabled="submitting"
      >
        {{ submitting ? 'Signing in…' : 'Sign in' }}
      </button>

      <RouterLink
        to="/"
        class="text-sm text-slate-500 hover:text-slate-300 text-center"
      >
        Back to kiosk
      </RouterLink>
    </form>
  </main>
</template>
