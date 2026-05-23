import { watch, type Ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

// useUrlQuerySync ties a set of refs to the route's query string.
//
//   useUrlQuerySync({
//     page: { ref: page, default: 1, parse: (v) => Number(v) || 1 },
//     q:    { ref: search, default: '' },
//     type: { ref: typeFilter, default: 'all' },
//   })
//
// On setup it hydrates each ref from the URL (one-shot — back/forward navigation
// to a different fullPath gets a fresh component, so we don't try to handle
// in-place query-only changes). On ref changes it pushes the merged query via
// router.replace so refresh + share-link work, without polluting history.
//
// A key whose value equals its default is omitted from the URL — empty searches
// and page=1 don't show up. Empty string and null/undefined are also treated
// as "no value present" for convenience.

export type QueryField<T> = {
  ref: Ref<T>
  default: T
  parse?: (raw: string) => T
  serialize?: (value: T) => string
}

export function useUrlQuerySync(fields: Record<string, QueryField<unknown>>): void {
  const route = useRoute()
  const router = useRouter()

  // Hydrate.
  for (const [key, f] of Object.entries(fields)) {
    const raw = route.query[key]
    if (raw === undefined || raw === null) continue
    const v = Array.isArray(raw) ? raw[0] : raw
    if (v === null) continue
    f.ref.value = f.parse ? f.parse(v) : (v as unknown)
  }

  // Sync ref → URL. Watching the array of refs lets us coalesce updates so
  // changing two refs in the same tick produces one router.replace.
  const refs = Object.values(fields).map((f) => f.ref)
  watch(refs, () => {
    const next: Record<string, string | undefined> = { ...route.query } as Record<
      string,
      string | undefined
    >
    for (const [key, f] of Object.entries(fields)) {
      const v = f.ref.value
      const isEmpty = v === '' || v === null || v === undefined
      if (isEmpty || v === f.default) {
        delete next[key]
      } else {
        next[key] = f.serialize ? f.serialize(v) : String(v)
      }
    }
    void router.replace({ query: next })
  })
}
