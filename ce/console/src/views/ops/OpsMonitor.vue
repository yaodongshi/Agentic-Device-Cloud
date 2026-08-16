<template>
  <div class="ops">
    <!-- A2.5: Grafana dashboard embedded for platform admins only. The
         iframe target is a build-time placeholder (VITE_GRAFANA_EMBED_URL);
         without it the page shows enablement guidance instead of a dead
         frame. The embed URL comes from the observability profile
         (deploy/observability, Grafana on 19300). -->
    <el-card v-if="embedUrl">
      <iframe
        :src="embedUrl"
        class="grafana"
        title="Grafana"
      />
    </el-card>
    <el-card v-else>
      <el-empty :description="t('ops.noUrl')">
        <template #default>
          <div class="guide">
            <el-alert
              type="info"
              :title="t('ops.guideTitle')"
              :description="t('ops.guideDesc')"
              show-icon
              :closable="false"
            />
            <pre class="snippet">VITE_GRAFANA_EMBED_URL=http://localhost:19300</pre>
          </div>
        </template>
      </el-empty>
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

const { t } = useI18n()

// Build-time placeholder (A2.5): the console SPA is static, so the Grafana
// embed target must be injected when the image is built
// (VITE_GRAFANA_EMBED_URL, e.g. http://localhost:19300 from the
// observability profile). Empty means the profile is not enabled.
const embedUrl = computed(() => import.meta.env.VITE_GRAFANA_EMBED_URL as string | undefined)
</script>

<style scoped>
.ops { height: calc(100vh - 120px); }
.grafana { width: 100%; height: calc(100vh - 160px); border: 0; }
.guide { display: flex; flex-direction: column; align-items: center; gap: var(--adc-space-3); max-width: 560px; margin: 0 auto; text-align: left; }
.snippet { background: var(--adc-bg, #f5f7fa); border: 1px solid var(--adc-border); border-radius: 4px; padding: 8px 12px; font-size: 13px; }
</style>
