<template>
  <el-card>
    <div class="head">
      <h3>{{ t('menu.adapters') }}</h3>
      <span class="hint">{{ t('adapters.hint') }}</span>
    </div>
    <el-table :data="items" v-loading="loading" empty-text="--">
      <el-table-column prop="protocol" :label="t('adapters.protocol')" width="160" />
      <el-table-column prop="version" :label="t('adapters.version')" width="120" />
      <el-table-column :label="t('adapters.health')" width="120">
        <template #default="{ row }">
          <el-tag :type="row.healthy ? 'success' : 'danger'">
            {{ row.healthy ? t('adapters.healthy') : t('adapters.unhealthy') }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="detail" :label="t('adapters.detail')" min-width="220" />
      <el-table-column prop="error" :label="t('adapters.error')" min-width="180" />
    </el-table>
  </el-card>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { api } from '@/api/request'

const { t } = useI18n()
const items = ref<Array<Record<string, unknown>>>([])
const loading = ref(false)

onMounted(async () => {
  loading.value = true
  try {
    const res = await api.get<{ items: Array<Record<string, unknown>> }>('/v1/admin/adapters')
    items.value = res.items ?? []
  } finally {
    loading.value = false
  }
})
</script>

<style scoped>
.head { display: flex; align-items: baseline; gap: 12px; }
.hint { color: var(--adc-text-secondary); font-size: 13px; }
</style>
