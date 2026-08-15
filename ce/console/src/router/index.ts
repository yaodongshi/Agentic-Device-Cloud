import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import type { Role } from '@/api/types'

// Route table mirrors design/20 information architecture: login plus the
// seven console modules under the main layout. meta.roles hides modules at
// the route layer (design/20 2.2 double-layer visibility with the menu).
const devices = { path: 'devices', name: 'devices', component: () => import('@/views/devices/Devices.vue'), meta: { title: 'devices' } }

const moduleRoles: Record<string, Role[]> = {
  tools: ['platform_admin', 'tenant_admin'],
  approvals: ['platform_admin', 'tenant_admin', 'approver'],
  audit: ['platform_admin', 'tenant_admin', 'auditor'],
  apiKeys: ['platform_admin', 'tenant_admin'],
  tenants: ['platform_admin'],
  monitor: ['platform_admin', 'tenant_admin', 'auditor'],
}

function moduleRoute(name: string, component: string) {
  return {
    path: name === 'apiKeys' ? 'api-keys' : name,
    name,
    component: () => import(component),
    meta: { title: name, roles: moduleRoles[name] },
  }
}

// Landing path per role for the RBAC guard fallback.
const roleHome: Record<Role, string> = {
  platform_admin: '/devices',
  tenant_admin: '/devices',
  approver: '/approvals',
  auditor: '/audit',
}

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', name: 'login', component: () => import('@/views/Login.vue') },
    {
      path: '/',
      component: () => import('@/layouts/MainLayout.vue'),
      redirect: '/devices',
      children: [
        devices,
        moduleRoute('tools', '@/views/tools/Tools.vue'),
        moduleRoute('approvals', '@/views/approvals/Approvals.vue'),
        moduleRoute('audit', '@/views/audit/Audit.vue'),
        moduleRoute('apiKeys', '@/views/apikeys/ApiKeys.vue'),
        moduleRoute('tenants', '@/views/tenants/Tenants.vue'),
        moduleRoute('monitor', '@/views/monitor/Monitor.vue'),
      ],
    },
  ],
})

// Auth guard: unauthenticated users are redirected to /login; authenticated
// users hitting a module outside their role are sent to their home module.
router.beforeEach((to) => {
  const auth = useAuthStore()
  if (to.name !== 'login' && !auth.token) return { name: 'login' }
  if (to.name === 'login' && auth.token) return { name: 'devices' }
  const roles = to.meta.roles as Role[] | undefined
  if (roles && auth.user && !roles.includes(auth.user.role)) {
    return { path: roleHome[auth.user.role] }
  }
  return true
})
