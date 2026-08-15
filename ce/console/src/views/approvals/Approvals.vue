<template>
  <el-card>
    <h3>{{ t('menu.approvals') }}</h3>
    <el-empty :description="t('menu.approvals')" />
  </el-card>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
const { t } = useI18n()
</script>
