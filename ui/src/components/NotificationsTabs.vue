<script setup lang="ts">
// Tab strip shared by the three notification sub-views: templates,
// scheduled reports, and the send log. Each view renders this at the top
// of its <main> so they read as one feature with three tabs from the
// operator's perspective, while staying independent components.
</script>

<template>
  <nav class="flex items-center gap-1 mb-5 border-b border-slate-800">
    <RouterLink
      :to="{ name: 'admin-notifications' }"
      class="px-3 py-2 -mb-px border-b-2 border-transparent text-slate-400 hover:text-slate-200 text-sm"
      active-class="border-brand-primary text-slate-100"
    >
      Templates
    </RouterLink>
    <RouterLink
      :to="{ name: 'admin-notifications-scheduled' }"
      class="px-3 py-2 -mb-px border-b-2 border-transparent text-slate-400 hover:text-slate-200 text-sm"
      active-class="border-brand-primary text-slate-100"
    >
      Scheduled
    </RouterLink>
    <RouterLink
      :to="{ name: 'admin-notifications-log' }"
      class="px-3 py-2 -mb-px border-b-2 border-transparent text-slate-400 hover:text-slate-200 text-sm"
      active-class="border-brand-primary text-slate-100"
    >
      Recent sends
    </RouterLink>
  </nav>
</template>
