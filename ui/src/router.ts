import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from './stores/auth'
import { loadKioskIdentity } from './composables/useKioskIdentity'

import CheckoutView from './views/CheckoutView.vue'
import AdminLoginView from './views/AdminLoginView.vue'
import AdminLayout from './views/AdminLayout.vue'
import AdminItemsView from './views/AdminItemsView.vue'
import AdminUsersView from './views/AdminUsersView.vue'
import AdminAdminsView from './views/AdminAdminsView.vue'
import AdminGroupsView from './views/AdminGroupsView.vue'
import AdminImportView from './views/AdminImportView.vue'
import AdminReportsView from './views/AdminReportsView.vue'
import AdminMetricsView from './views/AdminMetricsView.vue'
import AdminNotificationsView from './views/AdminNotificationsView.vue'
import AdminNotificationsLogView from './views/AdminNotificationsLogView.vue'
import AdminScheduledReportsView from './views/AdminScheduledReportsView.vue'
import AdminKiosksView from './views/AdminKiosksView.vue'
import AdminKioskDetailView from './views/AdminKioskDetailView.vue'
import AdminTransactionsView from './views/AdminTransactionsView.vue'
import AdminCatalogSyncView from './views/AdminCatalogSyncView.vue'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'checkout', component: CheckoutView },
    { path: '/admin/login', name: 'admin-login', component: AdminLoginView },
    {
      path: '/admin',
      component: AdminLayout,
      meta: { requiresAdmin: true },
      children: [
        // Resolved by the role-aware redirect in beforeEach. The empty
        // component is a placeholder vue-router needs for the route to
        // register; the guard always redirects before render.
        { path: '', name: 'admin-root', component: { render: () => null } },
        { path: 'items', name: 'admin-items', component: AdminItemsView },
        { path: 'users', name: 'admin-users', component: AdminUsersView },
        { path: 'admins', name: 'admin-admins', component: AdminAdminsView },
        { path: 'groups', name: 'admin-groups', component: AdminGroupsView },
        { path: 'import', name: 'admin-import', component: AdminImportView },
        { path: 'reports', name: 'admin-reports', component: AdminReportsView },
        { path: 'metrics', name: 'admin-metrics', component: AdminMetricsView },
        { path: 'notifications', name: 'admin-notifications', component: AdminNotificationsView },
        { path: 'notifications/scheduled', name: 'admin-notifications-scheduled', component: AdminScheduledReportsView },
        { path: 'notifications/log', name: 'admin-notifications-log', component: AdminNotificationsLogView },
        // Controller-only views. Nav links only render when role=controller,
        // but the routes are always registered so deep-links work on the
        // controller binary. On the kiosk binary the views render but their
        // queries hit empty / nonexistent data.
        { path: 'kiosks', name: 'admin-kiosks', component: AdminKiosksView },
        { path: 'kiosks/:code', name: 'admin-kiosk-detail', component: AdminKioskDetailView, props: true },
        { path: 'transactions', name: 'admin-transactions', component: AdminTransactionsView },
        { path: 'catalog-sync', name: 'admin-catalog-sync', component: AdminCatalogSyncView },
      ],
    },
  ],
})

// landingRoute is the admin's home tab once authenticated. On the
// controller the operator's job is fleet management, so they land on
// Kiosks; on the kiosk the catalog is what they're there to maintain.
async function landingRoute(): Promise<string> {
  const id = await loadKioskIdentity()
  return id?.role === 'controller' ? 'admin-kiosks' : 'admin-items'
}

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  if (to.meta.requiresAdmin && !auth.isAuthenticated) {
    return { name: 'admin-login', query: { redirect: to.fullPath } }
  }
  if (to.name === 'admin-login' && auth.isAuthenticated) {
    return { name: await landingRoute() }
  }
  // /admin/ with no child route — redirect to the role-appropriate landing.
  if (to.name === 'admin-root') {
    return { name: await landingRoute() }
  }
  // On the controller binary there is no checkout flow — operators always
  // belong in /admin. Redirect at the root; deep links into /admin work as-is.
  if (to.name === 'checkout') {
    const id = await loadKioskIdentity()
    if (id?.role === 'controller') {
      return { name: auth.isAuthenticated ? await landingRoute() : 'admin-login' }
    }
  }
})
