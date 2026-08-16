<template>
  <el-container class="layout">
    <el-aside
      width="220px"
      class="aside"
    >
      <div class="brand">
        {{ t('common.appName') }}
      </div>
      <el-menu
        :default-active="activeMenu"
        router
      >
        <el-menu-item
          v-for="item in visibleMenus"
          :key="item.path"
          :index="item.path"
        >
          {{ t(`menu.${item.key}`) }}
        </el-menu-item>
      </el-menu>
    </el-aside>
    <el-container>
      <el-header class="header">
        <span class="page-title">{{ t(`menu.${currentKey}`) }}</span>
        <el-dropdown
          v-if="auth.user"
          trigger="click"
        >
          <span class="user">
            {{ auth.user.displayName }}
            <el-tag
              size="small"
              class="role"
            >{{ t(`layout.roles.${auth.user.role}`) }}</el-tag>
          </span>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item disabled>
                {{ t('layout.settings') }}
              </el-dropdown-item>
              <el-dropdown-item
                divided
                @click="openDocs"
              >
                {{ t('layout.docs') }}
              </el-dropdown-item>
              <el-dropdown-item @click="confirmLogout">
                {{ t('layout.logout') }}
              </el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </el-header>
      <el-main><router-view /></el-main>
    </el-container>
  </el-container>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { ElMessageBox } from 'element-plus'
import { api } from '@/api/request'
import { useAuthStore } from '@/stores/auth'
import { DOCS_URL } from '@/utils/links'
import type { Role } from '@/api/types'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

// Menu visibility by role mirrors the design/20 2.2 matrix mapped onto the
// design/33 Admin API roles; unknown roles see every module (the backend
// still enforces access server-side).
interface MenuItem {
  path: string
  key: string
  roles?: Role[]
}

const menus: MenuItem[] = [
  { path: '/dashboard', key: 'dashboard' },
  { path: '/devices', key: 'devices' },
  { path: '/tools', key: 'tools', roles: ['platform_admin', 'tenant_admin'] },
  { path: '/approvals', key: 'approvals', roles: ['platform_admin', 'tenant_admin', 'approver'] },
  { path: '/audit', key: 'audit', roles: ['platform_admin', 'tenant_admin', 'auditor'] },
  { path: '/api-keys', key: 'apiKeys', roles: ['platform_admin', 'tenant_admin'] },
  { path: '/tenants', key: 'tenants', roles: ['platform_admin'] },
  { path: '/monitor', key: 'monitor', roles: ['platform_admin', 'tenant_admin', 'auditor'] },
  // A2.5: Grafana embed is a platform-level ops view (design/82 A2.5).
  { path: '/ops', key: 'ops', roles: ['platform_admin'] },
]

const visibleMenus = computed(() =>
  auth.user ? menus.filter((m) => !m.roles || m.roles.includes(auth.user!.role)) : menus,
)

// Sub-pages (e.g. /tenants/:id/users) keep the parent module highlighted.
const activeMenu = computed(() =>
  route.path.startsWith('/tenants/') ? '/tenants' : route.path,
)

const currentKey = computed(() => route.meta.title as string)

// The docs site ships with FR-020; until then the repo README is the docs
// entry point (links.ts).
function openDocs() {
  window.open(DOCS_URL, '_blank', 'noopener')
}

async function confirmLogout() {
  const ok = await ElMessageBox.confirm(t('layout.logoutConfirm'), t('layout.logout'), {
    type: 'warning',
    confirmButtonText: t('common.confirm'),
    cancelButtonText: t('common.cancel'),
  }).catch(() => false)
  if (!ok) return
  logout()
}

async function logout() {
  // Best-effort server-side session invalidation; local state is always
  // cleared even if the call fails (offline/expired session).
  try {
    await api.post('/v1/admin/auth/logout')
  } catch {
    // ignore: local cleanup below is authoritative for the console
  }
  auth.clear()
  router.push('/login')
}
</script>

<style scoped>
.layout { height: 100vh; }
.aside { background: #fff; border-right: 1px solid var(--adc-border); }
.brand { height: 56px; display: flex; align-items: center; padding: 0 16px; font-weight: 600; color: var(--adc-brand); }
.header { display: flex; align-items: center; justify-content: space-between; background: #fff; border-bottom: 1px solid var(--adc-border); }
.page-title { font-weight: 600; }
.user { display: inline-flex; align-items: center; gap: var(--adc-space-2); cursor: pointer; color: var(--adc-text); }
</style>
