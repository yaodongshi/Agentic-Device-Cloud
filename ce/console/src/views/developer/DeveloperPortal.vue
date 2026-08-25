<template>
  <div class="developer-page">
    <el-card class="access-card">
      <template #header>
        <strong>{{ t('developer.accessTitle') }}</strong>
      </template>
      <el-descriptions
        :column="1"
        border
      >
        <el-descriptions-item :label="t('developer.agentCard')">
          <code>{{ agentCardUrl }}</code>
        </el-descriptions-item>
        <el-descriptions-item :label="t('developer.endpoint')">
          <code>{{ a2aEndpoint }}</code>
        </el-descriptions-item>
        <el-descriptions-item :label="t('developer.authentication')">
          {{ t('developer.authenticationValue') }}
        </el-descriptions-item>
      </el-descriptions>
      <el-alert
        class="boundary"
        type="warning"
        :title="t('developer.boundary')"
        :closable="false"
        show-icon
      />
      <div class="example-title">
        {{ t('developer.example') }}
      </div>
      <pre><code>{{ requestExample }}</code></pre>
    </el-card>

    <el-card>
      <div class="toolbar">
        <el-button
          type="primary"
          @click="createVisible = true"
        >
          {{ t('developer.create') }}
        </el-button>
      </div>
      <el-table
        v-loading="loading"
        :data="applications"
      >
        <el-table-column
          prop="name"
          :label="t('developer.name')"
          min-width="150"
        />
        <el-table-column
          prop="purpose"
          :label="t('developer.purpose')"
          min-width="180"
          show-overflow-tooltip
        />
        <el-table-column
          :label="t('developer.scopes')"
          min-width="180"
        >
          <template #default="{ row }">
            <el-tag
              v-for="scope in row.scopes"
              :key="scope"
              class="scope"
            >
              {{ scope }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column
          :label="t('developer.prefix')"
          min-width="150"
        >
          <template #default="{ row }">
            <code>{{ row.credential_prefix || '—' }}</code>
          </template>
        </el-table-column>
        <el-table-column
          :label="t('common.status')"
          width="130"
        >
          <template #default="{ row }">
            <el-tag :type="row.status === 'active' ? 'success' : 'info'">
              {{ t(`developer.statuses.${row.status}`) }}
            </el-tag>
            <el-tag
              class="credential-state"
              :type="row.credential_status === 'active' ? 'success' : 'danger'"
              effect="plain"
            >
              {{ t(`developer.credentials.${row.credential_status}`) }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column
          :label="t('common.actions')"
          width="250"
          fixed="right"
        >
          <template #default="{ row }">
            <el-button
              link
              type="primary"
              :disabled="row.status !== 'active'"
              @click="confirmAction('rotate', row)"
            >
              {{ t('developer.rotate') }}
            </el-button>
            <el-button
              link
              type="warning"
              :disabled="row.credential_status !== 'active'"
              @click="confirmAction('revoke', row)"
            >
              {{ t('developer.revoke') }}
            </el-button>
            <el-button
              link
              :disabled="row.status !== 'active'"
              @click="confirmAction('disable', row)"
            >
              {{ t('developer.disable') }}
            </el-button>
            <el-button
              link
              type="danger"
              @click="confirmAction('delete', row)"
            >
              {{ t('developer.delete') }}
            </el-button>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty :description="t('developer.empty')" />
        </template>
      </el-table>
    </el-card>

    <el-dialog
      v-model="createVisible"
      :title="t('developer.createTitle')"
      width="min(520px, 92vw)"
    >
      <el-form label-position="top">
        <el-form-item :label="t('developer.name')">
          <el-input
            v-model="form.name"
            :placeholder="t('developer.namePlaceholder')"
          />
        </el-form-item>
        <el-form-item :label="t('developer.purpose')">
          <el-input
            v-model="form.purpose"
            type="textarea"
            :rows="3"
            :placeholder="t('developer.purposePlaceholder')"
          />
        </el-form-item>
        <el-form-item :label="t('developer.scopes')">
          <el-checkbox-group v-model="form.scopes">
            <el-checkbox value="a2a.tasks:read">
              {{ t('developer.scopeRead') }}
            </el-checkbox>
            <el-checkbox value="a2a.tasks:write">
              {{ t('developer.scopeWrite') }}
            </el-checkbox>
          </el-checkbox-group>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createVisible = false">
          {{ t('common.cancel') }}
        </el-button><el-button
          type="primary"
          :loading="creating"
          @click="createApplication"
        >
          {{ t('developer.create') }}
        </el-button>
      </template>
    </el-dialog>

    <el-dialog
      v-model="secretVisible"
      :title="t('developer.secretTitle')"
      width="min(620px, 94vw)"
      :close-on-click-modal="false"
      :show-close="false"
    >
      <el-alert
        type="warning"
        :title="t('developer.secretWarning')"
        :closable="false"
        show-icon
      />
      <div class="secret-row">
        <el-input
          :model-value="oneTimeSecret"
          readonly
        /><el-button @click="copySecret">
          {{ t('common.copy') }}
        </el-button>
      </div>
      <el-divider>{{ t('developer.connectivity') }}</el-divider>
      <p class="hint">
        {{ t('developer.connectivityHint') }}
      </p>
      <el-button
        :loading="testing"
        @click="testConnectivity"
      >
        {{ t('developer.connectivityRun') }}
      </el-button>
      <el-alert
        v-if="testResult"
        class="test-result"
        :type="testResult.ok ? 'success' : 'error'"
        :title="testResult.message"
        :closable="false"
        show-icon
      />
      <el-checkbox
        v-model="secretAcked"
        class="ack"
      >
        {{ t('developer.ackSaved') }}
      </el-checkbox>
      <template #footer>
        <el-button
          type="primary"
          :disabled="!secretAcked"
          @click="closeSecret"
        >
          {{ t('common.close') }}
        </el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, request } from '@/api/request'
import { errorMessage } from '@/api/errors'
import { copyText } from '@/utils/clipboard'
import type { CreatedDeveloperApplication, DeveloperApplication, DeveloperApplicationScope, DeveloperConnectivity, Page } from '@/api/types'

const { t, locale } = useI18n()
const origin = window.location.origin
const agentCardUrl = `${origin}/.well-known/agent-card.json`
const a2aEndpoint = `${origin}/v2/agents/a2a`
const requestExample = computed(() => locale.value === 'zh-CN'
  ? `curl -X POST '${a2aEndpoint}/tasks' -H 'Content-Type: application/json' -d '{"task_type":"inspect","goal":"检查设备状态","devices":["cnc-01"]}'`
  : `curl -X POST '${a2aEndpoint}/tasks' -H 'Content-Type: application/json' -d '{"task_type":"inspect","goal":"Inspect device status","devices":["cnc-01"]}'`)

const loading = ref(false)
const applications = ref<DeveloperApplication[]>([])
const createVisible = ref(false)
const creating = ref(false)
const form = reactive<{ name: string; purpose: string; scopes: DeveloperApplicationScope[] }>({ name: '', purpose: '', scopes: ['a2a.tasks:read'] })

async function loadApplications() {
  loading.value = true
  try {
    const res = await api.get<Page<DeveloperApplication>>('/v1/admin/developer-applications')
    applications.value = res.items ?? []
  } catch (err) { ElMessage.error(errorMessage(err, t)) } finally { loading.value = false }
}

async function createApplication() {
  if (!form.name.trim() || form.scopes.length === 0) { ElMessage.warning(t('errors.10001')); return }
  creating.value = true
  try {
    const result = await api.post<CreatedDeveloperApplication>('/v1/admin/developer-applications', { name: form.name.trim(), purpose: form.purpose.trim(), scopes: form.scopes })
    createVisible.value = false
    form.name = ''; form.purpose = ''; form.scopes = ['a2a.tasks:read']
    showSecret(result.secret)
    ElMessage.success(t('developer.createSuccess'))
    await loadApplications()
  } catch (err) { ElMessage.error(errorMessage(err, t)) } finally { creating.value = false }
}

type Action = 'rotate' | 'revoke' | 'disable' | 'delete'
async function confirmAction(action: Action, app: DeveloperApplication) {
  const ok = await ElMessageBox.confirm(t(`developer.confirm${action[0].toUpperCase()}${action.slice(1)}`), t(`developer.${action}`), { type: 'warning' }).catch(() => false)
  if (!ok) return
  try {
    if (action === 'delete') await api.delete(`/v1/admin/developer-applications/${app.application_id}`)
    else if (action === 'rotate') {
      const result = await api.post<CreatedDeveloperApplication>(`/v1/admin/developer-applications/${app.application_id}/credentials/rotate`)
      showSecret(result.secret)
    } else await api.post(`/v1/admin/developer-applications/${app.application_id}/${action === 'revoke' ? 'credentials/revoke' : 'disable'}`)
    ElMessage.success(t(`developer.${action === 'disable' ? 'disabled' : action === 'delete' ? 'deleted' : action === 'revoke' ? 'revoked' : 'createSuccess'}`))
    await loadApplications()
  } catch (err) { ElMessage.error(errorMessage(err, t)) }
}

const secretVisible = ref(false)
const oneTimeSecret = ref('')
const secretAcked = ref(false)
const testing = ref(false)
const testResult = ref<{ ok: boolean; message: string }>()
function showSecret(secret: string) { oneTimeSecret.value = secret; secretAcked.value = false; testResult.value = undefined; secretVisible.value = true }
function closeSecret() { secretVisible.value = false; oneTimeSecret.value = ''; testResult.value = undefined }
async function copySecret() { const ok = await copyText(oneTimeSecret.value); ElMessage[ok ? 'success' : 'error'](ok ? t('common.copied') : t('common.copyFailed')); if (ok) secretAcked.value = true }
async function testConnectivity() {
  testing.value = true
  try {
    const result = await request<DeveloperConnectivity>('/v1/developer/connectivity', { headers: { 'X-ADC-Application-Credential': oneTimeSecret.value } })
    testResult.value = { ok: result.ok && result.mode === 'credential_introspection', message: t('developer.connectivitySuccess') }
  } catch { testResult.value = { ok: false, message: t('developer.connectivityFailed') } } finally { testing.value = false }
}

onMounted(loadApplications)
</script>

<style scoped>
.developer-page { display: grid; gap: var(--adc-space-4); min-width: 0; }
.access-card code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.boundary, .example-title { margin-top: var(--adc-space-3); }
pre { margin: var(--adc-space-2) 0 0; padding: var(--adc-space-3); overflow: auto; border-radius: 6px; background: #111827; color: #e5e7eb; font-size: 12px; }
.toolbar { display: flex; justify-content: flex-end; margin-bottom: var(--adc-space-3); }
.scope, .credential-state { margin: 2px 4px 2px 0; }
.secret-row { display: flex; gap: var(--adc-space-2); margin-top: var(--adc-space-4); }
.hint { color: var(--adc-text-secondary); line-height: 1.5; }
.test-result, .ack { margin-top: var(--adc-space-3); }
@media (max-width: 720px) {
  .access-card :deep(.el-descriptions__label) { width: 110px; }
  .secret-row { align-items: stretch; }
  pre { white-space: pre-wrap; overflow-wrap: anywhere; }
}
</style>
