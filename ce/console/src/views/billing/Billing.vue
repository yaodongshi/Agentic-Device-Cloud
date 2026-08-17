<template>
  <el-card>
    <div class="toolbar">
      <div class="filters">
        <el-select
          v-model="tenantId"
          class="filter-item tenant"
          :placeholder="t('billing.tenantPlaceholder')"
          filterable
          :loading="tenantsLoading"
          @change="onTenantChange"
        >
          <el-option
            v-for="item in tenants"
            :key="item.tenant_id"
            :label="item.name"
            :value="item.tenant_id"
          />
        </el-select>
        <el-date-picker
          v-model="generateMonth"
          class="filter-item"
          type="month"
          value-format="YYYY-MM"
          :placeholder="t('billing.monthPlaceholder')"
          :clearable="false"
        />
        <el-button
          type="primary"
          :disabled="!tenantId || !generateMonth"
          :loading="generating"
          @click="runGenerate"
        >
          {{ t('billing.generate') }}
        </el-button>
      </div>
    </div>

    <el-table
      v-loading="loading"
      :data="list"
    >
      <el-table-column
        prop="period"
        :label="t('billing.period')"
        width="110"
      />
      <el-table-column
        prop="device_peak"
        :label="t('billing.devicePeak')"
        width="110"
      />
      <el-table-column
        prop="tool_calls"
        :label="t('billing.toolCalls')"
        min-width="110"
      >
        <template #default="{ row }">
          {{ fmtNum(row.tool_calls) }}
        </template>
      </el-table-column>
      <el-table-column
        prop="tokens"
        :label="t('billing.tokens')"
        min-width="110"
      >
        <template #default="{ row }">
          {{ fmtNum(row.tokens) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('billing.subscriptionFee')"
        width="120"
      >
        <template #default="{ row }">
          {{ fmtMoney(row.subscription_fee_fen) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('billing.deviceFee')"
        width="120"
      >
        <template #default="{ row }">
          {{ fmtMoney(row.device_fee_fen) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('billing.tokenFee')"
        width="120"
      >
        <template #default="{ row }">
          {{ fmtMoney(row.token_fee_fen) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('billing.totalFee')"
        width="130"
      >
        <template #default="{ row }">
          <strong>{{ fmtMoney(row.total_fee_fen) }}</strong>
        </template>
      </el-table-column>
      <el-table-column
        :label="t('billing.statusLabel')"
        width="110"
      >
        <template #default="{ row }">
          <el-tag
            :type="statusTagType(row.status)"
            effect="light"
          >
            {{ t(`billing.statuses.${statusKey(row.status)}`) }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column
        :label="t('common.actions')"
        width="100"
        fixed="right"
      >
        <template #default="{ row }">
          <el-button
            link
            type="primary"
            @click="openDetail(row)"
          >
            {{ t('billing.detail') }}
          </el-button>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty :description="t('billing.empty')" />
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

    <!-- Bill detail: fee lines, device ladder breakdown and the audit
         reconciliation assertion (FR-016 acceptance). -->
    <el-drawer
      v-model="detailVisible"
      :title="t('billing.detailTitle', { period: detailPeriod })"
      size="480px"
    >
      <template v-if="detail">
        <el-descriptions
          :column="1"
          border
        >
          <el-descriptions-item :label="t('billing.period')">
            {{ detail.period }}
          </el-descriptions-item>
          <el-descriptions-item :label="t('billing.devicePeak')">
            {{ detail.device_peak }}
          </el-descriptions-item>
          <el-descriptions-item :label="t('billing.toolCalls')">
            {{ fmtNum(detail.tool_calls) }}
          </el-descriptions-item>
          <el-descriptions-item :label="t('billing.tokens')">
            {{ fmtNum(detail.tokens) }}
          </el-descriptions-item>
          <el-descriptions-item :label="t('billing.subscriptionFee')">
            {{ fmtMoney(detail.subscription_fee_fen) }}
          </el-descriptions-item>
          <el-descriptions-item :label="t('billing.deviceFee')">
            {{ fmtMoney(detail.device_fee_fen) }}
          </el-descriptions-item>
          <el-descriptions-item :label="t('billing.tokenFee')">
            {{ fmtMoney(detail.token_fee_fen) }}
          </el-descriptions-item>
          <el-descriptions-item :label="t('billing.totalFee')">
            <strong>{{ fmtMoney(detail.total_fee_fen) }}</strong>
          </el-descriptions-item>
        </el-descriptions>

        <h4 class="section-title">
          {{ t('billing.breakdown') }}
        </h4>
        <el-table :data="breakdownRows(detail)">
          <el-table-column
            prop="tier"
            :label="t('billing.breakdownTier')"
          />
          <el-table-column
            prop="count"
            :label="t('billing.breakdownCount')"
            width="120"
          />
        </el-table>

        <h4 class="section-title">
          {{ t('billing.reconciliation') }}
        </h4>
        <template v-if="reconciliation">
          <el-alert
            v-if="reconciliation.matched"
            :title="t('billing.reconciliationMatch')"
            type="success"
            :closable="false"
            show-icon
          />
          <el-alert
            v-else
            :title="t('billing.reconciliationMismatch', {
              usage: reconciliation.tool_calls_usage,
              audit: reconciliation.tool_calls_audit,
            })"
            type="warning"
            :closable="false"
            show-icon
          />
        </template>
        <span
          v-else
          class="muted"
        >{{ t('billing.reconciliationUnavailable') }}</span>
      </template>
    </el-drawer>
  </el-card>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage } from 'element-plus'
import { errorMessage } from '@/api/errors'
import type { Tenant } from '@/api/types'
import { api } from '@/api/request'
import {
  fenToYuan,
  fetchStatementDetail,
  fetchStatements,
  generateStatement,
  type BillingReconciliation,
  type BillingStatement,
} from '@/api/billing'

const { t } = useI18n()

const tenantsLoading = ref(false)
const tenants = ref<Tenant[]>([])
const tenantId = ref('')

const loading = ref(false)
const list = ref<BillingStatement[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)

const generateMonth = ref('')
const generating = ref(false)

const detailVisible = ref(false)
const detail = ref<BillingStatement>()
const reconciliation = ref<BillingReconciliation | null>(null)

const detailPeriod = computed(() => detail.value?.period ?? '')

// The billing page is a platform_admin exclusive (router + menu), so the
// tenant scope is picked explicitly here and sent as tenant_id on every
// call (design/33 1.2).
async function fetchTenants() {
  tenantsLoading.value = true
  try {
    const res = await api.get<{ items: Tenant[] }>('/v1/admin/tenants', { page: 1, page_size: 200 })
    tenants.value = res.items ?? []
    if (!tenantId.value && tenants.value.length > 0) {
      tenantId.value = tenants.value[0].tenant_id
      fetchList()
    }
  } catch (err) {
    ElMessage.error(t('billing.loadTenantsFailed'))
    void err
  } finally {
    tenantsLoading.value = false
  }
}

async function fetchList() {
  if (!tenantId.value) return
  loading.value = true
  try {
    const res = await fetchStatements(tenantId.value, page.value, pageSize.value)
    list.value = res.items ?? []
    total.value = res.total ?? 0
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    loading.value = false
  }
}

function onTenantChange() {
  page.value = 1
  fetchList()
}

function onSizeChange() {
  page.value = 1
  fetchList()
}

async function runGenerate() {
  if (!tenantId.value) {
    ElMessage.warning(t('billing.noTenant'))
    return
  }
  if (!generateMonth.value) {
    ElMessage.warning(t('billing.noMonth'))
    return
  }
  generating.value = true
  try {
    const res = await generateStatement(tenantId.value, generateMonth.value)
    ElMessage.success(res.created ? t('billing.generateSuccess') : t('billing.generateExists'))
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    generating.value = false
  }
}

async function openDetail(row: BillingStatement) {
  detail.value = undefined
  reconciliation.value = null
  detailVisible.value = true
  try {
    const res = await fetchStatementDetail(row.id)
    detail.value = res.statement
    reconciliation.value = res.reconciliation
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  }
}

function breakdownRows(st: BillingStatement) {
  const b = st.breakdown
  return [
    { tier: t('billing.breakdownIncluded'), count: b.included_devices },
    { tier: t('billing.breakdownTier400'), count: b.tier_400_devices },
    { tier: t('billing.breakdownTier300'), count: b.tier_300_devices },
    { tier: t('billing.breakdownTier250'), count: b.tier_250_devices },
  ]
}

function statusKey(status: string): string {
  switch (status) {
    case 'GENERATED': return 'GENERATED'
    case 'PAID': return 'PAID'
    case 'OVERDUE': return 'OVERDUE'
    default: return 'GENERATED'
  }
}

function statusTagType(status: string): 'success' | 'danger' | 'info' {
  switch (status) {
    case 'PAID': return 'success'
    case 'OVERDUE': return 'danger'
    default: return 'info'
  }
}

function fmtMoney(fen: number): string {
  return `¥ ${fenToYuan(fen)}`
}

function fmtNum(n: number): string {
  return new Intl.NumberFormat(undefined).format(n)
}

onMounted(() => {
  fetchTenants()
})
</script>

<style scoped>
.toolbar { display: flex; align-items: flex-start; justify-content: space-between; margin-bottom: var(--adc-space-4); }
.filters { display: flex; flex-wrap: wrap; gap: var(--adc-space-2); }
.filter-item { width: 220px; }
.tenant { width: 280px; }
.pagination { margin-top: var(--adc-space-4); justify-content: flex-end; }
.section-title { margin: var(--adc-space-4) 0 var(--adc-space-2); font-weight: 600; }
.muted { color: var(--adc-text-secondary); }
</style>
