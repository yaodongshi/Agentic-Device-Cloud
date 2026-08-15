<template>
  <el-card>
    <el-tabs
      v-model="filters.status"
      @tab-change="onTabChange"
    >
      <el-tab-pane
        :label="t('approvals.tabs.pending')"
        name="pending"
      />
      <el-tab-pane
        :label="t('approvals.tabs.all')"
        name=""
      />
      <el-tab-pane
        :label="t('approvals.tabs.approved')"
        name="approved"
      />
      <el-tab-pane
        :label="t('approvals.tabs.rejected')"
        name="rejected"
      />
      <el-tab-pane
        :label="t('approvals.tabs.expired')"
        name="expired"
      />
    </el-tabs>

    <div class="filters">
      <el-input
        v-model="filters.deviceCode"
        class="filter-item"
        :placeholder="t('approvals.devicePlaceholder')"
        clearable
        @keyup.enter="search"
      />
      <el-input
        v-model="filters.toolName"
        class="filter-item"
        :placeholder="t('approvals.toolPlaceholder')"
        clearable
        @keyup.enter="search"
      />
      <el-date-picker
        v-model="timeRange"
        class="filter-item time"
        type="datetimerange"
        :start-placeholder="t('approvals.timeFrom')"
        :end-placeholder="t('approvals.timeTo')"
        value-format="YYYY-MM-DDTHH:mm:ss[Z]"
      />
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

    <el-table
      v-loading="loading"
      :data="list"
    >
      <el-table-column
        prop="ticket_id"
        :label="t('approvals.ticketId')"
        min-width="150"
      >
        <template #default="{ row }">
          <span class="mono">{{ row.ticket_id }}</span>
        </template>
      </el-table-column>
      <el-table-column
        prop="device_code"
        :label="t('approvals.device')"
        min-width="140"
      />
      <el-table-column
        prop="tool_name"
        :label="t('approvals.tool')"
        min-width="150"
      />
      <el-table-column
        :label="t('approvals.risk')"
        width="120"
      >
        <template #default="{ row }">
          <RiskTag :level="row.risk_level" />
        </template>
      </el-table-column>
      <el-table-column
        :label="t('approvals.status')"
        width="110"
      >
        <template #default="{ row }">
          <el-tag
            :type="statusTagType(row.status)"
            effect="light"
          >
            {{ t(`approvals.statuses.${row.status}`) }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column
        :label="t('approvals.remaining')"
        width="120"
      >
        <template #default="{ row }">
          <span
            v-if="row.status === 'pending'"
            :class="{ urgent: remainingSeconds(row) < 60 }"
          >{{ countdown(row) }}</span>
          <span
            v-else
            class="muted"
          >—</span>
        </template>
      </el-table-column>
      <el-table-column
        prop="approver"
        :label="t('approvals.approver')"
        width="130"
      >
        <template #default="{ row }">
          {{ row.approver ?? '—' }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('approvals.createdAt')"
        width="170"
      >
        <template #default="{ row }">
          {{ formatTime(row.created_at) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('common.actions')"
        width="110"
        fixed="right"
      >
        <template #default="{ row }">
          <el-button
            link
            :type="row.status === 'pending' ? 'primary' : 'info'"
            @click="openDetail(row)"
          >
            {{ row.status === 'pending' ? t('approvals.handle') : t('approvals.detail') }}
          </el-button>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty :description="t('approvals.empty')" />
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

    <!-- Ticket detail drawer: full context plus the desktop decision
         panel (design/20 4.7). -->
    <el-drawer
      v-model="drawerVisible"
      :title="current ? `${t('approvals.ticketId')} ${current.ticket_id}` : ''"
      size="480px"
    >
      <template v-if="current">
        <el-descriptions
          :column="1"
          border
        >
          <el-descriptions-item :label="t('approvals.device')">
            <span class="mono">{{ current.device_code }}</span>
          </el-descriptions-item>
          <el-descriptions-item :label="t('approvals.tool')">
            <span class="mono">{{ current.tool_name }}</span>
          </el-descriptions-item>
          <el-descriptions-item :label="t('approvals.risk')">
            <RiskTag :level="current.risk_level" />
          </el-descriptions-item>
          <el-descriptions-item :label="t('approvals.createdAt')">
            {{ formatTime(current.created_at) }}
          </el-descriptions-item>
          <el-descriptions-item :label="t('approvals.expireAt')">
            {{ formatTime(current.expire_at) }}
          </el-descriptions-item>
          <el-descriptions-item
            v-if="current.approver"
            :label="t('approvals.approver')"
          >
            {{ current.approver }}
          </el-descriptions-item>
          <el-descriptions-item
            v-if="current.comment"
            :label="t('approvals.comment')"
          >
            {{ current.comment }}
          </el-descriptions-item>
          <el-descriptions-item
            v-if="current.resolved_at"
            :label="t('approvals.resolvedAt')"
          >
            {{ formatTime(current.resolved_at) }}
          </el-descriptions-item>
        </el-descriptions>

        <div class="args-title">
          {{ t('approvals.arguments') }}
        </div>
        <pre class="mono args">{{ prettyJson(current.arguments) }}</pre>

        <div
          v-if="current.status === 'pending'"
          class="decision"
        >
          <div class="remaining-line">
            {{ t('approvals.remaining') }}:
            <span :class="{ urgent: remainingSeconds(current) < 60 }">{{ countdown(current) }}</span>
          </div>
          <el-input
            v-model="decisionComment"
            type="textarea"
            :rows="2"
            maxlength="200"
            :placeholder="t('approvals.commentPlaceholder')"
          />
          <div class="decision-buttons">
            <el-button
              type="success"
              :loading="deciding"
              @click="confirmDecision('approve')"
            >
              {{ t('approvals.approve') }}
            </el-button>
            <el-button
              type="danger"
              :loading="deciding"
              @click="confirmDecision('reject')"
            >
              {{ t('approvals.reject') }}
            </el-button>
          </div>
        </div>
        <el-alert
          v-else
          type="info"
          :closable="false"
          show-icon
          :title="t('approvals.processedHint')"
        />
      </template>
    </el-drawer>
  </el-card>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '@/api/request'
import { errorMessage } from '@/api/errors'
import { formatTime } from '@/utils/format'
import { useAuthStore } from '@/stores/auth'
import RiskTag from '@/components/RiskTag.vue'
import type { ApprovalTicket, HitlCallbackResponse, HitlDecision, Page } from '@/api/types'

const { t } = useI18n()
const auth = useAuthStore()

const loading = ref(false)
const list = ref<ApprovalTicket[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const timeRange = ref<[string, string] | null>(null)

const filters = reactive({ status: 'pending', deviceCode: '', toolName: '' })

// GET /v1/admin/approval-tickets (design/33 3.1.15), offset pagination.
async function fetchList() {
  loading.value = true
  try {
    const res = await api.get<Page<ApprovalTicket>>('/v1/admin/approval-tickets', {
      page: page.value,
      page_size: pageSize.value,
      status: filters.status || undefined,
      device_code: filters.deviceCode || undefined,
      tool_name: filters.toolName || undefined,
      time_from: timeRange.value?.[0] || undefined,
      time_to: timeRange.value?.[1] || undefined,
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
  filters.deviceCode = ''
  filters.toolName = ''
  timeRange.value = null
  search()
}

function onTabChange() {
  search()
}

function onSizeChange() {
  page.value = 1
  fetchList()
}

function statusTagType(status: string): 'success' | 'warning' | 'danger' | 'info' {
  switch (status) {
    case 'approved': return 'success'
    case 'pending': return 'warning'
    case 'rejected': return 'danger'
    default: return 'info'
  }
}

// Remaining-time countdown for pending tickets, refreshed every second and
// rendered red under one minute (design/20 4.6).
const now = ref(Date.now())
let timer: ReturnType<typeof setInterval> | undefined

function remainingSeconds(ticket: ApprovalTicket): number {
  const left = new Date(ticket.expire_at).getTime() - now.value
  return Math.max(0, Math.floor(left / 1000))
}

function countdown(ticket: ApprovalTicket): string {
  const s = remainingSeconds(ticket)
  const mm = String(Math.floor(s / 60)).padStart(2, '0')
  const ss = String(s % 60).padStart(2, '0')
  return `${mm}:${ss}`
}

// Detail drawer
const drawerVisible = ref(false)
const current = ref<ApprovalTicket | null>(null)
const decisionComment = ref('')
const deciding = ref(false)

function openDetail(row: ApprovalTicket) {
  current.value = row
  decisionComment.value = ''
  drawerVisible.value = true
}

function prettyJson(args: Record<string, unknown> | undefined | null): string {
  if (!args) return '{}'
  try {
    return JSON.stringify(args, null, 2)
  } catch {
    return String(args)
  }
}

// Decision flow. design/33 defines a single decision endpoint,
// POST /v1/hitl/callback (#23), guarded by HMAC headers produced with the
// ticket-level callback secret (design/33 1.4, SEC-01/13). The console
// cannot mint that signature — the secret only travels inside IM card
// URLs. Production flow: GET /v1/hitl/action (signed URL, design/33
// 3.4.2) redirects to the console confirm page and the server performs
// the signed callback. V1.0 CE calls the callback endpoint directly and
// relies on the gateway session + RBAC + server-side approver resolution;
// replace with the signed flow before production. The real subject
// (SEC-01) is the session user, never a hardcoded name.
async function confirmDecision(decision: HitlDecision) {
  if (!current.value) return
  const reason = decisionComment.value.trim()
  if (decision === 'reject' && !reason) {
    ElMessage.warning(t('approvals.rejectReasonRequired'))
    return
  }
  const box = await ElMessageBox.confirm(
    decision === 'approve' ? t('approvals.confirmApprove') : t('approvals.confirmReject'),
    decision === 'approve' ? t('approvals.approve') : t('approvals.reject'),
    {
      type: 'warning',
      confirmButtonText: decision === 'approve' ? t('approvals.confirmApproveButton') : t('approvals.confirmRejectButton'),
      cancelButtonText: t('common.cancel'),
    },
  ).catch(() => false)
  if (!box) return
  deciding.value = true
  try {
    const res = await api.post<HitlCallbackResponse>('/v1/hitl/callback', {
      ticket_id: current.value.ticket_id,
      decision,
      approver: auth.user?.userId ?? '',
      comment: reason || undefined,
    })
    ElMessage.success(t('approvals.decisionSuccess'))
    drawerVisible.value = false
    current.value = { ...current.value, status: res.status }
    await fetchList()
  } catch (err) {
    // 12002: someone else processed the ticket first (design/33 3.4.1) —
    // refresh so the list reflects the latest state.
    ElMessage.error(errorMessage(err, t))
    await fetchList()
  } finally {
    deciding.value = false
  }
}

onMounted(() => {
  fetchList()
  timer = setInterval(() => {
    now.value = Date.now()
  }, 1000)
})

onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
})
</script>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: var(--adc-space-2); margin-bottom: var(--adc-space-4); }
.filter-item { width: 200px; }
.filter-item.time { width: 360px; }
.pagination { margin-top: var(--adc-space-4); justify-content: flex-end; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.muted { color: var(--adc-text-secondary); }
.urgent { color: var(--adc-risk-3); font-weight: 600; }
.args-title { margin: var(--adc-space-4) 0 var(--adc-space-2); font-weight: 600; }
.args {
  margin: 0;
  padding: var(--adc-space-3);
  background: var(--adc-bg);
  border: 1px solid var(--adc-border);
  border-radius: var(--adc-radius);
  font-size: 12px;
  line-height: 1.6;
  max-height: 240px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
.decision { margin-top: var(--adc-space-4); }
.remaining-line { margin-bottom: var(--adc-space-3); font-weight: 600; }
.decision-buttons { display: flex; gap: var(--adc-space-2); margin-top: var(--adc-space-3); }
.decision-buttons .el-button { flex: 1; }
</style>
