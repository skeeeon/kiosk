<script setup lang="ts">
import { ref } from 'vue'
import { storeToRefs } from 'pinia'
import ScanInput from '../components/ScanInput.vue'
import CartTable from '../components/CartTable.vue'
import ItemBrowseDialog from '../components/ItemBrowseDialog.vue'
import { useCart } from '../composables/useCart'
import { useSessionStore } from '../stores/session'
import type { CartAction, CommitResult, Item, User } from '../types'

const session = useSessionStore()
const { cart, flash } = storeToRefs(session)
const c = useCart()

const SUCCESS_SCREEN_MS = 3000
const success = ref<CommitResult | null>(null)
const committing = ref(false)
const browseOpen = ref(false)
const browsePending = ref(false)

async function onScan(raw: string) {
  let result
  try {
    result = await c.scanDispatch(raw)
  } catch (e) {
    session.setFlash('error', `Scan failed: ${(e as Error).message}`)
    return
  }

  if (result.type === 'unknown') {
    session.setFlash('warn', `Unknown code: ${result.value ?? raw}`)
    return
  }

  if (result.type === 'user') {
    const u = result.record as User
    if (cart.value && cart.value.user_id !== u.id) {
      session.setFlash(
        'warn',
        `${cart.value.user_name} is still active. Cancel or commit first.`,
      )
      return
    }
    try {
      await c.start(u.code)
      session.setFlash('info', `Welcome, ${u.name}`)
    } catch (e) {
      session.setFlash('error', (e as Error).message)
    }
    return
  }

  if (result.type === 'item') {
    if (!cart.value) {
      session.setFlash('warn', 'Scan your badge first')
      return
    }
    try {
      const line = await c.addItem((result.record as Item).code)
      if (line.warnings && line.warnings.length > 0) {
        const first = line.warnings[0]
        if (first.startsWith('cross_user_return:')) {
          session.setFlash('warn', `Returning ${first.slice('cross_user_return:'.length)}'s ${line.item_name}`)
        }
      }
    } catch (e) {
      session.setFlash('error', (e as Error).message)
    }
  }
}

async function onUpdate(id: string, patch: { qty?: number; action?: CartAction }) {
  try {
    await c.updateLine(id, patch)
  } catch (e) {
    session.setFlash('error', (e as Error).message)
  }
}

async function onRemove(id: string) {
  try {
    await c.deleteLine(id)
  } catch (e) {
    session.setFlash('error', (e as Error).message)
  }
}

async function onCancel() {
  try {
    await c.cancel()
    session.setFlash('info', 'Session ended')
  } catch (e) {
    session.setFlash('error', (e as Error).message)
  }
}

async function onBrowsePick(code: string) {
  if (!cart.value || browsePending.value) return
  browsePending.value = true
  try {
    const line = await c.addItem(code)
    if (line.warnings && line.warnings.length > 0) {
      const first = line.warnings[0]
      if (first.startsWith('cross_user_return:')) {
        session.setFlash('warn', `Returning ${first.slice('cross_user_return:'.length)}'s ${line.item_name}`)
      }
    } else {
      session.setFlash('info', `Added ${line.item_name}`)
    }
  } catch (e) {
    session.setFlash('error', (e as Error).message)
  } finally {
    browsePending.value = false
  }
}

async function onCommit() {
  if (committing.value) return
  committing.value = true
  try {
    const result = await c.commit()
    success.value = result
    setTimeout(() => {
      success.value = null
    }, SUCCESS_SCREEN_MS)
  } catch (e) {
    session.setFlash('error', (e as Error).message)
  } finally {
    committing.value = false
  }
}

const flashClasses = {
  info: 'bg-sky-900/60 border-sky-700/70 text-sky-100',
  warn: 'bg-amber-900/60 border-amber-700/70 text-amber-100',
  error: 'bg-red-900/60 border-red-700/70 text-red-100',
}
</script>

<template>
  <ScanInput @scan="onScan" />

  <Transition
    enter-active-class="transition duration-200 ease-out"
    enter-from-class="opacity-0 -translate-y-2"
    enter-to-class="opacity-100 translate-y-0"
    leave-active-class="transition duration-150 ease-in"
    leave-from-class="opacity-100"
    leave-to-class="opacity-0"
  >
    <div
      v-if="flash"
      class="fixed top-16 left-1/2 -translate-x-1/2 z-20 px-6 py-3 rounded-xl border text-lg shadow-lg"
      :class="flashClasses[flash.kind]"
    >
      {{ flash.text }}
    </div>
  </Transition>

  <main
    v-if="success"
    class="flex flex-col items-center justify-center px-8 py-16 text-center"
  >
    <div class="max-w-2xl">
      <p class="text-6xl font-bold text-emerald-400 mb-4">Done</p>
      <p class="text-2xl text-slate-300 mb-8">
        {{ success.lines_count }} line<span v-if="success.lines_count !== 1">s</span> committed
      </p>
      <div class="flex justify-center gap-6 text-lg text-slate-400">
        <span v-if="success.checked_out > 0">
          <span class="text-emerald-400 font-semibold">{{ success.checked_out }}</span> checked out
        </span>
        <span v-if="success.returned > 0">
          <span class="text-amber-400 font-semibold">{{ success.returned }}</span> returned
        </span>
        <span v-if="success.consumed > 0">
          <span class="text-sky-400 font-semibold">{{ success.consumed }}</span> consumed
        </span>
      </div>
    </div>
  </main>

  <main v-else-if="!cart" class="flex flex-col items-center justify-center px-8 py-16 text-center">
    <div class="max-w-2xl">
      <p class="text-5xl font-bold tracking-tight mb-4">Scan your badge to begin</p>
      <p class="text-xl text-slate-400">Or scan an item code to identify it.</p>
    </div>
  </main>

  <main v-else class="px-6 py-8 max-w-4xl mx-auto w-full">
    <header class="flex items-baseline justify-between mb-6">
      <div>
        <p class="text-sm text-slate-400">Signed in as</p>
        <h1 class="text-3xl font-semibold">{{ cart.user_name }}</h1>
        <p class="text-sm text-slate-500">{{ cart.user_code }}</p>
      </div>
      <p class="text-slate-500 text-sm">
        Cart {{ cart.lines.length }} line<span v-if="cart.lines.length !== 1">s</span>
      </p>
    </header>

    <p
      v-if="cart.lines.length === 0"
      class="rounded-2xl bg-slate-900 border border-slate-800 border-dashed text-slate-500 text-center py-12 text-lg"
    >
      Scan items to add them to your cart.
    </p>

    <CartTable v-else :lines="cart.lines" @update="onUpdate" @remove="onRemove" />

    <div class="mt-8 flex gap-3 justify-end">
      <button
        type="button"
        class="px-6 py-4 rounded-xl bg-slate-800 hover:bg-slate-700 text-lg mr-auto"
        @click="browseOpen = true"
      >
        Browse items
      </button>
      <button
        type="button"
        class="px-6 py-4 rounded-xl bg-slate-800 hover:bg-slate-700 text-lg"
        @click="onCancel"
      >
        Cancel
      </button>
      <button
        type="button"
        class="px-8 py-4 rounded-xl bg-emerald-600 hover:bg-emerald-500 disabled:bg-slate-700 disabled:text-slate-500 text-lg font-semibold"
        :disabled="cart.lines.length === 0 || committing"
        @click="onCommit"
      >
        {{ committing ? 'Committing…' : 'Commit' }}
      </button>
    </div>
  </main>

  <ItemBrowseDialog
    :open="browseOpen"
    :pending="browsePending"
    @update:open="browseOpen = $event"
    @pick="onBrowsePick"
    @error="session.setFlash('error', $event)"
  />
</template>
