<template>
  <el-card>
    <div class="toolbar">
      <div class="left">
        <el-select
          v-model="range"
          class="range-select"
        >
          <el-option
            v-for="(label, key) in tm('monitor.ranges')"
            :key="key"
            :label="label"
            :value="key"
          />
        </el-select>
        <el-switch
          v-model="autoRefresh"
          :active-text="t('monitor.autoRefresh')"
        />
        <el-button
          :loading="loading"
          @click="refreshNow"
        >
          {{ t('monitor.refresh') }}
        </el-button>
        <span class="muted">{{ lastUpdatedText }}</span>
      </div>
    </div>

    <!-- Data-interruption banner per design/20 4.12: keep the last data on
         screen and surface the stale state with the last fetch time. -->
    <el-alert
      v-if="error"
      class="stale"
      type="warning"
      :title="hasHistory ? t('monitor.staleHint') : t('monitor.loadFailed')"
      show-icon
      :closable="false"
    />

    <el-row :gutter="16">
      <el-col
        :xs="24"
        :md="12"
      >
        <div class="panel">
          <div class="panel-head">
            <span class="panel-title">{{ t('monitor.deviceOnline') }}</span>
            <span class="big-number">{{ formatCompact(snapshot.deviceOnlineTotal) }}</span>
          </div>
          <div
            ref="onlineEl"
            class="chart"
          />
        </div>
      </el-col>
      <el-col
        :xs="24"
        :md="12"
      >
        <div class="panel">
          <div class="panel-head">
            <span class="panel-title">{{ t('monitor.agentCalls') }}</span>
            <span class="big-number">{{ formatCompact(snapshot.agentCallsTotal) }}</span>
          </div>
          <div
            ref="callsEl"
            class="chart"
          />
        </div>
      </el-col>
      <el-col
        :xs="24"
        :md="12"
      >
        <div class="panel">
          <div class="panel-head">
            <span class="panel-title">{{ t('monitor.hitl') }}</span>
            <span
              v-if="snapshot.hitlBlockedTotal > 0"
              class="big-number blocked"
            >
              {{ t('monitor.hitlBlocked') }} {{ formatCompact(snapshot.hitlBlockedTotal) }}
            </span>
          </div>
          <div
            ref="hitlEl"
            class="chart"
          />
        </div>
      </el-col>
      <el-col
        :xs="24"
        :md="12"
      >
        <div class="panel">
          <div class="panel-head">
            <span class="panel-title">{{ t('monitor.approvalP95') }}</span>
            <span class="big-number">{{ p95Text }}</span>
          </div>
          <div
            ref="p95El"
            class="chart"
          />
        </div>
      </el-col>
    </el-row>

    <!-- FR-017 threshold alert rules (design/82 B2): tenant-scoped editor,
         PUT full-replace semantics, change_reason goes to the audit log.
         Admin roles only; other roles get a hint instead of dead controls. -->
    <el-card
      v-if="canEditAlerts"
      class="alerts"
    >
      <template #header>
        <div class="alerts-head">
          <span>{{ t('monitor.alertsRulesTitle') }}</span>
          <span class="muted">{{ t('monitor.alertsAggregationHint') }}</span>
        </div>
      </template>
      <el-alert
        v-if="rulesError"
        class="stale"
        type="warning"
        :title="t('monitor.alertsRulesLoadFailed')"
        show-icon
        :closable="false"
      />
      <el-table
        v-loading="rulesLoading"
        :data="ruleDrafts"
        size="small"
      >
        <el-table-column
          :label="t('monitor.alertsRuleName')"
          min-width="180"
        >
          <template #default="{ row }">
            <el-input
              v-model="row.name"
              size="small"
              :placeholder="t('monitor.alertsRuleNamePlaceholder')"
            />
          </template>
        </el-table-column>
        <el-table-column
          :label="t('monitor.alertsMetric')"
          min-width="240"
        >
          <template #default="{ row }">
            <el-select
              v-model="row.metric"
              size="small"
            >
              <el-option
                v-for="opt in ALERT_METRIC_OPTIONS"
                :key="opt.name"
                :label="opt.name"
                :value="opt.name"
              />
            </el-select>
          </template>
        </el-table-column>
        <el-table-column
          :label="t('monitor.alertsOperator')"
          width="90"
        >
          <template #default="{ row }">
            <el-select
              v-model="row.operator"
              size="small"
            >
              <el-option
                v-for="op in ALERT_OPERATORS"
                :key="op"
                :label="op"
                :value="op"
              />
            </el-select>
          </template>
        </el-table-column>
        <el-table-column
          :label="t('monitor.alertsThreshold')"
          width="140"
        >
          <template #default="{ row }">
            <el-input-number
              v-model="row.threshold"
              size="small"
              :min="0"
              :controls="false"
            />
          </template>
        </el-table-column>
        <el-table-column
          :label="t('monitor.alertsDuration')"
          width="130"
        >
          <template #default="{ row }">
            <el-input-number
              v-model="row.durationSec"
              size="small"
              :min="0"
              :max="3600"
              :step="30"
            />
          </template>
        </el-table-column>
        <el-table-column
          :label="t('monitor.alertsSeverity')"
          width="90"
        >
          <template #default="{ row }">
            <el-select
              v-model="row.severity"
              size="small"
            >
              <el-option
                v-for="sev in ALERT_SEVERITIES"
                :key="sev"
                :label="sev"
                :value="sev"
              />
            </el-select>
          </template>
        </el-table-column>
        <el-table-column
          :label="t('monitor.alertsEnabled')"
          width="80"
        >
          <template #default="{ row }">
            <el-switch v-model="row.enabled" />
          </template>
        </el-table-column>
        <el-table-column
          :label="t('common.actions')"
          width="80"
        >
          <template #default="{ $index }">
            <el-button
              size="small"
              text
              type="danger"
              @click="removeRule($index)"
            >
              {{ t('monitor.alertsDelete') }}
            </el-button>
          </template>
        </el-table-column>
        <template #empty>
          {{ t('monitor.alertsRulesEmpty') }}
        </template>
      </el-table>
      <div class="alerts-toolbar">
        <el-button
          size="small"
          @click="addRule"
        >
          {{ t('monitor.alertsAddRule') }}
        </el-button>
        <el-input
          v-model="saveReason"
          size="small"
          class="reason"
          :placeholder="t('common.reasonPlaceholder')"
        />
        <el-button
          type="primary"
          size="small"
          :loading="rulesSaving"
          @click="saveRules"
        >
          {{ t('common.save') }}
        </el-button>
      </div>
    </el-card>

    <!-- FR-017 fired-alert history: newest first, aggregated (one row per
         dedup window), severity tags P1-P3. -->
    <el-card
      v-if="canEditAlerts"
      class="alerts"
    >
      <template #header>
        <div class="alerts-head">
          <span>{{ t('monitor.alertsEventsTitle') }}</span>
          <el-button
            size="small"
            text
            :loading="eventsLoading"
            @click="loadAlertData"
          >
            {{ t('monitor.refresh') }}
          </el-button>
        </div>
      </template>
      <el-alert
        v-if="eventsError"
        class="stale"
        type="warning"
        :title="t('monitor.alertsEventsLoadFailed')"
        show-icon
        :closable="false"
      />
      <el-table
        v-loading="eventsLoading"
        :data="alertEvents"
        size="small"
      >
        <el-table-column
          :label="t('monitor.alertsSeverity')"
          width="90"
        >
          <template #default="{ row }">
            <el-tag
              size="small"
              :type="severityTagType(row.severity)"
            >
              {{ row.severity }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column
          prop="rule_name"
          :label="t('monitor.alertsRuleName')"
          min-width="160"
        />
        <el-table-column
          prop="metric"
          :label="t('monitor.alertsMetric')"
          min-width="220"
        />
        <el-table-column
          :label="t('monitor.alertsObserved')"
          min-width="160"
        >
          <template #default="{ row }">
            {{ formatNumber(row.observed) }} {{ row.operator }} {{ formatNumber(row.threshold) }}
          </template>
        </el-table-column>
        <el-table-column
          :label="t('monitor.alertsFiredAt')"
          min-width="170"
        >
          <template #default="{ row }">
            {{ new Date(row.fired_at).toLocaleString() }}
          </template>
        </el-table-column>
        <template #empty>
          {{ t('monitor.alertsEventsEmpty') }}
        </template>
      </el-table>
    </el-card>
    <el-alert
      v-else
      class="stale"
      type="info"
      :title="t('monitor.alertsAdminOnly')"
      show-icon
      :closable="false"
    />
  </el-card>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage } from 'element-plus'
import * as echarts from 'echarts/core'
import { BarChart, LineChart, PieChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import { ALL_TENANTS_LABEL, fetchMetricsSnapshot } from '@/utils/metrics'
import type { MonitorSnapshot } from '@/utils/metrics'
import { formatCompact } from '@/utils/format'
import { useAuthStore } from '@/stores/auth'
import {
  ALERT_METRIC_OPTIONS,
  fetchAlertEvents,
  fetchAlertRules,
  saveAlertRules,
} from '@/api/alerts'
import type { AlertEvent, AlertOperator, AlertSeverity } from '@/api/alerts'

// ECharts over hand-rolled SVG: four different chart types (line/bar/donut/
// line with a dashed threshold markLine) plus tooltips, legends, window
// resize and 30s refresh come for free, and the tree-shaken echarts/core
// bundle stays small. Hand-written SVG would duplicate a chart library
// badly for zero benefit (design/20 6.6 also assumes a chart library).
echarts.use([LineChart, BarChart, PieChart, GridComponent, TooltipComponent, LegendComponent, CanvasRenderer])

const { t, tm } = useI18n()
const auth = useAuthStore()

// Auto refresh interval per design/20 4.12; the P95 threshold line per
// design/20 6.6 (500ms dashed red).
const REFRESH_INTERVAL_MS = 30000
const P95_THRESHOLD_MS = 500
const CALLS_BAR_MAX_TENANTS = 8
const ALERT_EVENTS_LIMIT = 50

// Alert rule editor constants (mirror the backend whitelist/validators in
// internal/alerts, design/82 B2).
const ALERT_OPERATORS: AlertOperator[] = ['>', '<', '>=']
const ALERT_SEVERITIES: AlertSeverity[] = ['P1', 'P2', 'P3']

// The history is a rolling in-memory window: /metrics is instantaneous
// (Prometheus snapshot), so the selected range only controls how many
// refresh points are kept (30s apart).
const HISTORY_CAPACITY: Record<string, number> = { '1h': 120, '24h': 240, '7d': 336 }

interface HistoryPoint {
  time: string
  value: number
}

// Editable draft of one rule row: ids round-trip through GET/PUT so the
// backend keeps dedup state stable; duration uses seconds on the wire.
interface RuleDraft {
  id: string
  name: string
  metric: string
  operator: AlertOperator
  threshold: number
  durationSec: number
  severity: AlertSeverity
  enabled: boolean
}

const range = ref('24h')
const autoRefresh = ref(true)
const loading = ref(false)
const error = ref(false)
const lastUpdated = ref<Date | null>(null)

// FR-017 alert state: rules are only editable by admin-tier roles
// (platform_admin / tenant_admin); the RBAC matrix denies the endpoints
// for auditors, so the sections hide instead of showing 403 noise.
const canEditAlerts = computed(
  () => auth.user?.role === 'platform_admin' || auth.user?.role === 'tenant_admin',
)
const ruleDrafts = ref<RuleDraft[]>([])
const rulesLoading = ref(false)
const rulesSaving = ref(false)
const rulesError = ref(false)
const saveReason = ref('')
const alertEvents = ref<AlertEvent[]>([])
const eventsLoading = ref(false)
const eventsError = ref(false)

const snapshot = reactive(emptySnapshot())
const onlineHistory = ref<HistoryPoint[]>([])
const p95History = ref<HistoryPoint[]>([])

const onlineEl = ref<HTMLDivElement>()
const callsEl = ref<HTMLDivElement>()
const hitlEl = ref<HTMLDivElement>()
const p95El = ref<HTMLDivElement>()

let onlineChart: echarts.ECharts | undefined
let callsChart: echarts.ECharts | undefined
let hitlChart: echarts.ECharts | undefined
let p95Chart: echarts.ECharts | undefined
let timer: ReturnType<typeof setInterval> | undefined

function emptySnapshot(): MonitorSnapshot {
  return {
    deviceOnlineTotal: 0,
    deviceOnlineByTenant: {},
    agentCallsTotal: 0,
    agentCallsByTenant: {},
    hitlByState: {},
    hitlBlockedTotal: 0,
    approvalP95Seconds: null,
    approvalTotal: 0,
  }
}

const hasHistory = computed(() => onlineHistory.value.length > 0)

const lastUpdatedText = computed(() =>
  lastUpdated.value
    ? t('monitor.lastUpdated', { time: lastUpdated.value.toLocaleTimeString(undefined, { hour12: false }) })
    : '—',
)

const p95Text = computed(() => {
  const p95 = snapshot.approvalP95Seconds
  return p95 === null ? '—' : `${Math.round(p95 * 1000)} ms`
})

async function refreshNow() {
  if (loading.value) return
  loading.value = true
  try {
    const snap = await fetchMetricsSnapshot()
    Object.assign(snapshot, snap)
    error.value = false
    lastUpdated.value = new Date()
    pushHistory(snap)
    renderCharts()
  } catch {
    // Keep rendering the last data and surface the stale banner
    // (design/20 4.12 data-interruption state).
    error.value = true
  } finally {
    loading.value = false
  }
  if (canEditAlerts.value) await loadAlertData()
}

// FR-017 alert data load: rules and fired events share the refresh cycle.
function loadAlertData() {
  return Promise.all([loadRules(), loadEvents()])
}

async function loadRules() {
  rulesLoading.value = true
  try {
    const rules = await fetchAlertRules()
    ruleDrafts.value = rules.map((r) => ({
      id: r.id,
      name: r.name,
      metric: r.metric,
      operator: r.operator,
      threshold: r.threshold,
      durationSec: r.duration_sec,
      severity: r.severity,
      enabled: r.enabled,
    }))
    rulesError.value = false
  } catch {
    rulesError.value = true
  } finally {
    rulesLoading.value = false
  }
}

async function loadEvents() {
  eventsLoading.value = true
  try {
    alertEvents.value = await fetchAlertEvents(ALERT_EVENTS_LIMIT)
    eventsError.value = false
  } catch {
    eventsError.value = true
  } finally {
    eventsLoading.value = false
  }
}

function addRule() {
  ruleDrafts.value.push({
    id: '',
    name: '',
    metric: ALERT_METRIC_OPTIONS[0]?.name ?? '',
    operator: '>',
    threshold: 0,
    durationSec: 0,
    severity: 'P2',
    enabled: true,
  })
}

function removeRule(index: number) {
  ruleDrafts.value.splice(index, 1)
}

async function saveRules() {
  if (rulesSaving.value) return
  if (ruleDrafts.value.some((r) => !r.name.trim())) {
    ElMessage.warning(t('monitor.alertsRuleNameRequired'))
    return
  }
  if (!saveReason.value.trim()) {
    ElMessage.warning(t('common.reasonRequired'))
    return
  }
  rulesSaving.value = true
  try {
    const saved = await saveAlertRules({
      rules: ruleDrafts.value.map((r) => ({
        id: r.id || undefined,
        name: r.name.trim(),
        metric: r.metric,
        operator: r.operator,
        threshold: r.threshold,
        duration_sec: r.durationSec,
        severity: r.severity,
        enabled: r.enabled,
      })),
      change_reason: saveReason.value.trim(),
    })
    ruleDrafts.value = saved.map((r) => ({
      id: r.id,
      name: r.name,
      metric: r.metric,
      operator: r.operator,
      threshold: r.threshold,
      durationSec: r.duration_sec,
      severity: r.severity,
      enabled: r.enabled,
    }))
    saveReason.value = ''
    rulesError.value = false
    ElMessage.success(t('monitor.alertsSaveSuccess'))
  } catch {
    ElMessage.error(t('monitor.alertsSaveFailed'))
  } finally {
    rulesSaving.value = false
  }
}

function severityTagType(severity: AlertSeverity): 'danger' | 'warning' | 'info' {
  switch (severity) {
    case 'P1':
      return 'danger'
    case 'P2':
      return 'warning'
    default:
      return 'info'
  }
}

// Metric values keep their natural precision: integers render as integers.
function formatNumber(v: number): string {
  return Number.isInteger(v) ? String(v) : v.toFixed(2).replace(/\.?0+$/, '')
}

function pushHistory(snap: MonitorSnapshot) {
  const time = new Date().toLocaleTimeString(undefined, { hour12: false })
  onlineHistory.value.push({ time, value: snap.deviceOnlineTotal })
  p95History.value.push({
    time,
    value: snap.approvalP95Seconds === null ? NaN : Math.round(snap.approvalP95Seconds * 1000),
  })
  const cap = HISTORY_CAPACITY[range.value] ?? 240
  if (onlineHistory.value.length > cap) onlineHistory.value.splice(0, onlineHistory.value.length - cap)
  if (p95History.value.length > cap) p95History.value.splice(0, p95History.value.length - cap)
}

function emptyOption(text: string): echarts.EChartsCoreOption {
  return {
    title: {
      text,
      left: 'center',
      top: 'middle',
      textStyle: { color: '#9ca3af', fontSize: 14, fontWeight: 'normal' },
    },
  }
}

function renderOnline() {
  if (!onlineChart) return
  if (!onlineHistory.value.length) {
    onlineChart.setOption(emptyOption(t('monitor.noData')), { notMerge: true })
    return
  }
  onlineChart.setOption(
    {
      title: { show: false },
      grid: { left: 48, right: 16, top: 24, bottom: 28 },
      tooltip: { trigger: 'axis' },
      xAxis: { type: 'category', data: onlineHistory.value.map((p) => p.time), boundaryGap: false },
      yAxis: { type: 'value', name: t('monitor.deviceOnlineHint'), minInterval: 1 },
      series: [
        {
          name: t('monitor.deviceOnline'),
          type: 'line',
          smooth: true,
          showSymbol: false,
          areaStyle: { opacity: 0.15 },
          data: onlineHistory.value.map((p) => p.value),
        },
      ],
    },
    { notMerge: true },
  )
}

function renderCalls() {
  if (!callsChart) return
  const entries = Object.entries(snapshot.agentCallsByTenant).sort((a, b) => b[1] - a[1])
  if (!entries.length) {
    callsChart.setOption(emptyOption(t('monitor.noData')), { notMerge: true })
    return
  }
  const top = entries.slice(0, CALLS_BAR_MAX_TENANTS)
  const rest = entries.slice(CALLS_BAR_MAX_TENANTS).reduce((acc, [, value]) => acc + value, 0)
  const names = top.map(([tenant]) => (tenant === ALL_TENANTS_LABEL ? t('monitor.allTenants') : tenant))
  const values = top.map(([, value]) => value)
  if (rest > 0) {
    names.push(t('monitor.tenantOther'))
    values.push(rest)
  }
  callsChart.setOption(
    {
      title: { show: false },
      grid: { left: 48, right: 16, top: 24, bottom: 48 },
      tooltip: { trigger: 'axis' },
      xAxis: { type: 'category', data: names, axisLabel: { rotate: 30, width: 90, overflow: 'truncate' } },
      yAxis: { type: 'value', name: t('monitor.agentCallsHint') },
      series: [{ name: t('monitor.agentCalls'), type: 'bar', data: values, barMaxWidth: 40 }],
    },
    { notMerge: true },
  )
}

function renderHitl() {
  if (!hitlChart) return
  const data = Object.entries(snapshot.hitlByState).map(([state, value]) => ({
    name: t(`monitor.hitlStates.${state}`),
    value,
  }))
  if (snapshot.hitlBlockedTotal > 0 && !Object.keys(snapshot.hitlByState).includes('blocked')) {
    data.push({ name: t('monitor.hitlBlocked'), value: snapshot.hitlBlockedTotal })
  }
  if (!data.length) {
    hitlChart.setOption(emptyOption(t('monitor.noData')), { notMerge: true })
    return
  }
  hitlChart.setOption(
    {
      title: { show: false },
      tooltip: { trigger: 'item' },
      legend: { bottom: 0, type: 'scroll' },
      series: [
        {
          name: t('monitor.hitl'),
          type: 'pie',
          radius: ['40%', '68%'],
          center: ['50%', '45%'],
          data,
          label: { show: false },
          emphasis: { label: { show: true, fontWeight: 600 } },
        },
      ],
    },
    { notMerge: true },
  )
}

function renderP95() {
  if (!p95Chart) return
  if (!p95History.value.length) {
    p95Chart.setOption(emptyOption(t('monitor.noData')), { notMerge: true })
    return
  }
  p95Chart.setOption(
    {
      title: { show: false },
      grid: { left: 48, right: 16, top: 24, bottom: 28 },
      tooltip: { trigger: 'axis', valueFormatter: (value: unknown) => `${value} ms` },
      xAxis: { type: 'category', data: p95History.value.map((p) => p.time), boundaryGap: false },
      yAxis: { type: 'value', name: t('monitor.approvalP95Hint') },
      series: [
        {
          name: t('monitor.approvalP95'),
          type: 'line',
          smooth: true,
          showSymbol: false,
          connectNulls: true,
          data: p95History.value.map((p) => p.value),
          markLine: {
            symbol: 'none',
            silent: true,
            data: [{ yAxis: P95_THRESHOLD_MS }],
            label: { formatter: t('monitor.approvalThreshold') },
            lineStyle: { type: 'dashed', color: '#dc2626' },
          },
        },
      ],
    },
    { notMerge: true },
  )
}

function renderCharts() {
  renderOnline()
  renderCalls()
  renderHitl()
  renderP95()
}

function handleResize() {
  onlineChart?.resize()
  callsChart?.resize()
  hitlChart?.resize()
  p95Chart?.resize()
}

function startTimer() {
  stopTimer()
  timer = setInterval(refreshNow, REFRESH_INTERVAL_MS)
}

function stopTimer() {
  if (timer !== undefined) {
    clearInterval(timer)
    timer = undefined
  }
}

watch(autoRefresh, (on) => {
  if (on) startTimer()
  else stopTimer()
})

onMounted(() => {
  onlineChart = echarts.init(onlineEl.value!)
  callsChart = echarts.init(callsEl.value!)
  hitlChart = echarts.init(hitlEl.value!)
  p95Chart = echarts.init(p95El.value!)
  window.addEventListener('resize', handleResize)
  refreshNow()
  if (autoRefresh.value) startTimer()
})

onBeforeUnmount(() => {
  stopTimer()
  window.removeEventListener('resize', handleResize)
  onlineChart?.dispose()
  callsChart?.dispose()
  hitlChart?.dispose()
  p95Chart?.dispose()
})
</script>

<style scoped>
.toolbar { display: flex; align-items: center; justify-content: space-between; margin-bottom: var(--adc-space-4); }
.left { display: flex; align-items: center; gap: var(--adc-space-3); flex-wrap: wrap; }
.range-select { width: 150px; }
.muted { color: var(--adc-text-secondary); }
.stale { margin-bottom: var(--adc-space-4); }
.panel { border: 1px solid var(--adc-border); border-radius: var(--adc-radius, 6px); padding: var(--adc-space-3); margin-bottom: var(--adc-space-4); }
.panel-head { display: flex; align-items: baseline; justify-content: space-between; margin-bottom: var(--adc-space-2); }
.panel-title { font-weight: 600; }
.big-number { font-size: 22px; font-weight: 700; color: var(--adc-brand); }
.big-number.blocked { font-size: 14px; color: var(--adc-risk-3, #dc2626); }
.chart { height: 260px; }
.alerts { margin-bottom: var(--adc-space-4); }
.alerts-head { display: flex; align-items: baseline; justify-content: space-between; gap: var(--adc-space-3); }
.alerts-toolbar { display: flex; align-items: center; gap: var(--adc-space-3); margin-top: var(--adc-space-3); flex-wrap: wrap; }
.reason { max-width: 320px; }
</style>
