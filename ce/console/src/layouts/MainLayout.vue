<template>
  <el-container class="layout">
    <el-aside width="220px" class="aside">
      <div class="brand">{{ t('common.appName') }}</div>
      <el-menu :default-active="$route.path" router>
        <el-menu-item v-for="item in menus" :key="item.path" :index="item.path">
          {{ t(`menu.${item.key}`) }}
        </el-menu-item>
      </el-menu>
    </el-aside>
    <el-container>
      <el-header class="header">
        <span>{{ t(`menu.${currentKey}`) }}</span>
        <el-button text @click="logout">{{ t('common.cancel') }}</el-button>
      </el-header>
      <el-main><router-view /></el-main>
    </el-container>
  </el-container>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const menus = [
  { path: '/devices', key: 'devices' },
  { path: '/tools', key: 'tools' },
  { path: '/approvals', key: 'approvals' },
  { path: '/audit', key: 'audit' },
  { path: '/api-keys', key: 'apiKeys' },
  { path: '/tenants', key: 'tenants' },
  { path: '/monitor', key: 'monitor' },
]

const currentKey = computed(() => route.meta.title as string)

function logout() {
  auth.clear()
  router.push('/login')
}
</script>

<style scoped>
.layout { height: 100vh; }
.aside { background: #fff; border-right: 1px solid var(--adc-border); }
.brand { height: 56px; display: flex; align-items: center; padding: 0 16px; font-weight: 600; color: var(--adc-brand); }
.header { display: flex; align-items: center; justify-content: space-between; background: #fff; border-bottom: 1px solid var(--adc-border); }
</style>
