<template>
  <section class="space-y-5 p-4 sm:p-6">
    <header class="flex flex-col gap-3 border-b border-gray-200 pb-4 dark:border-dark-700 sm:flex-row sm:items-end sm:justify-between">
      <div><h1 class="text-2xl font-semibold text-gray-900 dark:text-white">Quality Radar</h1><p class="mt-1 text-sm text-gray-500">Model quality, reliability and release decisions in one workspace.</p></div>
      <button class="rounded-md bg-primary-600 px-3 py-2 text-sm font-medium text-white hover:bg-primary-700" :disabled="store.loading" @click="store.refresh()">Refresh</button>
    </header>
    <nav class="flex gap-1 overflow-x-auto border-b border-gray-200 dark:border-dark-700"><RouterLink v-for="tab in tabs" :key="tab.path" :to="tab.path" class="whitespace-nowrap border-b-2 px-3 py-2 text-sm" :class="route.path === tab.path ? 'border-primary-600 text-primary-600' : 'border-transparent text-gray-500 hover:text-gray-800 dark:hover:text-white'">{{ tab.label }}</RouterLink></nav>
    <div v-if="store.error" class="rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">{{ store.error }}</div>
    <RouterView />
  </section>
</template>
<script setup lang="ts">
import { onMounted } from 'vue'
import { useRoute } from 'vue-router'
import { useRadarStore } from '@/stores/radar'
const route = useRoute(); const store = useRadarStore()
const tabs = [{ path: '/admin/radar', label: 'Overview' }, { path: '/admin/radar/models', label: 'Models' }, { path: '/admin/radar/runs', label: 'Runs' }, { path: '/admin/radar/alerts', label: 'Alerts' }, { path: '/admin/radar/gates', label: 'Gates' }, { path: '/admin/radar/workers', label: 'Workers' }, { path: '/admin/radar/datasets', label: 'Datasets' }]
onMounted(() => { if (!store.overview) store.refresh() })
</script>
