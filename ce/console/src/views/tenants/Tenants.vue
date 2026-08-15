<template>
  <el-card>
    <div class="toolbar">
      <el-button
        type="primary"
        @click="openCreate"
      >
        {{ t('tenants.create') }}
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
          :placeholder="t('tenants.statusLabel')"
          clearable
        >
          <el-option
            v-for="(label, key) in tm('tenants.statuses')"
            :key="key"
            :label="label"
            :value="key"
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
        prop="name"
        :label="t('tenants.name')"
        min-width="180"
      />
      <el-table-column
        :label="t('tenants.statusLabel')"
        width="110"
      >
        <template #default="{ row }">
          <el-tag
            :type="statusTagType(row.status)"
            effect="light"
          >
            {{ t(`tenants.statuses.${statusKey(row.status)}`) }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column
        :label="t('tenants.deviceQuota')"
        min-width="220"
      >
        <template #default="{ row }">
          <quota-cell
            :used="row.used_devices ?? row.device_count"
            :quota="row.quota?.quota_devices"
          />
        </template>
      </el-table-column>
      <el-table-column
        :label="t('tenants.callsQuota')"
        min-width="220"
      >
        <template #default="{ row }">
          <quota-cell
            :used="row.used_calls_month"
            :quota="row.quota?.quota_calls_monthly"
          />
        </template>
      </el-table-column>
      <el-table-column
        :label="t('tenants.expiresAt')"
        width="120"
      >
        <template #default="{ row }">
          <span v-if="row.expires_at">{{ formatDate(row.expires_at) }}</span>
          <span
            v-else
            class="muted"
          >—</span>
        </template>
      </el-table-column>
      <el-table-column
        :label="t('tenants.createdAt')"
        width="170"
      >
        <template #default="{ row }">
          {{ formatTime(row.created_at) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('common.actions')"
        width="240"
        fixed="right"
      >
        <template #default="{ row }">
          <el-button
            link
            type="primary"
            @click="openEdit(row)"
          >
            {{ t('tenants.configure') }}
          </el-button>
          <el-button
            link
            type="primary"
            @click="openMembers(row)"
          >
            {{ t('tenants.members') }}
          </el-button>
          <el-popconfirm
            :title="row.status === 'active' ? t('tenants.confirmDisable') : t('tenants.confirmEnable')"
            :confirm-button-text="t('common.confirm')"
            :cancel-button-text="t('common.cancel')"
            width="320"
            @confirm="runStatusToggle(row)"
          >
            <template #reference>
              <el-button
                link
                :type="row.status === 'active' ? 'danger' : 'success'"
              >
                {{ row.status === 'active' ? t('tenants.disable') : t('tenants.enable') }}
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
        <el-empty :description="hasFilters ? t('tenants.searchEmpty') : t('tenants.empty')" />
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

    <!-- Create tenant dialog (POST /v1/admin/tenants, design/33 3.1.3) -->
    <el-dialog
      v-model="createVisible"
      :title="t('tenants.createTitle')"
      width="480px"
    >
      <el-form
        ref="createFormRef"
        :model="createForm"
        :rules="createRules"
        label-width="120px"
      >
        <el-form-item
          :label="t('tenants.name')"
          prop="name"
        >
          <el-input
            v-model="createForm.name"
            :placeholder="t('tenants.namePlaceholder')"
          />
        </el-form-item>
        <el-form-item
          :label="t('tenants.code')"
          prop="code"
        >
          <el-input
            v-model="createForm.code"
            :placeholder="t('tenants.codePlaceholder')"
          />
        </el-form-item>
        <el-form-item
          :label="t('tenants.quotaDevices')"
          prop="quota_devices"
        >
          <el-input-number
            v-model="createForm.quota_devices"
            :min="1"
            :max="10000"
            :step="100"
            controls-position="right"
            class="full"
          />
          <div class="hint">
            {{ t('tenants.quotaHints.devices') }}
          </div>
        </el-form-item>
        <el-form-item
          :label="t('tenants.quotaCallsMonthly')"
          prop="quota_calls_monthly"
        >
          <el-input-number
            v-model="createForm.quota_calls_monthly"
            :min="0"
            :step="10000"
            controls-position="right"
            class="full"
          />
          <div class="hint">
            {{ t('tenants.quotaHints.callsMonthly') }}
          </div>
        </el-form-item>
        <el-form-item
          :label="t('tenants.quotaConcurrent')"
          prop="quota_concurrent"
        >
          <el-input-number
            v-model="createForm.quota_concurrent"
            :min="1"
            :step="1"
            controls-position="right"
            class="full"
          />
          <div class="hint">
            {{ t('tenants.quotaHints.concurrent') }}
          </div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createVisible = false">
          {{ t('common.cancel') }}
        </el-button>
        <el-button
          type="primary"
          :loading="saving"
          @click="submitCreate"
        >
          {{ t('common.create') }}
        </el-button>
      </template>
    </el-dialog>

    <!-- Edit quota dialog (PATCH /v1/admin/tenants/{id}, design/33 3.1.5) -->
    <el-dialog
      v-model="editVisible"
      :title="t('tenants.editTitle')"
      width="480px"
    >
      <div class="edit-name">
        {{ editingTenant?.name }}
      </div>
      <el-form
        ref="editFormRef"
        :model="editForm"
        :rules="editRules"
        label-width="120px"
      >
        <el-form-item
          :label="t('tenants.quotaDevices')"
          prop="quota_devices"
        >
          <el-input-number
            v-model="editForm.quota_devices"
            :min="1"
            :max="10000"
            :step="100"
            controls-position="right"
            class="full"
          />
        </el-form-item>
        <el-form-item
          :label="t('tenants.quotaCallsMonthly')"
          prop="quota_calls_monthly"
        >
          <el-input-number
            v-model="editForm.quota_calls_monthly"
            :min="0"
            :step="10000"
            controls-position="right"
            class="full"
          />
        </el-form-item>
        <el-form-item
          :label="t('tenants.quotaConcurrent')"
          prop="quota_concurrent"
        >
          <el-input-number
            v-model="editForm.quota_concurrent"
            :min="1"
            :step="1"
            controls-position="right"
            class="full"
          />
        </el-form-item>
        <el-form-item
          :label="t('common.reason')"
          prop="change_reason"
        >
          <el-input
            v-model="editForm.change_reason"
            type="textarea"
            :rows="2"
            :placeholder="t('common.reasonPlaceholder')"
          />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="editVisible = false">
          {{ t('common.cancel') }}
        </el-button>
        <el-button
          type="primary"
          :loading="saving"
          @click="submitEdit"
        >
          {{ t('common.save') }}
        </el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'
import { api } from '@/api/request'
import { errorMessage } from '@/api/errors'
import QuotaCell from '@/components/QuotaCell.vue'
import { formatDate, formatTime } from '@/utils/format'
import type { CreateTenantPayload, Page, Tenant, TenantPatchPayload, TenantQuota } from '@/api/types'

const { t, tm } = useI18n()
const router = useRouter()

// Design/32 adc_tenants defaults (100 devices / 100000 calls / 10 concurrent);
// the device quota is hard-capped at 10000 per NFR-002.
const QUOTA_DEFAULTS: TenantQuota = { quota_devices: 100, quota_calls_monthly: 100000, quota_concurrent: 10 }

const loading = ref(false)
const list = ref<Tenant[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)

const filters = reactive({ keyword: '', status: '' })

const hasFilters = computed(() => Boolean(filters.keyword || filters.status))

// Wire statuses come lowercase from design/33 ('active'/'disabled') but
// design/32 stores uppercase ('ACTIVE'/'SUSPENDED'); normalize both for the
// tag lookup and always send the design/33 values back on PATCH.
function statusKey(status: string): string {
  const key = String(status).toLowerCase()
  return key === 'suspended' ? 'disabled' : key
}

function statusTagType(status: string): 'success' | 'danger' | 'warning' | 'info' | 'primary' {
  switch (String(status).toLowerCase()) {
    case 'active': return 'success'
    case 'deleting': return 'warning'
    case 'deleted': return 'info'
    default: return 'danger'
  }
}

// The list endpoint (design/33 3.1.2) does not carry quota/usage, so rows
// missing those counters are enriched from the detail endpoint
// (design/33 3.1.4) in parallel. Quota keys accept both the design/32 column
// names and the legacy design/33 3.1.3 names (max_devices etc.).
function normalizeQuota(raw: unknown): TenantQuota | undefined {
  if (!raw || typeof raw !== 'object') return undefined
  const q = raw as Record<string, unknown>
  const devices = q.quota_devices ?? q.max_devices
  const calls = q.quota_calls_monthly ?? q.monthly_call_limit
  const concurrent = q.quota_concurrent ?? q.max_concurrent
  if (typeof devices !== 'number' && typeof calls !== 'number' && typeof concurrent !== 'number') return undefined
  return {
    quota_devices: typeof devices === 'number' ? devices : 0,
    quota_calls_monthly: typeof calls === 'number' ? calls : 0,
    quota_concurrent: typeof concurrent === 'number' ? concurrent : 0,
  }
}

async function enrichRows(items: Tenant[]): Promise<Tenant[]> {
  const rows = items.map((item) => ({ ...item }))
  const missing = rows.filter((r) => !r.quota || r.used_devices === undefined || r.used_calls_month === undefined)
  await Promise.allSettled(
    missing.map(async (row) => {
      const detail = await api.get<Tenant>(`/v1/admin/tenants/${row.tenant_id}`)
      if (detail.quota) row.quota = normalizeQuota(detail.quota)
      if (detail.used_devices !== undefined) row.used_devices = detail.used_devices
      if (detail.used_calls_month !== undefined) row.used_calls_month = detail.used_calls_month
      if (detail.expires_at !== undefined) row.expires_at = detail.expires_at
    }),
  )
  return rows
}

// GET /v1/admin/tenants (design/33 3.1.2) with filters and offset pagination.
async function fetchList() {
  loading.value = true
  try {
    const res = await api.get<Page<Tenant>>('/v1/admin/tenants', {
      page: page.value,
      page_size: pageSize.value,
      status: filters.status || undefined,
      keyword: filters.keyword || undefined,
    })
    list.value = await enrichRows(res.items ?? [])
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
  search()
}

function onSizeChange() {
  page.value = 1
  fetchList()
}

// Create form
const createVisible = ref(false)
const createFormRef = ref<FormInstance>()
const createForm = reactive({ ...QUOTA_DEFAULTS, name: '', code: '' })

const createRules: FormRules = {
  name: [{ required: true, message: t('tenants.namePlaceholder'), trigger: 'blur' }],
  code: [
    { required: true, message: t('tenants.codePlaceholder'), trigger: 'blur' },
    { pattern: /^[A-Za-z0-9_-]{1,64}$/, message: t('tenants.codePattern'), trigger: 'blur' },
  ],
  quota_devices: [{ required: true, type: 'number', message: t('tenants.quotaDevices'), trigger: 'change' }],
  quota_calls_monthly: [{ required: true, type: 'number', message: t('tenants.quotaCallsMonthly'), trigger: 'change' }],
  quota_concurrent: [{ required: true, type: 'number', message: t('tenants.quotaConcurrent'), trigger: 'change' }],
}

function openCreate() {
  createFormRef.value?.resetFields()
  Object.assign(createForm, { ...QUOTA_DEFAULTS, name: '', code: '' })
  createVisible.value = true
}

const saving = ref(false)

async function submitCreate() {
  const valid = await createFormRef.value?.validate().catch(() => false)
  if (!valid) return
  saving.value = true
  try {
    const payload: CreateTenantPayload = {
      name: createForm.name.trim(),
      code: createForm.code.trim(),
      quota: {
        quota_devices: createForm.quota_devices,
        quota_calls_monthly: createForm.quota_calls_monthly,
        quota_concurrent: createForm.quota_concurrent,
      },
    }
    await api.post<Tenant>('/v1/admin/tenants', payload)
    createVisible.value = false
    ElMessage.success(t('tenants.createSuccess'))
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    saving.value = false
  }
}

// Edit quota form
const editVisible = ref(false)
const editFormRef = ref<FormInstance>()
const editingTenant = ref<Tenant>()
const editForm = reactive({ ...QUOTA_DEFAULTS, change_reason: '' })

const editRules: FormRules = {
  change_reason: [{ required: true, message: t('common.reasonPlaceholder'), trigger: 'blur' }],
  quota_devices: [{ required: true, type: 'number', message: t('tenants.quotaDevices'), trigger: 'change' }],
  quota_calls_monthly: [{ required: true, type: 'number', message: t('tenants.quotaCallsMonthly'), trigger: 'change' }],
  quota_concurrent: [{ required: true, type: 'number', message: t('tenants.quotaConcurrent'), trigger: 'change' }],
}

function openEdit(row: Tenant) {
  editingTenant.value = row
  editFormRef.value?.resetFields()
  Object.assign(editForm, {
    quota_devices: row.quota?.quota_devices ?? QUOTA_DEFAULTS.quota_devices,
    quota_calls_monthly: row.quota?.quota_calls_monthly ?? QUOTA_DEFAULTS.quota_calls_monthly,
    quota_concurrent: row.quota?.quota_concurrent ?? QUOTA_DEFAULTS.quota_concurrent,
    change_reason: '',
  })
  editVisible.value = true
}

async function submitEdit() {
  const valid = await editFormRef.value?.validate().catch(() => false)
  if (!valid || !editingTenant.value) return
  // design/32 enforces used_* <= quota_*; mirror the check client-side so
  // the server error 13003 is reserved for race conditions.
  const tenant = editingTenant.value
  if (tenant.used_devices !== undefined && editForm.quota_devices < tenant.used_devices) {
    ElMessage.warning(t('tenants.quotaBelowUsed'))
    return
  }
  if (tenant.used_calls_month !== undefined && editForm.quota_calls_monthly < tenant.used_calls_month) {
    ElMessage.warning(t('tenants.quotaBelowUsed'))
    return
  }
  saving.value = true
  try {
    const payload: TenantPatchPayload = {
      quota: {
        quota_devices: editForm.quota_devices,
        quota_calls_monthly: editForm.quota_calls_monthly,
        quota_concurrent: editForm.quota_concurrent,
      },
      change_reason: editForm.change_reason.trim(),
    }
    await api.patch<Tenant>(`/v1/admin/tenants/${tenant.tenant_id}`, payload)
    editVisible.value = false
    ElMessage.success(t('tenants.quotaSaveSuccess'))
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    saving.value = false
  }
}

// Status toggle: disabling a tenant is a high-risk operation (kicks all
// devices offline, expires in-flight tickets, design/33 3.1.5); the reason
// is mandatory and audited.
const actionReason = ref('')

async function runStatusToggle(row: Tenant) {
  const reason = actionReason.value.trim()
  actionReason.value = ''
  if (!reason) {
    ElMessage.warning(t('common.reasonRequired'))
    return
  }
  try {
    const target = statusKey(row.status) === 'active' ? 'disabled' : 'active'
    const payload: TenantPatchPayload = { status: target, change_reason: reason }
    await api.patch<Tenant>(`/v1/admin/tenants/${row.tenant_id}`, payload)
    ElMessage.success(t(target === 'disabled' ? 'tenants.disableSuccess' : 'tenants.enableSuccess'))
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  }
}

function openMembers(row: Tenant) {
  router.push(`/tenants/${row.tenant_id}/users`)
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
.hint { width: 100%; font-size: 12px; color: var(--adc-text-secondary); line-height: 1.4; }
.reason { padding: var(--adc-space-2) 0; }
.edit-name { font-weight: 600; margin-bottom: var(--adc-space-3); }
.muted { color: var(--adc-text-secondary); }
</style>
