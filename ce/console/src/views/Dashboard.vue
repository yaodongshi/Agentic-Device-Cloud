<template>
  <div class="dashboard">
    <div class="toolbar">
      <el-button
        :loading="loading"
        @click="loadAll"
      >
        {{ t('dashboard.refresh') }}
      </el-button>
    </div>

    <!-- First-login onboarding banner (design/20 4.2 new-tenant guidance):
         dismissed once per browser via localStorage. -->
    <el-card
      v-if="showOnboarding"
      class="onboarding"
    >
      <div class="onboarding-head">
        <span class="onboarding-title">{{ t('dashboard.onboarding.title') }}</span>
        <el-button
          link
          type="primary"
          @click="openGuide"
        >
          {{ t('dashboard.onboarding.guide') }}
        </el-button>
      </div>
      <p class="onboarding-intro">
        {{ t('dashboard.onboarding.intro') }}
      </p>
      <el-row :gutter="16">
        <el-col
          v-for="(step, i) in onboardingSteps"
          :key="step.title"
          :xs="24"
          :md="8"
          class="step-col"
        >
          <div class="step">
            <span class="step-index">{{ i + 1 }}</span>
            <div>
              <div class="step-title">
                {{ t(step.title) }}
              </div>
              <div class="step-desc">
                {{ t(step.desc) }}
              </div>
            </div>
          </div>
        </el-col>
      </el-row>
      <div class="onboarding-foot">
        <el-button
          type="primary"
          @click="dismissOnboarding"
        >
          {{ t('dashboard.onboarding.gotIt') }}
        </el-button>
      </div>
    </el-card>

    <!-- Overview cards: online devices / today's agent calls / pending
         approvals / HITL blocked (design/20 4.2). A card links to its
         module when the current role has access. -->
    <el-row :gutter="16">
      <el-col
        v-for="card in statCards"
        :key="card.key"
        :xs="24"
        :sm="12"
        :md="6"
        class="stat-col"
      >
        <el-card
          shadow="hover"
          :class="{ stat: true, clickable: !!card.to }"
          @click="card.to && go(card.to)"
        >
          <div class="stat-label">
            {{ t(card.label) }}
          </div>
          <div class="stat-value">
            {{ card.value }}
          </div>
          <div
            v-if="card.hint"
            class="stat-hint"
          >
            {{ t(card.hint) }}
          </div>
        </el-card>
      </el-col>
    </el-row>

    <el-card class="section">
      <div class="section-head">
        {{ t('dashboard.quickActions') }}
      </div>
      <div class="quick-actions">
        <el-button
          v-for="action in quickActions"
          :key="action.path"
          @click="go(action.path)"
        >
          {{ t(action.label) }}
        </el-button>
      </div>
    </el-card>

    <el-card
      v-if="showAudit"
      class="section"
    >
      <div class="section-head">
        <span>{{ t('dashboard.recentAudit') }}</span>
        <span class="head-right">
          <span class="muted hint">
            {{ t('dashboard.recentAuditHint') }}
          </span>
          <el-button
            link
            type="primary"
            @click="go('/audit')"
          >
            {{ t('dashboard.viewAll') }}
          </el-button>
        </span>
      </div>
      <el-table
        v-loading="auditLoading"
        :data="recentAudit"
        size="small"
      >
        <el-table-column
          :label="t('audit.time')"
          width="170"
        >
          <template #default="{ row }">
            {{ formatTime(row.created_at) }}
          </template>
        </el-table-column>
        <el-table-column
          prop="device_code"
          :label="t('audit.device')"
          min-width="140"
        >
          <template #default="{ row }">
            {{ row.device_code ?? '—' }}
          </template>
        </el-table-column>
        <el-table-column
          prop="tool_name"
          :label="t('audit.tool')"
          min-width="150"
        >
          <template #default="{ row }">
            {{ row.tool_name ?? '—' }}
          </template>
        </el-table-column>
        <el-table-column
          :label="t('audit.statusLabel')"
          width="130"
        >
          <template #default="{ row }">
            <el-tag
              :type="auditStatusTag(row.status)"
              effect="light"
            >
              {{ t(`audit.statuses.${row.status}`) }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column
          :label="t('audit.traceId')"
          min-width="200"
        >
          <template #default="{ row }">
            <span class="mono">{{ row.trace_id }}</span>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty
            :description="t('dashboard.auditEmpty')"
            :image-size="70"
          />
        </template>
      </el-table>
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { api } from '@/api/request'
import { useAuthStore } from '@/stores/auth'
import { buildMonitorSnapshot, parseMetrics } from '@/utils/metrics'
import { formatCompact, formatTime } from '@/utils/format'
import { DOCS_URL } from '@/utils/links'
import type { ApprovalTicket, AuditLog, AuditLogPage, AuditLogStatus, Device, Page, Role } from '@/api/types'

const { t } = useI18n()
const router = useRouter()
const auth = useAuthStore()

// Module access mirrors the menu matrix (MainLayout / design/20 2.2); the
// backend still enforces access server-side.
const AUDIT_ROLES: Role[] = ['platform_admin', 'tenant_admin', 'auditor']
const APPROVAL_ROLES: Role[] = ['platform_admin', 'tenant_admin', 'approver']
const APIKEY_ROLES: Role[] = ['platform_admin', 'tenant_admin']
const MONITOR_ROLES: Role[] = ['platform_admin', 'tenant_admin', 'auditor']

function can(roles: Role[]): boolean {
  return !!auth.user && roles.includes(auth.user.role)
}

// First-login onboarding banner: shown until dismissed via localStorage.
const ONBOARDING_KEY = 'adc_onboarding_dismissed'
const showOnboarding = ref(localStorage.getItem(ONBOARDING_KEY) !== '1')

const onboardingSteps = [
  { title: 'dashboard.onboarding.step1', desc: 'dashboard.onboarding.step1Desc' },
  { title: 'dashboard.onboarding.step2', desc: 'dashboard.onboarding.step2Desc' },
  { title: 'dashboard.onboarding.step3', desc: 'dashboard.onboarding.step3Desc' },
]

function dismissOnboarding() {
  localStorage.setItem(ONBOARDING_KEY, '1')
  showOnboarding.value = false
}

// The docs site ships with FR-020; until then the repo README is the
// integration guide entry point.
function openGuide() {
  window.open(DOCS_URL, '_blank', 'noopener')
}

// Overview numbers. null means "failed to load" and renders '—'.
const onlineDevices = ref<number | null>(null)
const pendingTickets = ref<number | null>(null)
const agentCallsToday = ref<number | null>(null)
const agentCallsCumulative = ref(false)
const hitlBlocked = ref<number | null>(null)

interface StatCard {
  key: string
  label: string
  value: string
  hint?: string
  to?: string
}

const statCards = computed<StatCard[]>(() => [
  {
    key: 'online',
    label: 'dashboard.onlineDevices',
    value: formatCompact(onlineDevices.value),
    to: '/devices',
  },
  {
    key: 'calls',
    label: 'dashboard.agentCallsToday',
    value: formatCompact(agentCallsToday.value),
    hint: agentCallsCumulative.value ? 'dashboard.cumulativeHint' : undefined,
    to: can(MONITOR_ROLES) ? '/monitor' : undefined,
  },
  {
    key: 'pending',
    label: 'dashboard.pendingTickets',
    value: formatCompact(pendingTickets.value),
    hint: can(APPROVAL_ROLES) ? 'dashboard.pendingHint' : undefined,
    to: can(APPROVAL_ROLES) ? '/approvals' : undefined,
  },
  {
    key: 'blocked',
    label: 'dashboard.hitlBlocked',
    value: formatCompact(hitlBlocked.value),
  },
])

// GET /v1/admin/devices?status=online (design/33 3.1.7); only the total is
// needed, so page_size=1 keeps the count call cheap.
async function loadOnlineDevices() {
  try {
    const res = await api.get<Page<Device>>('/v1/admin/devices', {
      status: 'online',
      page: 1,
      page_size: 1,
    })
    onlineDevices.value = res.total ?? 0
  } catch {
    onlineDevices.value = null
  }
}

// Pending approval tickets: the same endpoint the approval center uses
// (design/33 3.1.15), counted via total with page_size=1.
async function loadPendingTickets() {
  try {
    const res = await api.get<Page<ApprovalTicket>>('/v1/admin/approval-tickets', {
      status: 'pending',
      page: 1,
      page_size: 1,
    })
    pendingTickets.value = res.total ?? 0
  } catch {
    pendingTickets.value = null
  }
}

// /metrics is unauthenticated plain text (design/33 1.2 endpoint 27), so it
// bypasses the JSON request wrapper like fetchMetricsSnapshot does.
// parseMetrics is reused directly here so day-windowed agent-call samples
// are preferred once the gateway ships them (design/60 6.1 only defines
// cumulative counters today; until then the card falls back to the
// cumulative total and shows the cumulative hint).
async function loadMetrics() {
  try {
    const res = await fetch('/metrics', { headers: { Accept: 'text/plain' } })
    if (!res.ok) throw new Error(`metrics request failed: HTTP ${res.status}`)
    const metrics = parseMetrics(await res.text())
    const snapshot = buildMonitorSnapshot(metrics)
    hitlBlocked.value = snapshot.hitlBlockedTotal
    const family = metrics.get('adc_agent_calls_total') ?? metrics.get('adc_tool_call_total')
    const daySamples = family?.samples.filter((s) => s.labels.window === 'day')
    if (daySamples?.length) {
      agentCallsToday.value = daySamples.reduce((acc, s) => acc + (Number.isFinite(s.value) ? s.value : 0), 0)
      agentCallsCumulative.value = false
    } else {
      agentCallsToday.value = snapshot.agentCallsTotal
      agentCallsCumulative.value = true
    }
  } catch {
    agentCallsToday.value = null
    hitlBlocked.value = null
  }
}

// Recent audit events: mandatory 24h default range (design/20 4.8) and the
// first 5 rows of the newest page (design/33 3.1.16 cursor pagination).
const auditLoading = ref(false)
const recentAudit = ref<AuditLog[]>([])

const showAudit = computed(() => can(AUDIT_ROLES))

async function loadRecentAudit() {
  if (!showAudit.value) return
  auditLoading.value = true
  try {
    const end = new Date()
    const start = new Date(end.getTime() - 24 * 60 * 60 * 1000)
    const res = await api.get<AuditLogPage>('/v1/admin/audit-logs', {
      limit: 5,
      time_from: start.toISOString(),
      time_to: end.toISOString(),
    })
    recentAudit.value = (res.items ?? []).slice(0, 5)
  } catch {
    recentAudit.value = []
  } finally {
    auditLoading.value = false
  }
}

function auditStatusTag(status: AuditLogStatus): 'success' | 'warning' | 'danger' | 'info' {
  switch (status) {
    case 'success': return 'success'
    case 'blocked_by_hitl': return 'warning'
    case 'failed': return 'danger'
    default: return 'info'
  }
}

// Quick actions honor the same role matrix as the menus.
const quickActions = computed(() => {
  const actions: Array<{ path: string; label: string }> = [
    { path: '/devices', label: 'dashboard.registerDevice' },
  ]
  if (can(APIKEY_ROLES)) actions.push({ path: '/api-keys', label: 'dashboard.createApiKey' })
  if (can(AUDIT_ROLES)) actions.push({ path: '/audit', label: 'dashboard.viewAudit' })
  return actions
})

function go(path: string) {
  router.push(path)
}

const loading = ref(false)

async function loadAll() {
  loading.value = true
  try {
    await Promise.allSettled([loadOnlineDevices(), loadPendingTickets(), loadMetrics(), loadRecentAudit()])
  } finally {
    loading.value = false
  }
}

onMounted(loadAll)
</script>

<style scoped>
.toolbar { display: flex; justify-content: flex-end; }
.onboarding { margin-bottom: var(--adc-space-4); border-color: var(--adc-brand-light); }
.onboarding-head { display: flex; align-items: center; justify-content: space-between; }
.onboarding-title { font-weight: 600; }
.onboarding-intro { margin: var(--adc-space-2) 0 var(--adc-space-4); color: var(--adc-text-secondary); }
.step-col { margin-bottom: var(--adc-space-3); }
.step { display: flex; gap: var(--adc-space-3); align-items: flex-start; }
.step-index {
  width: 22px;
  height: 22px;
  border-radius: 50%;
  background: var(--adc-brand);
  color: #fff;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 12px;
  flex-shrink: 0;
}
.step-title { font-weight: 600; }
.step-desc { margin-top: var(--adc-space-1); font-size: 12px; color: var(--adc-text-secondary); }
.onboarding-foot { margin-top: var(--adc-space-4); }
.stat-col { margin-bottom: var(--adc-space-4); }
.stat { cursor: default; }
.stat.clickable { cursor: pointer; }
.stat-label { font-size: 13px; color: var(--adc-text-secondary); }
.stat-value {
  margin: var(--adc-space-1) 0;
  font-size: 26px;
  font-weight: 700;
  color: var(--adc-brand);
  font-variant-numeric: tabular-nums;
}
.stat-hint { font-size: 12px; color: var(--adc-text-secondary); }
.section { margin-bottom: var(--adc-space-4); }
.section-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--adc-space-3);
  font-weight: 600;
}
.head-right { display: inline-flex; align-items: center; gap: var(--adc-space-2); }
.hint { font-size: 12px; font-weight: 400; }
.quick-actions { display: flex; gap: var(--adc-space-2); flex-wrap: wrap; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.muted { color: var(--adc-text-secondary); }
</style>
