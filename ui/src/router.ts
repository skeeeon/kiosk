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
import AdminNotificationsView from './views/AdminNotificationsView.vue'
import AdminKiosksView from './views/AdminKiosksView.vue'
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
        { path: '', redirect: { name: 'admin-items' } },
        { path: 'items', name: 'admin-items', component: AdminItemsView },
        { path: 'users', name: 'admin-users', component: AdminUsersView },
        { path: 'admins', name: 'admin-admins', component: AdminAdminsView },
        { path: 'groups', name: 'admin-groups', component: AdminGroupsView },
        { path: 'import', name: 'admin-import', component: AdminImportView },
        { path: 'reports', name: 'admin-reports', component: AdminReportsView },
        { path: 'notifications', name: 'admin-notifications', component: AdminNotificationsView },
        // Controller-only views. Nav links only render when role=controller,
        // but the routes are always registered so deep-links work on the
        // controller binary. On the kiosk binary the views render but their
        // queries hit empty / nonexistent data.
        { path: 'kiosks', name: 'admin-kiosks', component: AdminKiosksView },
        { path: 'transactions', name: 'admin-transactions', component: AdminTransactionsView },
        { path: 'catalog-sync', name: 'admin-catalog-sync', component: AdminCatalogSyncView },
      ],
    },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  if (to.meta.requiresAdmin && !auth.isAuthenticated) {
    return { name: 'admin-login', query: { redirect: to.fullPath } }
  }
  if (to.name === 'admin-login' && auth.isAuthenticated) {
    return { name: 'admin-items' }
  }
  // On the controller binary there is no checkout flow — operators always
  // belong in /admin. Redirect at the root; deep links into /admin work as-is.
  if (to.name === 'checkout') {
    const id = await loadKioskIdentity()
    if (id?.role === 'controller') {
      return { name: auth.isAuthenticated ? 'admin-items' : 'admin-login' }
    }
  }
})
