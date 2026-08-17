<template>
  <div
    v-if="!status || status.budget_monthly_cents <= 0"
    class="budget-none"
  >
    {{ t('tenants.budgetUnset') }}
  </div>
  <div
    v-else
    class="budget"
  >
    <el-progress
      :percentage="barPercent"
      :status="barStatus"
      :stroke-width="8"
      :show-text="false"
    />
    <div class="budget-text">
      {{ fenToYuan(status.billed_fen) }} / {{ fenToYuan(status.budget_monthly_cents) }} 元
      <el-tag
        v-if="status.status === 'WARN'"
        type="warning"
        size="small"
        effect="light"
      >
        {{ t('tenants.budgetStatuses.WARN') }}
      </el-tag>
      <el-tag
        v-else-if="status.status === 'EXCEEDED'"
        type="danger"
        size="small"
        effect="light"
      >
        {{ t('tenants.budgetStatuses.EXCEEDED') }}
      </el-tag>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { fenToYuan } from '@/api/billing'
import type { BudgetStatus } from '@/api/budget'

// C5.1 budget progress cell (design/83): the bar turns orange at 80%
// usage (WARN) and red at 100% (EXCEEDED), mirroring the backend
// thresholds; the bar itself caps at 100 while the text carries the real
// amounts.
const props = defineProps<{
  status?: BudgetStatus | null
}>()

const { t } = useI18n()

const barPercent = computed(() => {
  if (!props.status) return 0
  return Math.min(100, props.status.usage_percent)
})

const barStatus = computed(() => {
  if (!props.status) return undefined
  if (props.status.status === 'EXCEEDED') return 'exception'
  if (props.status.status === 'WARN') return 'warning'
  return undefined
})
</script>

<style scoped>
.budget-none { color: var(--adc-text-secondary); }
.budget-text { display: flex; align-items: center; gap: var(--adc-space-2); font-size: 12px; color: var(--adc-text-secondary); margin-top: 2px; }
</style>
