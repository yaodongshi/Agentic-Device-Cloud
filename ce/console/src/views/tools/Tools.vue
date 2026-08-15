<template>
  <el-card>
    <div class="toolbar">
      <el-select
        v-model="deviceId"
        class="device-select"
        :placeholder="t('tools.devicePlaceholder')"
        filterable
        remote
        :remote-method="searchDevices"
        :loading="deviceLoading"
        @change="onDeviceChange"
      >
        <el-option
          v-for="d in devices"
          :key="d.device_id"
          :label="`${d.device_code}${d.name ? ' / ' + d.name : ''}`"
          :value="d.device_id"
        />
      </el-select>
      <span class="hint">{{ t('tools.selectHint') }}</span>
    </div>

    <el-table
      v-loading="loading"
      :data="list"
      row-key="name"
    >
      <el-table-column type="expand">
        <template #default="{ row }">
          <div class="schema">
            <div class="schema-head">
              {{ t('tools.schemaTitle') }}
              <span class="muted">{{ t('tools.schemaVersion', { n: row.schema_version }) }}</span>
            </div>
            <pre class="mono">{{ prettyJson(row.input_schema) }}</pre>
          </div>
        </template>
      </el-table-column>
      <el-table-column
        prop="name"
        :label="t('tools.name')"
        min-width="200"
      >
        <template #default="{ row }">
          <span class="mono">{{ row.name }}</span>
        </template>
      </el-table-column>
      <el-table-column
        prop="description"
        :label="t('tools.description')"
        min-width="220"
        show-overflow-tooltip
      />
      <el-table-column
        :label="t('tools.riskLevel')"
        width="170"
      >
        <template #default="{ row }">
          <RiskTag :level="row.risk_level" />
        </template>
      </el-table-column>
      <el-table-column
        :label="t('tools.riskDescription')"
        min-width="220"
      >
        <template #default="{ row }">
          {{ t(`tools.riskDescriptions.${row.risk_level}`) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('tools.updatedAt')"
        width="170"
      >
        <template #default="{ row }">
          {{ formatTime(row.updated_at) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('common.actions')"
        width="120"
        fixed="right"
      >
        <template #default="{ row }">
          <el-button
            link
            type="primary"
            @click="openEdit(row)"
          >
            {{ t('tools.editLevel') }}
          </el-button>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty :description="deviceId ? t('tools.searchEmpty') : t('tools.empty')" />
      </template>
    </el-table>

    <el-pagination
      v-model:current-page="page"
      v-model:page-size="pageSize"
      class="pagination"
      background
      layout="total, prev, pager, next"
      :total="total"
      :page-sizes="[20, 50]"
      @current-change="fetchTools"
    />

    <!-- Risk level edit dialog (design/20 4.5): downgrade forces a
         double confirm + mandatory reason (10-200 chars). -->
    <el-dialog
      v-model="editVisible"
      :title="t('tools.editTitle')"
      width="480px"
    >
      <div class="current-line">
        {{ t('tools.editToolName') }}<span class="mono">{{ editing?.name }}</span>
        {{ t('tools.editCurrent') }}<RiskTag :level="editing?.risk_level ?? 2" />
      </div>
      <el-form
        ref="editFormRef"
        :model="editForm"
        :rules="editRules"
        label-width="90px"
      >
        <el-form-item
          :label="t('tools.newLevel')"
          prop="risk_level"
        >
          <el-select
            v-model="editForm.risk_level"
            class="full"
          >
            <el-option
              v-for="(label, key) in t('tools.riskLevels')"
              :key="key"
              :label="label"
              :value="Number(key)"
            />
          </el-select>
        </el-form-item>
        <el-form-item
          v-if="isDowngrade"
          :label="t('common.reason')"
          prop="reason"
        >
          <el-input
            v-model="editForm.reason"
            type="textarea"
            :rows="3"
            maxlength="200"
            show-word-limit
            :placeholder="t('tools.downgradeReasonPlaceholder')"
          />
        </el-form-item>
      </el-form>
      <el-alert
        v-if="isDowngrade"
        type="warning"
        :closable="false"
        show-icon
        :title="t('tools.downgradeWarning')"
      />
      <el-alert
        v-else
        type="info"
        :closable="false"
        show-icon
        :title="t('tools.upgradeHint')"
      />
      <template #footer>
        <el-button @click="editVisible = false">
          {{ t('common.cancel') }}
        </el-button>
        <el-button
          type="danger"
          :loading="saving"
          @click="submitEdit"
        >
          {{ isDowngrade ? t('tools.confirmDowngrade') : t('common.save') }}
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
import { api, request } from '@/api/request'
import { errorMessage } from '@/api/errors'
import { formatTime } from '@/utils/format'
import RiskTag from '@/components/RiskTag.vue'
import type { Device, DeviceTool, Page, ToolRiskLevel } from '@/api/types'

const { t } = useI18n()

// Device selector feeds the tool table (design/20 4.5: the catalog is
// scoped per device; FR-006 list endpoint takes device_id in the path).
const devices = ref<Device[]>([])
const deviceLoading = ref(false)
const deviceId = ref('')

// GET /v1/admin/devices (design/33 3.1.7), reused for the remote selector.
async function searchDevices(keyword?: string) {
  deviceLoading.value = true
  try {
    const res = await api.get<Page<Device>>('/v1/admin/devices', {
      page: 1,
      page_size: 200,
      keyword: keyword || undefined,
    })
    devices.value = res.items ?? []
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    deviceLoading.value = false
  }
}

const loading = ref(false)
const list = ref<DeviceTool[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)

// GET /v1/admin/devices/{id}/tools (design/33 3.1.10).
async function fetchTools() {
  if (!deviceId.value) {
    list.value = []
    total.value = 0
    return
  }
  loading.value = true
  try {
    const res = await api.get<Page<DeviceTool>>(`/v1/admin/devices/${deviceId.value}/tools`, {
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

function onDeviceChange() {
  page.value = 1
  fetchTools()
}

// input_schema preview: pretty-printed JSON inside the expandable row.
function prettyJson(schema: Record<string, unknown> | undefined | null): string {
  if (!schema) return '{}'
  try {
    return JSON.stringify(schema, null, 2)
  } catch {
    return String(schema)
  }
}

// Level edit: downgrade = 2/3 -> 0/1, which skips HITL and therefore needs
// a second confirm (header X-ADC-Confirm, design/33 3.1.11) and an audited
// reason of 10-200 chars (design/20 5.3).
const editVisible = ref(false)
const saving = ref(false)
const editing = ref<DeviceTool | null>(null)
const editFormRef = ref<FormInstance>()
const editForm = reactive({ risk_level: 2 as ToolRiskLevel, reason: '' })

const isDowngrade = computed(
  () => !!editing.value && editing.value.risk_level >= 2 && editForm.risk_level <= 1,
)

const editRules = computed<FormRules>(() => ({
  risk_level: [{ required: true, message: t('tools.newLevel'), trigger: 'change' }],
  reason: isDowngrade.value
    ? [
        { required: true, message: t('tools.downgradeReasonPlaceholder'), trigger: 'blur' },
        { min: 10, max: 200, message: t('tools.reasonLength'), trigger: 'blur' },
      ]
    : [],
}))

function openEdit(row: DeviceTool) {
  editing.value = row
  editForm.risk_level = row.risk_level
  editForm.reason = ''
  editVisible.value = true
}

async function submitEdit() {
  const valid = await editFormRef.value?.validate().catch(() => false)
  if (!valid || !editing.value) return
  saving.value = true
  try {
    const payload = {
      changes: [{ name: editing.value.name, risk_level: editForm.risk_level }],
      change_reason: editForm.reason.trim() || `risk level ${editing.value.risk_level} -> ${editForm.risk_level}`,
    }
    if (isDowngrade.value) {
      // Downgrade double confirm header (design/33 3.1.11); the gateway
      // forwards it to the Admin API which refuses the change without it.
      await request(`/v1/admin/devices/${deviceId.value}/tools`, {
        method: 'PATCH',
        headers: { 'X-ADC-Confirm': 'true' },
        body: JSON.stringify(payload),
      })
    } else {
      await api.patch(`/v1/admin/devices/${deviceId.value}/tools`, payload)
    }
    editVisible.value = false
    // Config takes effect on the data plane within 30s (design/20 5.3).
    ElMessage.success(t('tools.saveSuccess'))
    await fetchTools()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    saving.value = false
  }
}

onMounted(() => {
  searchDevices()
})
</script>

<style scoped>
.toolbar { display: flex; align-items: center; gap: var(--adc-space-3); margin-bottom: var(--adc-space-4); }
.device-select { width: 320px; }
.hint { color: var(--adc-text-secondary); }
.pagination { margin-top: var(--adc-space-4); justify-content: flex-end; }
.full { width: 100%; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.schema { padding: var(--adc-space-2) var(--adc-space-4) var(--adc-space-4); }
.schema-head { margin-bottom: var(--adc-space-2); color: var(--adc-text); }
.schema-head .muted { margin-left: var(--adc-space-2); color: var(--adc-text-secondary); font-size: 12px; }
.schema pre {
  margin: 0;
  padding: var(--adc-space-3);
  background: var(--adc-bg);
  border: 1px solid var(--adc-border);
  border-radius: var(--adc-radius);
  font-size: 12px;
  line-height: 1.6;
  max-height: 320px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
.current-line { display: flex; align-items: center; gap: var(--adc-space-2); margin-bottom: var(--adc-space-4); flex-wrap: wrap; }
</style>
