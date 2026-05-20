import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from './stores/auth'

import CheckoutView from './views/CheckoutView.vue'
import AdminLoginView from './views/AdminLoginView.vue'
import AdminLayout from './views/AdminLayout.vue'
import AdminItemsView from './views/AdminItemsView.vue'
import AdminUsersView from './views/AdminUsersView.vue'
import AdminImportView from './views/AdminImportView.vue'
import AdminReportsView from './views/AdminReportsView.vue'

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
        { path: 'import', name: 'admin-import', component: AdminImportView },
        { path: 'reports', name: 'admin-reports', component: AdminReportsView },
      ],
    },
  ],
})

router.beforeEach((to) => {
  const auth = useAuthStore()
  if (to.meta.requiresAdmin && !auth.isAuthenticated) {
    return { name: 'admin-login', query: { redirect: to.fullPath } }
  }
  if (to.name === 'admin-login' && auth.isAuthenticated) {
    return { name: 'admin-items' }
  }
})
