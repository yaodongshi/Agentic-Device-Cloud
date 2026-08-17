import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import type { Role } from '@/api/types'

// Route table mirrors design/20 information architecture: login, the
// dashboard landing page and the seven console modules under the main
// layout. meta.roles hides modules at the route layer (design/20 2.2
// double-layer visibility with the menu).
const dashboard = { path: 'dashboard', name: 'dashboard', component: () => import('@/views/Dashboard.vue'), meta: { title: 'dashboard' } }
const devices = { path: 'devices', name: 'devices', component: () => import('@/views/devices/Devices.vue'), meta: { title: 'devices' } }

const moduleRoles: Record<string, Role[]> = {
  tools: ['platform_admin', 'tenant_admin'],
  approvals: ['platform_admin', 'tenant_admin', 'approver'],
  audit: ['platform_admin', 'tenant_admin', 'auditor'],
  apiKeys: ['platform_admin', 'tenant_admin'],
  tenants: ['platform_admin'],
  billing: ['platform_admin'],
  monitor: ['platform_admin', 'tenant_admin', 'auditor'],
  ops: ['platform_admin'],
  adapters: ['platform_admin', 'tenant_admin'],
  market: ['platform_admin', 'tenant_admin'],
}

function moduleRoute(name: string, load: () => Promise<unknown>) {
  return {
    path: name === 'apiKeys' ? 'api-keys' : name,
    name,
    // The import() literal must stay at the call site: a string-argument
    // import() cannot be analyzed by Rollup and would inline the module
    // (and ECharts for /monitor) into the entry chunk.
    component: load,
    meta: { title: name, roles: moduleRoles[name] },
  }
}

// Landing path per role for the RBAC guard fallback.
const roleHome: Record<Role, string> = {
  platform_admin: '/dashboard',
  tenant_admin: '/dashboard',
  approver: '/approvals',
  auditor: '/audit',
}

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', name: 'login', component: () => import('@/views/Login.vue') },
    // HITL callback landing page (F-11): standalone, session-less and
    // outside the main layout (design/20 4.14); security comes from the
    // URL signature, not the console session.
    {
      path: '/hitl/action',
      name: 'hitlAction',
      component: () => import('@/views/hitl/ActionPage.vue'),
      meta: { title: 'hitlAction' },
    },
    {
      path: '/',
      component: () => import('@/layouts/MainLayout.vue'),
      redirect: '/dashboard',
      children: [
        dashboard,
        devices,
        moduleRoute('tools', () => import('@/views/tools/Tools.vue')),
        moduleRoute('approvals', () => import('@/views/approvals/Approvals.vue')),
        moduleRoute('audit', () => import('@/views/audit/Audit.vue')),
        moduleRoute('apiKeys', () => import('@/views/apikeys/ApiKeys.vue')),
        moduleRoute('tenants', () => import('@/views/tenants/Tenants.vue')),
        // Billing is a platform-level commercial surface (FR-016,
        // design/82 B3.2): statements, generation and reconciliation.
        moduleRoute('billing', () => import('@/views/billing/Billing.vue')),
        // Org members sub-page of a tenant (F-09, design/20 4.11); not a top
        // menu item, entered from the tenants row action.
        {
          path: 'tenants/:id/users',
          name: 'tenantUsers',
          component: () => import('@/views/tenants/OrgUsers.vue'),
          meta: { title: 'orgUsers', roles: ['platform_admin', 'tenant_admin'] as Role[] },
        },
        moduleRoute('monitor', () => import('@/views/monitor/Monitor.vue')),
        moduleRoute('ops', () => import('@/views/ops/OpsMonitor.vue')),
        moduleRoute('adapters', () => import('@/views/adapters/Adapters.vue')),
        // Tool package marketplace (design/83 C3.1/C3.2): browse/search/
        // install/uninstall plus the publish form.
        moduleRoute('market', () => import('@/views/market/Market.vue')),
      ],
    },
  ],
})

// Auth guard: unauthenticated users are redirected to /login; authenticated
// users hitting a module outside their role are sent to their home module.
// The HITL landing page is exempt from the session requirement (F-11).
const publicNames = ['login', 'hitlAction']
router.beforeEach((to) => {
  const auth = useAuthStore()
  if (!publicNames.includes(String(to.name)) && !auth.token) return { name: 'login' }
  if (to.name === 'login' && auth.token) return { name: 'dashboard' }
  const roles = to.meta.roles as Role[] | undefined
  if (roles && auth.user && !roles.includes(auth.user.role)) {
    return { path: roleHome[auth.user.role] }
  }
  return true
})
