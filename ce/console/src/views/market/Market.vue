<template>
  <el-card>
    <div class="toolbar">
      <div class="filters">
        <el-input
          v-model="keyword"
          class="filter-item search"
          :placeholder="t('market.keywordPlaceholder')"
          clearable
          @keyup.enter="onSearch"
          @clear="onSearch"
        />
        <el-button
          type="primary"
          @click="onSearch"
        >
          {{ t('common.search') }}
        </el-button>
      </div>
      <el-button
        type="primary"
        @click="openPublish"
      >
        {{ t('market.publish') }}
      </el-button>
    </div>

    <el-table
      v-loading="loading"
      :data="list"
    >
      <el-table-column
        :label="t('market.name')"
        min-width="220"
      >
        <template #default="{ row }">
          <span class="pkg-name">{{ row.name }}</span>
          <span class="pkg-version">v{{ row.version }}</span>
        </template>
      </el-table-column>
      <el-table-column
        prop="author"
        :label="t('market.author')"
        min-width="140"
        show-overflow-tooltip
      />
      <el-table-column
        :label="t('market.toolCount')"
        width="110"
      >
        <template #default="{ row }">
          {{ row.tool_count }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('market.scope')"
        width="120"
      >
        <template #default="{ row }">
          <el-tag
            :type="row.scope === 'platform' ? 'primary' : 'info'"
            effect="light"
          >
            {{ t(`market.scopes.${row.scope}`) }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column
        :label="t('market.installState')"
        width="130"
      >
        <template #default="{ row }">
          <el-tag
            v-if="row.installed"
            type="success"
            effect="light"
          >
            {{ t('market.installed') }}
          </el-tag>
          <el-tag
            v-else
            type="info"
            effect="light"
          >
            {{ t('market.notInstalled') }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column
        :label="t('market.publishedAt')"
        width="170"
      >
        <template #default="{ row }">
          {{ formatTime(row.published_at) }}
        </template>
      </el-table-column>
      <el-table-column
        :label="t('common.actions')"
        width="180"
        fixed="right"
      >
        <template #default="{ row }">
          <el-button
            link
            type="primary"
            @click="openDetail(row)"
          >
            {{ t('market.detail') }}
          </el-button>
          <el-button
            v-if="!row.installed"
            link
            type="success"
            @click="runInstall(row)"
          >
            {{ t('market.install') }}
          </el-button>
          <el-button
            v-else
            link
            type="danger"
            @click="runUninstall(row)"
          >
            {{ t('market.uninstall') }}
          </el-button>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty :description="t('market.empty')" />
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
      @current-change="fetchList"
    />

    <!-- Detail drawer: package metadata plus the tool manifest with the
         standard MCP tool shape (name/description/inputSchema/risk). -->
    <el-drawer
      v-model="detailVisible"
      :title="t('market.detailTitle')"
      size="520px"
    >
      <template v-if="detail">
        <el-descriptions
          :column="1"
          border
        >
          <el-descriptions-item :label="t('market.name')">
            {{ detail.name }} <span class="pkg-version">v{{ detail.version }}</span>
          </el-descriptions-item>
          <el-descriptions-item :label="t('market.author')">
            {{ detail.author }}
          </el-descriptions-item>
          <el-descriptions-item :label="t('market.scope')">
            {{ t(`market.scopes.${detail.scope}`) }}
          </el-descriptions-item>
          <el-descriptions-item :label="t('market.description')">
            {{ detail.description || '—' }}
          </el-descriptions-item>
          <el-descriptions-item :label="t('market.installState')">
            {{ detail.installed ? `${t('market.installed')} · ${formatTime(detail.installed_at ?? '')}` : t('market.notInstalled') }}
          </el-descriptions-item>
          <el-descriptions-item :label="t('market.publishedAt')">
            {{ formatTime(detail.published_at) }}
          </el-descriptions-item>
        </el-descriptions>

        <h4 class="section-title">
          {{ t('market.toolsTitle', { n: detail.tools.length }) }}
        </h4>
        <el-collapse>
          <el-collapse-item
            v-for="tool in detail.tools"
            :key="tool.name"
            :name="tool.name"
          >
            <template #title>
              <span class="mono">{{ tool.name }}</span>
              <RiskTag
                :level="tool.risk_level ?? 2"
                class="risk"
              />
            </template>
            <p class="tool-desc">
              {{ tool.description }}
            </p>
            <pre class="mono schema">{{ prettyJson(tool.input_schema) }}</pre>
          </el-collapse-item>
        </el-collapse>
      </template>
    </el-drawer>

    <!-- Publish dialog: simple form; tools are pasted as a JSON array of
         standard MCP tool definitions and validated client-side. -->
    <el-dialog
      v-model="publishVisible"
      :title="t('market.publishTitle')"
      width="640px"
    >
      <el-form
        ref="publishFormRef"
        :model="publishForm"
        :rules="publishRules"
        label-width="100px"
      >
        <el-form-item
          :label="t('market.name')"
          prop="name"
        >
          <el-input
            v-model="publishForm.name"
            maxlength="128"
            :placeholder="t('market.namePlaceholder')"
          />
        </el-form-item>
        <el-form-item
          :label="t('market.version')"
          prop="version"
        >
          <el-input
            v-model="publishForm.version"
            maxlength="32"
            placeholder="1.0.0"
          />
        </el-form-item>
        <el-form-item
          :label="t('market.author')"
          prop="author"
        >
          <el-input
            v-model="publishForm.author"
            maxlength="128"
            :placeholder="t('market.authorPlaceholder')"
          />
        </el-form-item>
        <el-form-item
          :label="t('market.description')"
          prop="description"
        >
          <el-input
            v-model="publishForm.description"
            type="textarea"
            :rows="3"
            maxlength="2000"
            show-word-limit
          />
        </el-form-item>
        <el-form-item
          :label="t('market.toolsJson')"
          prop="toolsText"
        >
          <el-input
            v-model="publishForm.toolsText"
            type="textarea"
            :rows="10"
            class="mono"
            :placeholder="toolsPlaceholder"
          />
        </el-form-item>
      </el-form>
      <el-alert
        type="info"
        :closable="false"
        show-icon
        :title="t('market.publishHint')"
      />
      <template #footer>
        <el-button @click="publishVisible = false">
          {{ t('common.cancel') }}
        </el-button>
        <el-button
          type="primary"
          :loading="publishing"
          @click="submitPublish"
        >
          {{ t('market.publish') }}
        </el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'
import { errorMessage } from '@/api/errors'
import { formatTime } from '@/utils/format'
import RiskTag from '@/components/RiskTag.vue'
import {
  fetchToolPackageDetail,
  fetchToolPackages,
  installToolPackage,
  publishToolPackage,
  uninstallToolPackage,
  type MarketTool,
  type ToolPackage,
  type ToolPackageDetail,
} from '@/api/market'

const { t } = useI18n()

const loading = ref(false)
const list = ref<ToolPackage[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const keyword = ref('')

// GET /v1/admin/tool-packages (design/83 C3.1 market list). The request
// layer auto-appends the session tenant scope (platform admins operate
// on their session tenant; tenant admins are locked to their own).
async function fetchList() {
  loading.value = true
  try {
    const res = await fetchToolPackages(keyword.value.trim(), page.value, pageSize.value)
    list.value = res.items ?? []
    total.value = res.total ?? 0
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    loading.value = false
  }
}

function onSearch() {
  page.value = 1
  fetchList()
}

// --- detail drawer ---

const detailVisible = ref(false)
const detail = ref<ToolPackageDetail>()

async function openDetail(row: ToolPackage) {
  detail.value = undefined
  detailVisible.value = true
  try {
    detail.value = await fetchToolPackageDetail(row.id)
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  }
}

function prettyJson(schema: Record<string, unknown> | undefined | null): string {
  if (!schema) return '{}'
  try {
    return JSON.stringify(schema, null, 2)
  } catch {
    return String(schema)
  }
}

// --- install / uninstall ---

async function runInstall(row: ToolPackage) {
  const ok = await ElMessageBox.confirm(
    t('market.installConfirm', { name: row.name }),
    t('market.install'),
    { type: 'info', confirmButtonText: t('common.confirm'), cancelButtonText: t('common.cancel') },
  ).catch(() => false)
  if (!ok) return
  try {
    await installToolPackage(row.id)
    ElMessage.success(t('market.installSuccess'))
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  }
}

async function runUninstall(row: ToolPackage) {
  const ok = await ElMessageBox.confirm(
    t('market.uninstallConfirm', { name: row.name }),
    t('market.uninstall'),
    { type: 'warning', confirmButtonText: t('common.confirm'), cancelButtonText: t('common.cancel') },
  ).catch(() => false)
  if (!ok) return
  try {
    await uninstallToolPackage(row.id)
    ElMessage.success(t('market.uninstallSuccess'))
    await fetchList()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  }
}

// --- publish dialog ---

const publishVisible = ref(false)
const publishing = ref(false)
const publishFormRef = ref<FormInstance>()
const publishForm = reactive({
  name: '',
  version: '',
  author: '',
  description: '',
  toolsText: '',
})

const publishRules = computed<FormRules>(() => ({
  name: [
    { required: true, message: t('market.nameRequired'), trigger: 'blur' },
    {
      pattern: /^[A-Za-z0-9][A-Za-z0-9 ._-]{0,127}$/,
      message: t('market.namePattern'),
      trigger: 'blur',
    },
  ],
  version: [
    { required: true, message: t('market.versionRequired'), trigger: 'blur' },
    {
      pattern: /^[0-9A-Za-z][0-9A-Za-z._-]{0,31}$/,
      message: t('market.versionPattern'),
      trigger: 'blur',
    },
  ],
  toolsText: [
    { required: true, message: t('market.toolsRequired'), trigger: 'blur' },
    {
      validator: (_rule, value: string, cb: (err?: Error) => void) => {
        if (!value.trim()) {
          cb(new Error(t('market.toolsRequired')))
          return
        }
        const parsed = parseTools(value)
        cb(parsed instanceof Error ? parsed : undefined)
      },
      trigger: 'blur',
    },
  ],
}))

const toolsPlaceholder = `[
  {
    "name": "get_status",
    "description": "read machine status",
    "inputSchema": { "type": "object", "properties": {}, "required": [] },
    "riskLevel": 0
  }
]`

/** Client-side mirror of the server tool validation (design/83 C3.1:
 * standard MCP tool definitions; the server re-validates authoritatively). */
function parseTools(text: string): MarketTool[] | Error {
  let raw: unknown
  try {
    raw = JSON.parse(text)
  } catch {
    return new Error(t('market.toolsInvalidJson'))
  }
  if (!Array.isArray(raw) || raw.length === 0) {
    return new Error(t('market.toolsInvalidJson'))
  }
  const tools: MarketTool[] = []
  for (let i = 0; i < raw.length; i++) {
    const item = raw[i]
    if (typeof item !== 'object' || item === null) return new Error(t('market.toolsInvalidJson'))
    const tool = item as Record<string, unknown>
    if (typeof tool.name !== 'string' || !tool.name.trim()) {
      return new Error(t('market.toolBadName', { n: i }))
    }
    if (typeof tool.description !== 'string' || !tool.description.trim()) {
      return new Error(t('market.toolBadDescription', { name: tool.name }))
    }
    if (typeof tool.inputSchema !== 'object' || tool.inputSchema === null || Array.isArray(tool.inputSchema)) {
      return new Error(t('market.toolBadSchema', { name: tool.name }))
    }
    tools.push({
      name: tool.name,
      description: tool.description,
      input_schema: tool.inputSchema as Record<string, unknown>,
      risk_level: typeof tool.riskLevel === 'number' ? (tool.riskLevel as 0 | 1 | 2 | 3) : undefined,
      schema_version: typeof tool.schemaVersion === 'string' ? tool.schemaVersion : undefined,
    })
  }
  return tools
}

function openPublish() {
  publishForm.name = ''
  publishForm.version = ''
  publishForm.author = ''
  publishForm.description = ''
  publishForm.toolsText = ''
  publishVisible.value = true
}

async function submitPublish() {
  const valid = await publishFormRef.value?.validate().catch(() => false)
  if (!valid) return
  const tools = parseTools(publishForm.toolsText)
  if (tools instanceof Error) {
    ElMessage.error(tools.message)
    return
  }
  publishing.value = true
  try {
    await publishToolPackage({
      name: publishForm.name.trim(),
      version: publishForm.version.trim(),
      description: publishForm.description.trim(),
      author: publishForm.author.trim() || undefined,
      tools,
    })
    publishVisible.value = false
    ElMessage.success(t('market.publishSuccess'))
    onSearch()
  } catch (err) {
    ElMessage.error(errorMessage(err, t))
  } finally {
    publishing.value = false
  }
}

onMounted(() => {
  fetchList()
})
</script>

<style scoped>
.toolbar { display: flex; align-items: flex-start; justify-content: space-between; margin-bottom: var(--adc-space-4); }
.filters { display: flex; gap: var(--adc-space-2); }
.filter-item { width: 220px; }
.search { width: 320px; }
.pagination { margin-top: var(--adc-space-4); justify-content: flex-end; }
.pkg-name { font-weight: 600; }
.pkg-version { margin-left: var(--adc-space-2); color: var(--adc-text-secondary); font-size: 12px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.section-title { margin: var(--adc-space-4) 0 var(--adc-space-2); font-weight: 600; }
.tool-desc { margin: 0 0 var(--adc-space-2); color: var(--adc-text-secondary); }
.risk { margin-left: var(--adc-space-2); }
.schema {
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
</style>
