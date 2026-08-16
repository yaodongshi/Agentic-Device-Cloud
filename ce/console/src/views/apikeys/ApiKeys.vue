<template>
  <el-card>
    <div class="toolbar">
      <el-button
        type="primary"
        @click="openCreate"
      >
        {{ t('apiKeys.create') }}
      </el-button>
    </div>

    <el-table
      v-loading="loading"
      :data="list"
    >
      <el-table-column
        prop="name"
        :label="t('apiKeys.name')"
        min-width="160"
      />
      <el-table-column
        prop="key_prefix"
        :label="t('apiKeys.prefix')"
        width="180"
      />
      <el-table-column
        :label="t('apiKeys.scopes')"
        min-width="160"
      >
        <template #default="{ row }">
          {{ scopesSummary(row) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('apiKeys.expiresAt')"
        width="130"
      >
        <template #default="{ row }">
          <span v-if="row.expires_at">{{ formatDate(row.expires_at) }}</span>
          <span
            v-else
            class="muted"
          >{{ t('apiKeys.neverExpires') }}</span>
        </template>
      </el-table-column>
      <el-table-column
        :label="t('common.status')"
        width="100"
      >
        <template #default="{ row }">
          <el-tag
            :type="keyStatusTag(row.status)"
            effect="light"
          >
            {{ t(`apiKeys.statuses.${row.status}`) }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column
        :label="t('apiKeys.lastUsedAt')"
        width="170"
      >
        <template #default="{ row }">
          <span v-if="row.last_used_at">{{ formatTime(row.last_used_at) }}</span>
          <span
            v-else
            class="muted"
          >{{ t('apiKeys.neverUsed') }}</span>
        </template>
      </el-table-column>
      <el-table-column
        :label="t('apiKeys.createdAt')"
        width="170"
      >
        <template #default="{ row }">
          {{ formatTime(row.created_at) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('common.actions')"
        width="170"
        fixed="right"
      >
        <template #default="{ row }">
          <el-popconfirm
            :title="t('apiKeys.confirmRotate')"
            :confirm-button-text="t('common.confirm')"
            :cancel-button-text="t('common.cancel')"
            width="300"
            @confirm="rotateKey(row)"
          >
            <template #reference>
              <el-button
                link
                type="primary"
                :disabled="row.status !== 'active'"
              >
                {{ t('apiKeys.rotate') }}
              </el-button>
            </template>
          </el-popconfirm>
          <el-popconfirm
            :title="t('apiKeys.confirmRevoke')"
            :confirm-button-text="t('common.confirm')"
            :cancel-button-text="t('common.cancel')"
            width="300"
            @confirm="revokeKey(row)"
          >
            <template #reference>
              <el-button
                link
                type="danger"
                :disabled="row.status !== 'active'"
              >
                {{ t('apiKeys.revoke') }}
              </el-button>
            </template>
          </el-popconfirm>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty :description="t('apiKeys.empty')">
          <div class="empty-actions">
            <el-button
              type="primary"
              @click="openCreate"
            >
              {{ t('apiKeys.create') }}
            </el-button>
          </div>
        </el-empty>
      </template>
    </el-table>

    <el-pagination
      v-model:current-page="page"
      v-model:page-size="pageSize"
      class="pagination"
      background
      layout="total, sizes, prev, pager, next"
      :total="total"
      :page-sizes="[10, 20, 50]"
      @current-change="fetchList"
      @size-change="onSizeChange"
    />

    <!-- Create dialog -->
    <el-dialog
      v-model="createVisible"
      :title="t('apiKeys.createTitle')"
      width="500px"
    >
      <el-form
        ref="createFormRef"
        :model="createForm"
        :rules="createRules"
        label-width="90px"
      >
        <el-form-item
          :label="t('apiKeys.name')"
          prop="name"
        >
          <el-input
            v-model="createForm.name"
            :placeholder="t('apiKeys.namePlaceholder')"
          />
        </el-form-item>
        <el-form-item
          :label="t('apiKeys.scopes')"
          prop="scopesInput"
        >
          <el-input
            v-model="createForm.scopesInput"
            type="textarea"
            :rows="2"
            :placeholder="t('apiKeys.scopesPlaceholder')"
          />
          <div class="hint">
            {{ t('apiKeys.scopesHint') }}
          </div>
        </el-form-item>
        <el-form-item :label="t('apiKeys.expiresAt')">
          <el-date-picker
            v-model="createForm.expiresAt"
            type="datetime"
            value-format="YYYY-MM-DDTHH:mm:ssZ"
            :placeholder="t('apiKeys.neverExpires')"
            class="full"
          />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createVisible = false">
          {{ t('common.cancel') }}
        </el-button>
        <el-button
          type="primary"
          :loading="creating"
          @click="submitCreate"
        >
          {{ t('apiKeys.create') }}
        </el-button>
      </template>
    </el-dialog>

    <!-- One-time key dialog (create / rotate) -->
    <el-dialog
      v-model="keyVisible"
      :title="t('apiKeys.keyTitle')"
      width="560px"
      :close-on-click-modal="false"
      :close-on-press-escape="false"
      :show-close="false"
    >
      <el-alert
        type="warning"
        :title="t('apiKeys.keyWarning')"
        :closable="false"
        show-icon
      />
      <div class="credential-box">
        <el-input
          :model-value="createdKey"
          readonly
        />
        <el-button
          class="copy"
          @click="copyKey"
        >
          {{ t('common.copy') }}
        </el-button>
      </div>
      <el-checkbox v-model="keyAcked">
        {{ t('apiKeys.ackSaved') }}
      </el-checkbox>
      <template #footer>
        <el-button
          type="primary"
          :disabled="!keyAcked"
          @click="closeKey"
        >
          {{ t('common.close') }}
        </el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'
import { api } from '@/api/request'
import { errorMessage } from '@/api/errors'
import { copyText } from '@/utils/clipboard'
import { formatDate, formatTime } from '@/utils/format'
import type { ApiKey, CreateApiKeyPayload, CreatedApiKey, Page } from '@/api/types'

const { t } = useI18n()

const loading = ref(false)
const list = ref<ApiKey[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)

// GET /v1/admin/agent-keys (design/33 3.1.13): only masked key_prefix is
// returned, never the full key (NFR-004).
async function fetchList() {
  loading.value = true
  try {
    const res = await api.get<Page<ApiKey>>('/v1/admin/agent-keys', {
      page: page.value,
      page_size: pageSize.value,
    })
    list.value = res.items ?? []
    total.value = res.total ?? 0
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    loading.value = false
  }
}

function onSizeChange() {
  page.value = 1
  fetchList()
}

function scopesSummary(key: ApiKey): string {
  const tools = key.scopes?.allowed_tools ?? []
  if (tools.includes('*')) return t('apiKeys.allTools')
  if (tools.length === 0) return t('apiKeys.noTools')
  return t('apiKeys.toolCount', { n: tools.length })
}

function keyStatusTag(status: string): 'success' | 'info' | 'danger' {
  switch (status) {
    case 'active': return 'success'
    case 'expired': return 'info'
    default: return 'danger'
  }
}

// Create form: allowed_tools entered as a comma-separated pattern list.
const createVisible = ref(false)
const creating = ref(false)
const createFormRef = ref<FormInstance>()
const createForm = reactive({ name: '', scopesInput: '*', expiresAt: '' })

const createRules: FormRules = {
  name: [{ required: true, message: t('apiKeys.nameRequired'), trigger: 'blur' }],
}

function openCreate() {
  createFormRef.value?.resetFields()
  createForm.name = ''
  createForm.scopesInput = '*'
  createForm.expiresAt = ''
  createVisible.value = true
}

function buildPayload(): CreateApiKeyPayload {
  const tools = createForm.scopesInput
    .split(/[,，\n]/)
    .map((s) => s.trim())
    .filter(Boolean)
  return {
    name: createForm.name.trim(),
    scopes: { allowed_tools: tools },
    expires_at: createForm.expiresAt || null,
  }
}

async function submitCreate() {
  const valid = await createFormRef.value?.validate().catch(() => false)
  if (!valid) return
  creating.value = true
  try {
    // POST /v1/admin/agent-keys (design/33 3.1.12); the full key appears
    // exactly once in the response.
    const res = await api.post<CreatedApiKey>('/v1/admin/agent-keys', buildPayload())
    createVisible.value = false
    ElMessage.success(t('apiKeys.createSuccess'))
    showKey(res.key)
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    creating.value = false
  }
}

// Rotation per design/33 3.1.14: issue a new key with the same scopes; old
// and new run in parallel during the 24h transition, then the old key is
// revoked by the admin.
async function rotateKey(key: ApiKey) {
  try {
    const res = await api.post<CreatedApiKey>('/v1/admin/agent-keys', {
      name: key.name,
      scopes: key.scopes,
      expires_at: key.expires_at ?? null,
    })
    showKey(res.key)
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  }
}

async function revokeKey(key: ApiKey) {
  try {
    // DELETE /v1/admin/agent-keys/{key_id} (design/33 3.1.14), immediate.
    await api.delete(`/v1/admin/agent-keys/${key.key_id}`)
    ElMessage.success(t('apiKeys.revokeSuccess'))
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  }
}

// One-time key dialog shared by create and rotate flows.
const keyVisible = ref(false)
const createdKey = ref('')
const keyAcked = ref(false)

function showKey(key: string) {
  createdKey.value = key
  keyAcked.value = false
  keyVisible.value = true
}

function closeKey() {
  keyVisible.value = false
  createdKey.value = ''
}

async function copyKey() {
  const ok = await copyText(createdKey.value)
  ElMessage[ok ? 'success' : 'error'](ok ? t('common.copied') : t('common.copyFailed'))
  if (ok) keyAcked.value = true
}

onMounted(fetchList)
</script>

<style scoped>
.toolbar { display: flex; justify-content: flex-end; margin-bottom: var(--adc-space-4); }
.pagination { margin-top: var(--adc-space-4); justify-content: flex-end; }
.hint { margin-top: var(--adc-space-1); font-size: 12px; color: var(--adc-text-secondary); line-height: 1.4; }
.full { width: 100%; }
.credential-box { display: flex; gap: var(--adc-space-2); margin: var(--adc-space-4) 0 var(--adc-space-3); }
.credential-box .el-input { flex: 1; }
.muted { color: var(--adc-text-secondary); }
.empty-actions { display: flex; justify-content: center; }
</style>
