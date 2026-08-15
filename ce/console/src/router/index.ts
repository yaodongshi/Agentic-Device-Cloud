import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

// Route table mirrors design/20 information architecture: login plus the
// seven console modules under the main layout.
export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', name: 'login', component: () => import('@/views/Login.vue') },
    {
      path: '/',
      component: () => import('@/layouts/MainLayout.vue'),
      redirect: '/devices',
      children: [
        { path: 'devices', name: 'devices', component: () => import('@/views/devices/Devices.vue'), meta: { title: 'devices' } },
        { path: 'tools', name: 'tools', component: () => import('@/views/tools/Tools.vue'), meta: { title: 'tools' } },
        { path: 'approvals', name: 'approvals', component: () => import('@/views/approvals/Approvals.vue'), meta: { title: 'approvals' } },
        { path: 'audit', name: 'audit', component: () => import('@/views/audit/Audit.vue'), meta: { title: 'audit' } },
        { path: 'api-keys', name: 'apiKeys', component: () => import('@/views/apikeys/ApiKeys.vue'), meta: { title: 'apiKeys' } },
        { path: 'tenants', name: 'tenants', component: () => import('@/views/tenants/Tenants.vue'), meta: { title: 'tenants' } },
        { path: 'monitor', name: 'monitor', component: () => import('@/views/monitor/Monitor.vue'), meta: { title: 'monitor' } },
      ],
    },
  ],
})

// Auth guard: unauthenticated users are redirected to /login (token check
// is a placeholder until the Admin API session is wired in Sprint 4).
router.beforeEach((to) => {
  const auth = useAuthStore()
  if (to.name !== 'login' && !auth.token) return { name: 'login' }
  if (to.name === 'login' && auth.token) return { name: 'devices' }
  return true
})
