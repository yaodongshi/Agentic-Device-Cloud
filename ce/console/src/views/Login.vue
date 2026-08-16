<template>
  <div class="login">
    <div class="brand">
      <h1>{{ t('common.appName') }}</h1>
      <p>{{ t('login.subtitle') }}</p>
    </div>
    <el-card class="card">
      <h2>{{ t('login.title') }}</h2>
      <el-alert
        v-if="errorMsg"
        class="alert"
        type="error"
        :title="errorMsg"
        :closable="false"
        show-icon
      />
      <el-form
        ref="formRef"
        :model="form"
        :rules="rules"
        label-position="top"
        @submit.prevent="submit"
      >
        <el-form-item
          :label="t('login.username')"
          prop="username"
        >
          <el-input
            v-model="form.username"
            autocomplete="username"
          />
        </el-form-item>
        <el-form-item
          :label="t('login.password')"
          prop="password"
        >
          <el-input
            v-model="form.password"
            type="password"
            autocomplete="current-password"
            show-password
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-button
          type="primary"
          class="submit"
          :loading="loading"
          @click="submit"
        >
          {{ loading ? t('login.submitting') : t('login.submit') }}
        </el-button>
        <el-button
          class="sso"
          disabled
        >
          {{ t('login.ssoSoon') }}
        </el-button>
      </el-form>
    </el-card>
    <p class="footer">
      {{ t('login.version') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import type { FormInstance, FormRules } from 'element-plus'
import { api } from '@/api/request'
import { errorMessage } from '@/api/errors'
import { useAuthStore } from '@/stores/auth'
import type { LoginResponse } from '@/api/types'

const { t } = useI18n()
const router = useRouter()
const auth = useAuthStore()

const formRef = ref<FormInstance>()
const loading = ref(false)
const errorMsg = ref('')

const form = reactive({ username: '', password: '' })

const rules: FormRules = {
  username: [{ required: true, message: t('login.usernameRequired'), trigger: 'blur' }],
  password: [{ required: true, message: t('login.passwordRequired'), trigger: 'blur' }],
}

async function submit() {
  const valid = await formRef.value?.validate().catch(() => false)
  if (!valid) return
  loading.value = true
  errorMsg.value = ''
  try {
    // POST /v1/admin/auth/login (design/33 3.1.1 via the unified gateway).
    const res = await api.post<LoginResponse>('/v1/admin/auth/login', {
      username: form.username,
      password: form.password,
    })
    auth.setToken(res.token)
    auth.setUser({
      userId: res.user_id,
      displayName: res.display_name,
      tenantId: res.tenant_id,
      role: res.role,
    })
    router.push('/dashboard')
  } catch (err) {
    errorMsg.value = errorMessage(err, t)
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
.login { min-height: 100vh; display: flex; flex-direction: column; align-items: center; justify-content: center; background: var(--adc-bg); }
.brand { text-align: center; margin-bottom: var(--adc-space-6); }
.brand h1 { margin: 0; color: var(--adc-brand); font-size: 26px; }
.brand p { margin: var(--adc-space-1) 0 0; color: var(--adc-text-secondary); }
.card { width: 380px; }
.alert { margin-bottom: var(--adc-space-4); }
.submit { width: 100%; margin-top: var(--adc-space-1); }
.sso { width: 100%; margin-left: 0; margin-top: var(--adc-space-2); }
.footer { margin-top: var(--adc-space-6); color: var(--adc-text-secondary); font-size: 12px; }
</style>
