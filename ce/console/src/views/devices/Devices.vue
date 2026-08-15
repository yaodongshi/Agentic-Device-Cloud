<template>
  <el-card>
    <div class="toolbar">
      <el-button
        type="primary"
        @click="openRegister"
      >
        {{ t('devices.register') }}
      </el-button>
      <div class="filters">
        <el-input
          v-model="filters.keyword"
          class="filter-item keyword"
          :placeholder="t('common.keywordPlaceholder')"
          clearable
          @keyup.enter="search"
        />
        <el-select
          v-model="filters.status"
          class="filter-item"
          :placeholder="t('devices.statusLabel')"
          clearable
        >
          <el-option
            v-for="(label, key) in t('devices.statuses')"
            :key="key"
            :label="label"
            :value="key"
          />
        </el-select>
        <el-select
          v-model="filters.deviceType"
          class="filter-item"
          :placeholder="t('devices.deviceType')"
          clearable
          filterable
          allow-create
        >
          <el-option
            v-for="opt in deviceTypeOptions"
            :key="opt"
            :label="opt"
            :value="opt"
          />
        </el-select>
        <el-button
          type="primary"
          @click="search"
        >
          {{ t('common.search') }}
        </el-button>
        <el-button @click="reset">
          {{ t('common.reset') }}
        </el-button>
      </div>
    </div>

    <el-table
      v-loading="loading"
      :data="list"
    >
      <el-table-column
        prop="device_code"
        :label="t('devices.deviceCode')"
        min-width="150"
      />
      <el-table-column
        prop="name"
        :label="t('devices.name')"
        min-width="140"
      />
      <el-table-column
        prop="device_type"
        :label="t('devices.deviceType')"
        width="120"
      />
      <el-table-column
        :label="t('devices.authType')"
        width="120"
      >
        <template #default="{ row }">
          {{ t(`devices.authTypes.${row.auth_type}`) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('devices.statusLabel')"
        width="110"
      >
        <template #default="{ row }">
          <el-tag
            :type="statusTagType(row.status)"
            effect="light"
          >
            {{ t(`devices.statuses.${statusKey(row)}`) }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column
        :label="t('devices.lastHeartbeat')"
        width="170"
      >
        <template #default="{ row }">
          <span v-if="row.last_heartbeat">{{ formatTime(row.last_heartbeat) }}</span>
          <span
            v-else
            class="muted"
          >{{ t('devices.neverHeartbeat') }}</span>
        </template>
      </el-table-column>
      <el-table-column
        prop="sdk_version"
        :label="t('devices.sdkVersion')"
        width="110"
      >
        <template #default="{ row }">
          {{ row.sdk_version ?? '—' }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('devices.createdAt')"
        width="170"
      >
        <template #default="{ row }">
          {{ formatTime(row.created_at) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('common.actions')"
        width="300"
        fixed="right"
      >
        <template #default="{ row }">
          <el-popconfirm
            :title="t(`devices.confirm${row.status === 'frozen' ? 'Unfreeze' : 'Freeze'}`)"
            :confirm-button-text="t('common.confirm')"
            :cancel-button-text="t('common.cancel')"
            width="300"
            @confirm="runAction(row, row.status === 'frozen' ? 'unfreeze' : 'freeze', row.status === 'frozen' ? 'devices.unfreezeSuccess' : 'devices.freezeSuccess')"
          >
            <template #reference>
              <el-button
                link
                type="warning"
                :disabled="row.status === 'retired'"
              >
                {{ row.status === 'frozen' ? t('devices.unfreeze') : t('devices.freeze') }}
              </el-button>
            </template>
            <div class="reason">
              <el-input
                v-model="actionReason"
                :placeholder="t('common.reasonPlaceholder')"
              />
            </div>
          </el-popconfirm>
          <el-popconfirm
            :title="t('devices.confirmReset')"
            :confirm-button-text="t('common.confirm')"
            :cancel-button-text="t('common.cancel')"
            width="300"
            @confirm="runAction(row, 'reset_credential', 'devices.resetSuccess')"
          >
            <template #reference>
              <el-button
                link
                type="primary"
                :disabled="row.status === 'retired'"
              >
                {{ t('devices.resetCredential') }}
              </el-button>
            </template>
            <div class="reason">
              <el-input
                v-model="actionReason"
                :placeholder="t('common.reasonPlaceholder')"
              />
            </div>
          </el-popconfirm>
          <el-popconfirm
            :title="t('devices.confirmRevoke')"
            :confirm-button-text="t('common.confirm')"
            :cancel-button-text="t('common.cancel')"
            width="300"
            @confirm="runAction(row, 'revoke_credential', 'devices.revokeSuccess')"
          >
            <template #reference>
              <el-button
                link
                type="danger"
                :disabled="row.status === 'retired'"
              >
                {{ t('devices.revoke') }}
              </el-button>
            </template>
            <div class="reason">
              <el-input
                v-model="actionReason"
                :placeholder="t('common.reasonPlaceholder')"
              />
            </div>
          </el-popconfirm>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty :description="hasFilters ? t('devices.searchEmpty') : t('devices.empty')" />
      </template>
    </el-table>

    <el-pagination
      v-model:current-page="page"
      v-model:page-size="pageSize"
      class="pagination"
      background
      layout="total, sizes, prev, pager, next, jumper"
      :total="total"
      :page-sizes="[10, 20, 50]"
      @current-change="fetchList"
      @size-change="onSizeChange"
    />

    <!-- Register dialog -->
    <el-dialog
      v-model="registerVisible"
      :title="t('devices.registerTitle')"
      width="480px"
    >
      <el-form
        ref="registerFormRef"
        :model="registerForm"
        :rules="registerRules"
        label-width="90px"
      >
        <el-form-item
          :label="t('devices.deviceCode')"
          prop="device_code"
        >
          <el-input
            v-model="registerForm.device_code"
            :placeholder="t('devices.deviceCodePlaceholder')"
          />
        </el-form-item>
        <el-form-item
          :label="t('devices.name')"
          prop="name"
        >
          <el-input
            v-model="registerForm.name"
            :placeholder="t('devices.namePlaceholder')"
          />
        </el-form-item>
        <el-form-item
          :label="t('devices.deviceType')"
          prop="device_type"
        >
          <el-select
            v-model="registerForm.device_type"
            :placeholder="t('devices.deviceTypePlaceholder')"
            filterable
            allow-create
            class="full"
          >
            <el-option
              v-for="opt in deviceTypeOptions"
              :key="opt"
              :label="opt"
              :value="opt"
            />
          </el-select>
        </el-form-item>
        <el-form-item
          :label="t('devices.authType')"
          prop="auth_type"
        >
          <el-select
            v-model="registerForm.auth_type"
            class="full"
          >
            <el-option
              v-for="(label, key) in t('devices.authTypes')"
              :key="key"
              :label="label"
              :value="key"
            />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="registerVisible = false">
          {{ t('common.cancel') }}
        </el-button>
        <el-button
          type="primary"
          :loading="registering"
          @click="submitRegister"
        >
          {{ t('devices.register') }}
        </el-button>
      </template>
    </el-dialog>

    <!-- One-time credential dialog (register / reset credential) -->
    <el-dialog
      v-model="credentialVisible"
      :title="credentialTitleKey ? t(credentialTitleKey) : t('devices.credentialTitle')"
      width="520px"
      :close-on-click-modal="false"
      :close-on-press-escape="false"
      :show-close="false"
    >
      <el-alert
        type="warning"
        :title="t('devices.credentialWarning')"
        :closable="false"
        show-icon
      />
      <div class="credential-box">
        <el-input
          :model-value="credentialSecret"
          readonly
        />
        <el-button
          class="copy"
          @click="copyCredential"
        >
          {{ t('common.copy') }}
        </el-button>
      </div>
      <el-checkbox v-model="credentialAcked">
        {{ t('devices.ackSaved') }}
      </el-checkbox>
      <template #footer>
        <el-button
          type="primary"
          :disabled="!credentialAcked"
          @click="closeCredential"
        >
          {{ t('common.close') }}
        </el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'
import { api } from '@/api/request'
import { errorMessage } from '@/api/errors'
import { copyText } from '@/utils/clipboard'
import { formatTime } from '@/utils/format'
import type {
  Device,
  DeviceAuthType,
  DevicePatchOp,
  DevicePatchResponse,
  DeviceStatus,
  Page,
  RegisteredDevice,
} from '@/api/types'

const { t } = useI18n()

// Common device_type suggestions (design/32 example set); the backend
// accepts any 64-char string, so the select allows free input too.
const deviceTypeOptions = ['cnc', 'plc', 'robot', 'sensor_hub', 'gateway']

const loading = ref(false)
const list = ref<Device[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)

const filters = reactive({ keyword: '', status: '', deviceType: '' })

const hasFilters = computed(() => Boolean(filters.keyword || filters.status || filters.deviceType))

// GET /v1/admin/devices with filters and offset pagination (design/33 3.1.7).
async function fetchList() {
  loading.value = true
  try {
    const res = await api.get<Page<Device>>('/v1/admin/devices', {
      page: page.value,
      page_size: pageSize.value,
      status: filters.status || undefined,
      device_type: filters.deviceType || undefined,
      keyword: filters.keyword || undefined,
    })
    list.value = res.items ?? []
    total.value = res.total ?? 0
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    loading.value = false
  }
}

function search() {
  page.value = 1
  fetchList()
}

function reset() {
  filters.keyword = ''
  filters.status = ''
  filters.deviceType = ''
  search()
}

function onSizeChange() {
  page.value = 1
  fetchList()
}

// Normalize the wire status (design/33 lower-case, design/32 upper-case)
// and pick one of the five tag colors from design/20.
function statusKey(row: Device): DeviceStatus {
  return String(row.status).toLowerCase() as DeviceStatus
}

function statusTagType(status: string): 'success' | 'info' | 'warning' | 'danger' | 'primary' {
  switch (String(status).toLowerCase()) {
    case 'online': return 'success'
    case 'error': return 'danger'
    case 'frozen': return 'warning'
    case 'retired': return 'primary'
    default: return 'info'
  }
}

// Register form
const registerVisible = ref(false)
const registering = ref(false)
const registerFormRef = ref<FormInstance>()
const registerForm = reactive({
  device_code: '',
  name: '',
  device_type: '',
  auth_type: 'token' as DeviceAuthType,
})

const registerRules: FormRules = {
  device_code: [
    { required: true, message: t('devices.deviceCodePlaceholder'), trigger: 'blur' },
    { pattern: /^[A-Za-z0-9_-]{1,128}$/, message: t('devices.deviceCodePattern'), trigger: 'blur' },
  ],
  name: [{ required: true, message: t('devices.namePlaceholder'), trigger: 'blur' }],
  device_type: [{ required: true, message: t('devices.deviceTypePlaceholder'), trigger: 'change' }],
  auth_type: [{ required: true, message: t('devices.authType'), trigger: 'change' }],
}

function openRegister() {
  registerFormRef.value?.resetFields()
  registerVisible.value = true
}

async function submitRegister() {
  const valid = await registerFormRef.value?.validate().catch(() => false)
  if (!valid) return
  registering.value = true
  try {
    // POST /v1/admin/devices (design/33 3.1.6); credential is shown once.
    const res = await api.post<RegisteredDevice>('/v1/admin/devices', {
      device_code: registerForm.device_code.trim(),
      name: registerForm.name.trim(),
      device_type: registerForm.device_type.trim(),
      auth_type: registerForm.auth_type,
    })
    registerVisible.value = false
    ElMessage.success(t('devices.registerSuccess'))
    openCredential(res.credential?.secret ?? '', 'devices.credentialTitle')
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    registering.value = false
  }
}

// Row actions: freeze/unfreeze/revoke/reset credential all require a reason
// (change_reason is mandatory and audited, design/33 3.1.8).
const actionReason = ref('')

async function runAction(device: Device, op: DevicePatchOp, successKey: string) {
  const reason = actionReason.value.trim()
  actionReason.value = ''
  if (!reason) {
    ElMessage.warning(t('common.reasonRequired'))
    return
  }
  try {
    const res = await api.patch<DevicePatchResponse>(`/v1/admin/devices/${device.device_id}`, {
      op,
      change_reason: reason,
    })
    ElMessage.success(t(successKey))
    if (op === 'reset_credential' && res.credential) {
      openCredential(res.credential.secret, 'devices.credentialTitle')
    }
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  }
}

// One-time credential dialog shared by register and reset credential flows.
const credentialVisible = ref(false)
const credentialSecret = ref('')
const credentialTitleKey = ref('')
const credentialAcked = ref(false)

function openCredential(secret: string, titleKey: string) {
  credentialSecret.value = secret
  credentialTitleKey.value = titleKey
  credentialAcked.value = false
  credentialVisible.value = true
}

function closeCredential() {
  credentialVisible.value = false
  credentialSecret.value = ''
}

async function copyCredential() {
  const ok = await copyText(credentialSecret.value)
  ElMessage[ok ? 'success' : 'error'](ok ? t('common.copied') : t('common.copyFailed'))
  if (ok) credentialAcked.value = true
}

onMounted(fetchList)
</script>

<style scoped>
.toolbar { display: flex; align-items: flex-start; justify-content: space-between; margin-bottom: var(--adc-space-4); }
.filters { display: flex; flex-wrap: wrap; gap: var(--adc-space-2); }
.filter-item { width: 170px; }
.keyword { width: 220px; }
.pagination { margin-top: var(--adc-space-4); justify-content: flex-end; }
.full { width: 100%; }
.reason { padding: var(--adc-space-2) 0; }
.credential-box { display: flex; gap: var(--adc-space-2); margin: var(--adc-space-4) 0 var(--adc-space-3); }
.credential-box .el-input { flex: 1; }
.muted { color: var(--adc-text-secondary); }
</style>
