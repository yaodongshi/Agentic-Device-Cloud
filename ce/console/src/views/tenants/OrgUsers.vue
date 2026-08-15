<template>
  <el-card>
    <div class="toolbar">
      <div class="left">
        <el-button @click="backToTenants">
          {{ t('orgUsers.backToTenants') }}
        </el-button>
        <span class="tenant-name">{{ t('orgUsers.tenantLabel', { name: tenantName }) }}</span>
      </div>
      <div class="filters">
        <el-input
          v-model="filters.keyword"
          class="filter-item keyword"
          :placeholder="t('common.keywordPlaceholder')"
          clearable
          @keyup.enter="search"
        />
        <el-select
          v-model="filters.role"
          class="filter-item"
          :placeholder="t('orgUsers.role')"
          clearable
        >
          <el-option
            v-for="(label, key) in tm('orgUsers.assignableRoles')"
            :key="key"
            :label="label"
            :value="key"
          />
        </el-select>
        <el-select
          v-model="filters.status"
          class="filter-item"
          :placeholder="t('orgUsers.statusLabel')"
          clearable
        >
          <el-option
            v-for="(label, key) in tm('orgUsers.statuses')"
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

    <el-alert
      class="roles-hint"
      type="info"
      :title="t('orgUsers.rolesHint')"
      :closable="false"
      show-icon
    />

    <el-table
      v-loading="loading"
      :data="list"
    >
      <el-table-column
        prop="display_name"
        :label="t('orgUsers.name')"
        min-width="140"
      />
      <el-table-column
        prop="account"
        :label="t('orgUsers.account')"
        min-width="140"
      />
      <el-table-column
        :label="t('orgUsers.role')"
        width="160"
      >
        <template #default="{ row }">
          {{ roleLabel(row.role) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('orgUsers.statusLabel')"
        width="110"
      >
        <template #default="{ row }">
          <el-tag
            :type="statusTagType(row.status)"
            effect="light"
          >
            {{ t(`orgUsers.statuses.${statusKey(row.status)}`) }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column
        :label="t('orgUsers.createdAt')"
        width="170"
      >
        <template #default="{ row }">
          {{ formatTime(row.created_at) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('common.actions')"
        width="160"
        fixed="right"
      >
        <template #default="{ row }">
          <el-button
            link
            type="primary"
            @click="openEdit(row)"
          >
            {{ t('orgUsers.edit') }}
          </el-button>
          <el-popconfirm
            :title="row.status === 'active' ? t('orgUsers.confirmDisable') : t('orgUsers.confirmEnable')"
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
                {{ row.status === 'active' ? t('orgUsers.disable') : t('orgUsers.enable') }}
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
        <el-empty :description="hasFilters ? t('orgUsers.searchEmpty') : t('orgUsers.empty')" />
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

    <!-- Role edit dialog: role assignment among tenant_admin/approver/auditor.
         Update endpoint contract is still open (design/33 3.1.18 only lists
         members), so the PATCH payload follows the tenant conventions. -->
    <el-dialog
      v-model="editVisible"
      :title="t('orgUsers.editTitle')"
      width="440px"
    >
      <div class="edit-name">
        {{ editingUser?.display_name }}（{{ editingUser?.account }}）
      </div>
      <el-form
        ref="editFormRef"
        :model="editForm"
        :rules="editRules"
        label-width="100px"
      >
        <el-form-item
          :label="t('orgUsers.role')"
          prop="role"
        >
          <el-select
            v-model="editForm.role"
            class="full"
          >
            <el-option
              v-for="(label, key) in tm('orgUsers.assignableRoles')"
              :key="key"
              :label="label"
              :value="key"
            />
          </el-select>
        </el-form-item>
        <el-form-item
          :label="t('common.reason')"
          prop="change_reason"
        >
          <el-input
            v-model="editForm.change_reason"
            type="textarea"
            :rows="2"
            :placeholder="t('orgUsers.reasonPlaceholder')"
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
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'
import { api } from '@/api/request'
import { errorMessage } from '@/api/errors'
import { formatTime } from '@/utils/format'
import type { OrgUser, OrgUserPatchPayload, OrgUserRole, Page } from '@/api/types'

const { t, tm } = useI18n()
const route = useRoute()
const router = useRouter()

const tenantId = String(route.params.id ?? '')

const loading = ref(false)
const list = ref<OrgUser[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const tenantName = ref('')

const filters = reactive({ keyword: '', role: '', status: '' })

const hasFilters = computed(() => Boolean(filters.keyword || filters.role || filters.status))

// V1.0 assignable org roles: tenant_admin (tenant-level admin, the "admin"
// of this page), approver (HITL), auditor (read-only). platform_admin is a
// platform-level role and is not assignable per tenant (design/33 3.1.18).
function roleLabel(role: OrgUserRole): string {
  if (role === 'platform_admin') return t('layout.roles.platform_admin')
  return t(`orgUsers.assignableRoles.${role}`) ?? role
}

function statusKey(status: string): string {
  return String(status).toLowerCase() === 'suspended' ? 'disabled' : String(status).toLowerCase()
}

function statusTagType(status: string): 'success' | 'danger' | 'info' {
  return statusKey(status) === 'active' ? 'success' : 'danger'
}

// GET /v1/admin/org/users (design/33 3.1.18). role/status are not in the
// contract's query set yet; they are sent anyway and ignored by a server
// that does not support them (契约待补 for the filter params).
async function fetchList() {
  loading.value = true
  try {
    const res = await api.get<Page<OrgUser>>('/v1/admin/org/users', {
      page: page.value,
      page_size: pageSize.value,
      keyword: filters.keyword || undefined,
      role: filters.role || undefined,
      status: filters.status || undefined,
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
  filters.role = ''
  filters.status = ''
  search()
}

function onSizeChange() {
  page.value = 1
  fetchList()
}

function backToTenants() {
  router.push('/tenants')
}

// Role edit form
const editVisible = ref(false)
const editFormRef = ref<FormInstance>()
const editingUser = ref<OrgUser>()
const editForm = reactive({ role: 'auditor' as OrgUserRole, change_reason: '' })

const editRules: FormRules = {
  role: [{ required: true, message: t('orgUsers.role'), trigger: 'change' }],
  change_reason: [{ required: true, message: t('orgUsers.reasonPlaceholder'), trigger: 'blur' }],
}

function openEdit(row: OrgUser) {
  editingUser.value = row
  editFormRef.value?.resetFields()
  Object.assign(editForm, { role: row.role, change_reason: '' })
  editVisible.value = true
}

const saving = ref(false)

async function submitEdit() {
  const valid = await editFormRef.value?.validate().catch(() => false)
  if (!valid || !editingUser.value) return
  saving.value = true
  try {
    // 契约待补: role/status update endpoint, see OrgUserPatchPayload in
    // api/types.ts.
    const payload: OrgUserPatchPayload = { role: editForm.role, change_reason: editForm.change_reason.trim() }
    await api.patch<OrgUser>(`/v1/admin/org/users/${editingUser.value.user_id}`, payload)
    editVisible.value = false
    ElMessage.success(t('orgUsers.roleSaveSuccess'))
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    saving.value = false
  }
}

// Disabling a member is a high-risk operation (design/20 4.11); the reason
// is mandatory and audited.
const actionReason = ref('')

async function runStatusToggle(row: OrgUser) {
  const reason = actionReason.value.trim()
  actionReason.value = ''
  if (!reason) {
    ElMessage.warning(t('common.reasonRequired'))
    return
  }
  try {
    // 契约待补: same open PATCH endpoint as the role change above.
    const target = statusKey(row.status) === 'active' ? 'disabled' : 'active'
    const payload: OrgUserPatchPayload = { status: target, change_reason: reason }
    await api.patch<OrgUser>(`/v1/admin/org/users/${row.user_id}`, payload)
    ElMessage.success(t(target === 'disabled' ? 'orgUsers.disableSuccess' : 'orgUsers.enableSuccess'))
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  }
}

// Best-effort tenant name for the page subtitle; the members list is the
// authoritative data and the title degrades gracefully to the raw id.
async function fetchTenantName() {
  try {
    const detail = await api.get<{ name?: string }>(`/v1/admin/tenants/${tenantId}`)
    tenantName.value = detail.name ?? tenantId
  } catch {
    tenantName.value = tenantId
  }
}

onMounted(() => {
  fetchTenantName()
  fetchList()
})
</script>

<style scoped>
.toolbar { display: flex; align-items: flex-start; justify-content: space-between; margin-bottom: var(--adc-space-4); }
.left { display: flex; align-items: center; gap: var(--adc-space-3); }
.tenant-name { font-weight: 600; color: var(--adc-text-secondary); }
.filters { display: flex; flex-wrap: wrap; gap: var(--adc-space-2); }
.filter-item { width: 170px; }
.keyword { width: 200px; }
.roles-hint { margin-bottom: var(--adc-space-3); }
.pagination { margin-top: var(--adc-space-4); justify-content: flex-end; }
.full { width: 100%; }
.reason { padding: var(--adc-space-2) 0; }
.edit-name { font-weight: 600; margin-bottom: var(--adc-space-3); }
</style>
