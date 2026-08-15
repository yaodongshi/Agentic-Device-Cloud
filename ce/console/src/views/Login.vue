<template>
  <div class="login">
    <el-card class="card">
      <h2>{{ t('login.title') }}</h2>
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item :label="t('login.username')">
          <el-input v-model="form.username" />
        </el-form-item>
        <el-form-item :label="t('login.password')">
          <el-input v-model="form.password" type="password" show-password />
        </el-form-item>
        <el-button type="primary" class="submit" @click="submit">{{ t('login.submit') }}</el-button>
      </el-form>
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { reactive } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const { t } = useI18n()
const router = useRouter()
const auth = useAuthStore()
const form = reactive({ username: '', password: '' })

// Placeholder: real login wires the Admin API endpoint in Sprint 4 (B-01).
function submit() {
  if (!form.username || !form.password) return
  auth.setToken('placeholder-token')
  router.push('/devices')
}
</script>

<style scoped>
.login { height: 100vh; display: flex; align-items: center; justify-content: center; background: var(--adc-bg); }
.card { width: 380px; }
.submit { width: 100%; }
</style>
