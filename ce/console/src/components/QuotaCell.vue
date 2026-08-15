<template>
  <div
    v-if="quota === null || quota === undefined"
    class="quota-none"
  >
    {{ t('tenants.quotaNone') }}
  </div>
  <div
    v-else
    class="quota"
  >
    <el-progress
      :percentage="barPercent"
      :status="barStatus"
      :stroke-width="8"
      :show-text="false"
    />
    <div class="quota-text">
      {{ formatCompact(used ?? 0) }} / {{ formatCompact(quota) }}
      <el-tag
        v-if="nearLimit"
        type="warning"
        size="small"
        effect="light"
      >
        {{ t('tenants.quotaWarn') }}
      </el-tag>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatCompact } from '@/utils/format'

// Shared used/quota progress cell (design/20 4.10): the bar turns orange at
// 80% usage and red at 100%; a warning badge shows when less than 10%
// remains. Rendering stays independent of the tenant row shape so it also
// fits future quota-aware pages.
const props = defineProps<{
  used?: number | null
  quota?: number | null
}>()

const { t } = useI18n()

const barPercent = computed(() => {
  const quota = props.quota ?? 0
  const used = props.used ?? 0
  if (quota <= 0) return 0
  return Math.min(100, Math.round((used / quota) * 1000) / 10)
})

const barStatus = computed(() => {
  const quota = props.quota ?? 0
  const used = props.used ?? 0
  if (quota > 0 && used >= quota) return 'exception'
  if (barPercent.value >= 80) return 'warning'
  return undefined
})

const nearLimit = computed(() => {
  const quota = props.quota ?? 0
  const used = props.used ?? 0
  return quota > 0 && used < quota && used / quota > 0.9
})
</script>

<style scoped>
.quota-none { color: var(--adc-text-secondary); }
.quota-text { display: flex; align-items: center; gap: var(--adc-space-2); font-size: 12px; color: var(--adc-text-secondary); margin-top: 2px; }
</style>
