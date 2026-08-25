<template>
  <div class="login">
    <div class="brand">
      <!-- C2.1 white-label brand (design/83): logo + title come from the
           branding store (GET /v1/admin/branding), defaulting to the
           i18n app name when the platform brand is unconfigured. -->
      <img
        v-if="branding.logoUrl"
        :src="branding.logoUrl"
        class="logo"
        alt=""
      >
      <h1>{{ branding.displayTitle }}</h1>
      <p>{{ t('login.subtitle') }}</p>
    </div>
    <el-card class="card">
      <el-alert
        type="info"
        :closable="false"
        class="demo-tip"
      >
        {{ t('login.demoTip') }}
      </el-alert>
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
          tag="a"
          :href="oidcEnabled ? '/v1/admin/auth/oidc/start' : undefined"
          :disabled="!oidcEnabled"
        >
          {{ t('login.sso') }}
        </el-button>
      </el-form>
    </el-card>
    <p class="footer">
      {{ t('login.version') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import type { FormInstance, FormRules } from 'element-plus'
import { api } from '@/api/request'
import { errorMessage } from '@/api/errors'
import { useAuthStore } from '@/stores/auth'
import { useBrandingStore } from '@/stores/branding'
import type { LoginResponse, OIDCStatusResponse } from '@/api/types'

const { t } = useI18n()
const router = useRouter()
const auth = useAuthStore()
const branding = useBrandingStore()

const formRef = ref<FormInstance>()
const loading = ref(false)
const errorMsg = ref('')
const oidcEnabled = ref(false)

const form = reactive({ username: '', password: '' })

const rules: FormRules = {
  username: [{ required: true, message: t('login.usernameRequired'), trigger: 'blur' }],
  password: [{ required: true, message: t('login.passwordRequired'), trigger: 'blur' }],
}

onMounted(async () => {
	try {
		const status = await api.get<OIDCStatusResponse>('/v1/admin/auth/oidc/status')
		oidcEnabled.value = status.enabled
		if (new URLSearchParams(window.location.search).get('oidc') === 'success') {
			await auth.restoreCookieSession()
			await router.push('/dashboard')
		}
	} catch (err) {
		if (new URLSearchParams(window.location.search).get('oidc') === 'success') {
			errorMsg.value = errorMessage(err, t)
		}
	}
})

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
.brand .logo { height: 40px; margin-bottom: var(--adc-space-2); }
.brand h1 { margin: 0; color: var(--adc-brand); font-size: 26px; }
.brand p { margin: var(--adc-space-1) 0 0; color: var(--adc-text-secondary); }
.card { width: 380px; }
.demo-tip { margin-bottom: 16px; }
.alert { margin-bottom: var(--adc-space-4); }
.submit { width: 100%; margin-top: var(--adc-space-1); }
.sso { width: 100%; margin-left: 0; margin-top: var(--adc-space-2); }
.footer { margin-top: var(--adc-space-6); color: var(--adc-text-secondary); font-size: 12px; }
</style>
