<template>
  <el-card>
    <h3>{{ t('menu.apikeys') }}</h3>
    <el-empty :description="t('menu.apikeys')" />
  </el-card>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
const { t } = useI18n()
</script>
