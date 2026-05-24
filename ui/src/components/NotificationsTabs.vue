<script setup lang="ts">
// Tab strip shared by the three notification sub-views: templates,
// scheduled reports, and the send log. Each view renders this at the top
// of its <main> so they read as one feature with three tabs from the
// operator's perspective, while staying independent components. Styling
// matches the in-component tabs on AdminReportsView so the two tab
// strips read as one design.
//
// We compute the active class instead of using RouterLink's `active-class`
// prop: that prop appends the active class to the base class string, which
// leaves both `border-transparent` and `border-brand-primary` in the DOM
// and lets Tailwind's CSS-emit order decide which wins (it picks
// border-transparent on this codebase). Returning only one of the two from
// a function avoids the ordering battle entirely.
import { useRoute } from 'vue-router'

const route = useRoute()

const tabs = [
  { name: 'admin-notifications', label: 'Templates' },
  { name: 'admin-notifications-scheduled', label: 'Scheduled' },
  { name: 'admin-notifications-log', label: 'Recent sends' },
] as const

function tabClasses(name: string) {
  return route.name === name
    ? 'border-brand-primary text-slate-100'
    : 'border-transparent text-slate-400 hover:text-slate-200'
}
</script>

<template>
  <nav class="flex gap-1 mb-4 border-b border-slate-800">
    <RouterLink
      v-for="t in tabs"
      :key="t.name"
      :to="{ name: t.name }"
      class="px-4 py-2 border-b-2 transition-colors"
      :class="tabClasses(t.name)"
    >
      {{ t.label }}
    </RouterLink>
  </nav>
</template>
