<template>
  <router-view />
</template>

<script setup lang="ts">
import { onMounted } from 'vue'
import { useBrandingStore } from '@/stores/branding'

// Root component: the router renders either the login page or the main
// layout (with sidebar/topbar) depending on the active route.
//
// C2.1 white-label branding (design/83): the brand config is fetched once
// at app init from the public GET /v1/admin/branding and applied to the
// CSS variables and the tab title; the login page brands even before a
// session exists.
const branding = useBrandingStore()
onMounted(() => {
  void branding.load()
})
</script>
