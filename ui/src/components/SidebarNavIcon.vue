<!-- SidebarNavIcon switches on route name to render the matching icon.
     20x20 viewBox keeps it consistent with the inline SVGs already used in
     the layout. Keeping all icons in one file means adding a nav item is a
     one-spot change. -->
<script setup lang="ts">
defineProps<{ name: string }>()
</script>

<template>
  <svg viewBox="0 0 20 20" fill="currentColor" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">
    <!-- Kiosks: computer-desktop -->
    <path
      v-if="name === 'admin-kiosks'"
      d="M3 4a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2h-3v2h2a.5.5 0 0 1 0 1H6a.5.5 0 0 1 0-1h2v-2H5a2 2 0 0 1-2-2V4Zm2 0v7h10V4H5Z"
    />
    <!-- Transactions: clipboard-document-list -->
    <path
      v-else-if="name === 'admin-transactions'"
      d="M8 2a1 1 0 0 0-1 1v1H6a2 2 0 0 0-2 2v10a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V6a2 2 0 0 0-2-2h-1V3a1 1 0 0 0-1-1H8Zm0 5h4a.5.5 0 0 1 0 1H8a.5.5 0 0 1 0-1Zm0 3h4a.5.5 0 0 1 0 1H8a.5.5 0 0 1 0-1Zm0 3h4a.5.5 0 0 1 0 1H8a.5.5 0 0 1 0-1Zm-2-6a.5.5 0 1 1 0 1 .5.5 0 0 1 0-1Zm0 3a.5.5 0 1 1 0 1 .5.5 0 0 1 0-1Zm0 3a.5.5 0 1 1 0 1 .5.5 0 0 1 0-1Z"
    />
    <!-- Items: cube -->
    <path
      v-else-if="name === 'admin-items'"
      d="M10 1.5 2.5 5v10L10 18.5 17.5 15V5L10 1.5Zm0 2.1 5.1 2.4L10 8.4 4.9 6 10 3.6Zm-6 4 5 2.3v7L4 14.5v-6.9Zm12 0v6.9l-5 2.3v-7l5-2.3Z"
    />
    <!-- Workers: users -->
    <path
      v-else-if="name === 'admin-users'"
      d="M7 8a3 3 0 1 0 0-6 3 3 0 0 0 0 6Zm6 1a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5ZM1.5 17a5.5 5.5 0 0 1 11 0v1h-11v-1Zm12.5-1c0-1.04-.25-2.02-.7-2.88A4 4 0 0 1 18.5 17v1H14v-1c0-.35-.02-.69-.07-1.02.04-.06.06-.13.07-.2V16Z"
    />
    <!-- Groups: user-group / building -->
    <path
      v-else-if="name === 'admin-groups'"
      d="M3 4a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v13a1 1 0 0 1-1 1h-4v-3a2 2 0 0 0-4 0v3H4a1 1 0 0 1-1-1V4Zm3 1.5a.5.5 0 0 0 0 1h1a.5.5 0 0 0 0-1H6Zm3 0a.5.5 0 0 0 0 1h1a.5.5 0 0 0 0-1H9Zm3 0a.5.5 0 0 0 0 1h1a.5.5 0 0 0 0-1h-1Zm-6 3a.5.5 0 0 0 0 1h1a.5.5 0 0 0 0-1H6Zm3 0a.5.5 0 0 0 0 1h1a.5.5 0 0 0 0-1H9Zm3 0a.5.5 0 0 0 0 1h1a.5.5 0 0 0 0-1h-1Zm-6 3a.5.5 0 0 0 0 1h1a.5.5 0 0 0 0-1H6Zm3 0a.5.5 0 0 0 0 1h1a.5.5 0 0 0 0-1H9Zm3 0a.5.5 0 0 0 0 1h1a.5.5 0 0 0 0-1h-1Z"
    />
    <!-- Reports: chart-bar -->
    <path
      v-else-if="name === 'admin-reports'"
      d="M3 17h14v1H3v-1Zm2-2V9h2v6H5Zm4 0V5h2v10H9Zm4 0v-4h2v4h-2Z"
    />
    <!-- Metrics: gauge / chart-pie -->
    <path
      v-else-if="name === 'admin-metrics'"
      d="M10 2a8 8 0 1 0 8 8 .75.75 0 0 0-.75-.75H10.75V2.75A.75.75 0 0 0 10 2Zm-1.5 1.62A6.5 6.5 0 1 0 16.38 11.5H9.25a.75.75 0 0 1-.75-.75V3.62Z"
    />
    <!-- Notifications: bell -->
    <path
      v-else-if="name === 'admin-notifications'"
      d="M10 2a1 1 0 0 0-1 1v.7A5 5 0 0 0 5 8.5V13l-1.5 2v1h13v-1L15 13V8.5a5 5 0 0 0-4-4.8V3a1 1 0 0 0-1-1Zm-2 16a2 2 0 1 0 4 0H8Z"
    />
    <!-- Catalog sync: arrow-path -->
    <path
      v-else-if="name === 'admin-catalog-sync'"
      d="M4.5 9a5.5 5.5 0 0 1 9.4-3.9l1.4-1.4v4H11.3l1.4-1.4A4 4 0 0 0 6 9H4.5Zm11 2a5.5 5.5 0 0 1-9.4 3.9l-1.4 1.4v-4h4l-1.4 1.4A4 4 0 0 0 14 11h1.5Z"
    />
    <!-- Import: arrow-up-tray -->
    <path
      v-else-if="name === 'admin-import'"
      d="M10 2.5 6.5 6h2.25v6h2.5V6h2.25L10 2.5Zm-6 11h12v3a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1v-3Z"
    />
    <!-- Admins: shield-check -->
    <path
      v-else-if="name === 'admin-admins'"
      d="M10 1.5 3.5 4v5.5C3.5 13.6 6.3 17 10 18c3.7-1 6.5-4.4 6.5-8.5V4L10 1.5Zm3.4 6.6-4 5a.75.75 0 0 1-1.1.07l-2-2a.75.75 0 1 1 1.06-1.06l1.4 1.4 3.46-4.34a.75.75 0 1 1 1.18.93Z"
    />
  </svg>
</template>
