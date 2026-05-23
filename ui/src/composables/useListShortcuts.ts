import { onMounted, onUnmounted, type Ref, type ComputedRef } from 'vue'

// useListShortcuts wires two keyboard shortcuts for admin list views:
//   "/"  → focuses the search input and selects its current contents
//   "n"  → invokes the create-new callback (if provided + allowed)
//
// The listener mirrors useScan's input-focus check so it never steals
// keys from a focused <input>, <textarea>, <select>, or contenteditable
// element. Modifier-key combos (Ctrl/Cmd/Alt) are passed through so
// browser shortcuts like Cmd+N keep working.
//
// Multiple list views can mount this concurrently (e.g. via tabbed
// detail panels) — there is no single-mount guard because, unlike the
// scan listener, double-dispatch here is benign (refocusing an already-
// focused input or opening an already-open dialog is a no-op).

export interface ListShortcutsOptions {
  // The search <input> to focus on "/". Bind via :ref="searchInput" on the
  // template element; the composable handles the null case during teardown.
  searchInput: Ref<HTMLInputElement | null>
  // Invoked on "n". Omit to disable the shortcut.
  onNew?: () => void
  // Runtime gate for onNew — e.g. a managed-mode flag where create is
  // not allowed. When false, "n" is a no-op.
  canCreate?: ComputedRef<boolean> | Ref<boolean>
}

function isEditableTarget(el: EventTarget | null): boolean {
  if (!(el instanceof HTMLElement)) return false
  if (el.isContentEditable) return true
  const tag = el.tagName
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT'
}

export function useListShortcuts(options: ListShortcutsOptions) {
  function onKey(e: KeyboardEvent) {
    if (e.ctrlKey || e.metaKey || e.altKey) return
    if (isEditableTarget(e.target)) return

    if (e.key === '/') {
      const input = options.searchInput.value
      if (input) {
        e.preventDefault()
        input.focus()
        input.select()
      }
      return
    }

    if (e.key === 'n' && options.onNew) {
      const allowed = options.canCreate ? options.canCreate.value : true
      if (!allowed) return
      e.preventDefault()
      options.onNew()
    }
  }

  onMounted(() => window.addEventListener('keydown', onKey))
  onUnmounted(() => window.removeEventListener('keydown', onKey))
}
