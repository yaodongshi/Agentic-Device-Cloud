<template>
  <el-tag
    :class="`risk-tag risk-${level}`"
    effect="plain"
    size="small"
  >
    {{ t(`tools.riskLevels.${level}`) }}
  </el-tag>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { ToolRiskLevel } from '@/api/types'

// Four-color risk badge shared by the tool catalog, approval center and
// audit views. Colors follow design/20 4.5: 0 green / 1 blue / 2 orange /
// 3 red, mapped onto the --adc-risk-* tokens (design/20 6.1).
defineProps<{ level: ToolRiskLevel }>()

const { t } = useI18n()
</script>

<style scoped>
.risk-tag { border-width: 1px; font-weight: 600; }
.risk-0 { color: var(--adc-risk-0); background: rgba(22, 163, 74, 0.08); border-color: rgba(22, 163, 74, 0.35); }
.risk-1 { color: var(--adc-risk-1); background: rgba(37, 99, 235, 0.08); border-color: rgba(37, 99, 235, 0.35); }
.risk-2 { color: var(--adc-risk-2); background: rgba(217, 119, 6, 0.08); border-color: rgba(217, 119, 6, 0.35); }
.risk-3 { color: var(--adc-risk-3); background: rgba(220, 38, 38, 0.08); border-color: rgba(220, 38, 38, 0.35); }
</style>
