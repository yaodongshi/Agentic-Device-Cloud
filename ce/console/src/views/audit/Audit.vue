<template>
  <el-card>
    <div class="toolbar">
      <div class="filters">
        <el-date-picker
          v-model="timeRange"
          class="filter-item time"
          type="datetimerange"
          :start-placeholder="t('audit.timeFrom')"
          :end-placeholder="t('audit.timeTo')"
          value-format="YYYY-MM-DDTHH:mm:ss[Z]"
          :shortcuts="shortcuts"
        />
        <el-input
          v-model="filters.deviceCode"
          class="filter-item"
          :placeholder="t('audit.devicePlaceholder')"
          clearable
          @keyup.enter="search"
        />
        <el-input
          v-model="filters.toolName"
          class="filter-item"
          :placeholder="t('audit.toolPlaceholder')"
          clearable
          @keyup.enter="search"
        />
        <el-select
          v-model="filters.status"
          class="filter-item"
          :placeholder="t('audit.statusLabel')"
          clearable
        >
          <el-option
            v-for="(label, key) in tm('audit.statuses')"
            :key="key"
            :label="label"
            :value="key"
          />
        </el-select>
        <el-input
          v-model="filters.traceId"
          class="filter-item trace"
          :placeholder="t('audit.tracePlaceholder')"
          clearable
          @keyup.enter="search"
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
      <div class="exports">
        <el-button
          :loading="exporting === 'csv'"
          @click="exportLogs('csv')"
        >
          {{ t('audit.exportCsv') }}
        </el-button>
        <el-button
          :loading="exporting === 'json'"
          @click="exportLogs('json')"
        >
          {{ t('audit.exportJson') }}
        </el-button>
      </div>
    </div>

    <el-table
      v-loading="loading"
      :data="list"
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
            :type="statusTagType(row.status)"
            effect="light"
          >
            {{ t(`audit.statuses.${row.status}`) }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column
        prop="hitl_approver"
        :label="t('audit.approver')"
        width="130"
      >
        <template #default="{ row }">
          {{ row.hitl_approver ?? '—' }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('audit.traceId')"
        min-width="250"
      >
        <template #default="{ row }">
          <span class="mono">{{ row.trace_id }}</span>
          <el-button
            link
            type="primary"
            class="copy"
            @click="copyTrace(row.trace_id)"
          >
            {{ t('common.copy') }}
          </el-button>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty :description="t('audit.empty')">
          <div class="empty-actions">
            <el-button @click="reset">
              {{ t('common.reset') }}
            </el-button>
          </div>
        </el-empty>
      </template>
    </el-table>

    <!-- Cursor pagination (design/33 1.6): audit logs are append-only and
         paged by an opaque cursor; the page stack keeps "previous" usable. -->
    <div class="pagination">
      <span class="page-info">{{ t('audit.pageOf', { n: cursorStack.length + 1 }) }}</span>
      <el-select
        v-model="limit"
        class="limit"
        @change="search"
      >
        <el-option
          v-for="n in [20, 50, 100]"
          :key="n"
          :label="String(n)"
          :value="n"
        />
      </el-select>
      <el-button
        :disabled="cursorStack.length === 0"
        @click="prevPage"
      >
        {{ t('audit.prevPage') }}
      </el-button>
      <el-button
        type="primary"
        :disabled="!nextCursor"
        @click="nextPage"
      >
        {{ t('audit.nextPage') }}
      </el-button>
    </div>
  </el-card>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage } from 'element-plus'
import { api, download } from '@/api/request'
import { errorMessage } from '@/api/errors'
import { copyText } from '@/utils/clipboard'
import { formatTime } from '@/utils/format'
import type { AuditLog, AuditLogPage } from '@/api/types'

const { t, tm } = useI18n()

// Time range is mandatory for the query (design/20 4.8): default last 24h,
// max span 31 days to avoid full scans. Shortcuts match design/20 5.2.
const day = 24 * 60 * 60 * 1000

function defaultRange(): [Date, Date] {
  const end = new Date()
  return [new Date(end.getTime() - day), end]
}

function rangeToIso(range: [Date, Date]): [string, string] {
  return [range[0].toISOString(), range[1].toISOString()]
}

const timeRange = ref<[Date, Date] | null>(defaultRange())
const filters = reactive({ deviceCode: '', toolName: '', status: '', traceId: '' })

const shortcuts = computed(() => [
  { text: t('audit.shortcut24h'), value: () => [new Date(Date.now() - day), new Date()] as [Date, Date] },
  { text: t('audit.shortcut7d'), value: () => [new Date(Date.now() - 7 * day), new Date()] as [Date, Date] },
])

const loading = ref(false)
const list = ref<AuditLog[]>([])
const limit = ref(50)
const nextCursor = ref<string | null>(null)
// Stack of cursors for visited pages: cursorStack[i] is the cursor that
// led to page i+1; "previous" pops the top (design/33 1.6).
const cursorStack = ref<string[]>([])

function queryParams(): Record<string, unknown> {
  const [timeFrom, timeTo] = timeRange.value ? rangeToIso(timeRange.value) : ['', '']
  return {
    limit: limit.value,
    time_from: timeFrom || undefined,
    time_to: timeTo || undefined,
    device_code: filters.deviceCode || undefined,
    tool_name: filters.toolName || undefined,
    status: filters.status || undefined,
    // Exact trace_id filter (design/20 4.8). design/33 3.1.16 does not
    // list this param yet; it is forwarded as an optional extra and the
    // gateway/backend simply ignore unknown fields until the contract
    // catches up.
    trace_id: filters.traceId || undefined,
  }
}

// GET /v1/admin/audit-logs (design/33 3.1.16), cursor pagination.
async function fetchList(cursor?: string) {
  loading.value = true
  try {
    const res = await api.get<AuditLogPage>('/v1/admin/audit-logs', {
      ...queryParams(),
      cursor: cursor || undefined,
    })
    list.value = res.items ?? []
    nextCursor.value = res.next_cursor ?? null
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    loading.value = false
  }
}

function validateRange(): boolean {
  if (!timeRange.value) {
    ElMessage.warning(t('audit.timeRequired'))
    return false
  }
  const span = timeRange.value[1].getTime() - timeRange.value[0].getTime()
  if (span > 31 * day) {
    ElMessage.warning(t('audit.timeSpanLimit'))
    return false
  }
  if (span < 0) {
    ElMessage.warning(t('audit.timeInvalid'))
    return false
  }
  return true
}

function search() {
  if (!validateRange()) return
  cursorStack.value = []
  nextCursor.value = null
  fetchList()
}

function reset() {
  filters.deviceCode = ''
  filters.toolName = ''
  filters.status = ''
  filters.traceId = ''
  timeRange.value = defaultRange()
  search()
}

function nextPage() {
  if (!nextCursor.value) return
  cursorStack.value.push(nextCursor.value)
  fetchList(nextCursor.value)
}

function prevPage() {
  cursorStack.value.pop()
  fetchList(cursorStack.value.length ? cursorStack.value[cursorStack.value.length - 1] : undefined)
}

function statusTagType(status: string): 'success' | 'warning' | 'danger' | 'info' {
  switch (status) {
    case 'success': return 'success'
    case 'blocked_by_hitl': return 'warning'
    case 'failed': return 'danger'
    default: return 'info'
  }
}

async function copyTrace(traceId: string) {
  const ok = await copyText(traceId)
  ElMessage[ok ? 'success' : 'error'](ok ? t('common.copied') : t('common.copyFailed'))
}

// Export: GET /v1/admin/audit-logs?format=csv|json (design/33 3.1.16).
// The backend streams the file (up to 100k rows); over-limit raises 14002
// and the user is pointed to the async export task (design/20 3.4).
const exporting = ref('')

async function exportLogs(format: 'csv' | 'json') {
  if (!validateRange()) return
  exporting.value = format
  try {
    const blob = await download('/v1/admin/audit-logs', { ...queryParams(), format })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    const stamp = new Date().toISOString().replace(/[-:]/g, '').slice(0, 15)
    a.href = url
    a.download = `audit-logs-${stamp}.${format}`
    a.click()
    URL.revokeObjectURL(url)
    ElMessage.success(t('audit.exportSuccess'))
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
    // 14002: result set exceeds the sync export limit (design/33 3.1.16).
    const code = (err as { code?: string })?.code
    if (code === '14002') {
      ElMessage.warning(t('audit.exportOverLimit'))
    }
  } finally {
    exporting.value = ''
  }
}

onMounted(() => {
  fetchList()
})
</script>

<style scoped>
.toolbar { display: flex; align-items: flex-start; justify-content: space-between; margin-bottom: var(--adc-space-4); gap: var(--adc-space-2); flex-wrap: wrap; }
.filters { display: flex; flex-wrap: wrap; gap: var(--adc-space-2); }
.filter-item { width: 190px; }
.filter-item.time { width: 380px; }
.filter-item.trace { width: 280px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.copy { margin-left: var(--adc-space-2); }
.pagination { margin-top: var(--adc-space-4); display: flex; align-items: center; justify-content: flex-end; gap: var(--adc-space-2); }
.page-info { color: var(--adc-text-secondary); margin-right: var(--adc-space-2); }
.limit { width: 90px; }
.empty-actions { display: flex; justify-content: center; }
</style>
