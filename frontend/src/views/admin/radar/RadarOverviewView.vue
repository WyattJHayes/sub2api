<template>
  <div class="space-y-5">
    <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
      <Metric v-for="metric in metrics" :key="metric.label" :label="metric.label" :value="metric.value" />
    </div>
    <div class="grid gap-5 xl:grid-cols-[1.5fr_1fr]">
      <section class="overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900">
        <div class="border-b border-gray-200 px-4 py-3 text-sm font-semibold dark:border-dark-700 dark:text-white">
          {{ t('admin.radar.overview.modelHealth') }}
        </div>
        <RadarTable :rows="store.models" />
      </section>
      <section class="rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900">
        <div class="border-b border-gray-200 px-4 py-3 text-sm font-semibold dark:border-dark-700 dark:text-white">
          {{ t('admin.radar.overview.openAlerts') }}
        </div>
        <div v-if="!store.alerts.length" class="p-5 text-sm text-gray-500">{{ t('admin.radar.overview.noOpenAlerts') }}</div>
        <div v-for="alert in store.alerts.slice(0, 6)" :key="alert.id" class="flex items-center justify-between border-b border-gray-100 px-4 py-3 text-sm last:border-0 dark:border-dark-800">
          <span class="truncate text-gray-700 dark:text-dark-100">{{ alert.model_route }} · {{ causeLabel(alert.cause) }}</span>
          <Status :value="alert.status" />
        </div>
      </section>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRadarStore } from '@/stores/radar'
import Metric from './components/Metric.vue'
import RadarTable from './components/RadarTable.vue'
import Status from './components/Status.vue'

const store = useRadarStore()
const { t, te } = useI18n()
const metrics = computed(() => [
  { label: t('admin.radar.overview.metrics.models'), value: store.overview?.summary?.models ?? store.models.length },
  { label: t('admin.radar.overview.metrics.openAlerts'), value: store.overview?.summary?.open_alerts ?? store.alerts.length },
  { label: t('admin.radar.overview.metrics.blockedGates'), value: store.overview?.summary?.blocked_gates ?? store.gates.filter((gate) => gate.status === 'blocked').length },
  { label: t('admin.radar.overview.metrics.healthyWorkers'), value: store.overview?.summary?.healthy_workers ?? store.workers.filter((worker) => worker.status === 'active').length }
])

function causeLabel(value?: string): string {
  const key = `admin.radar.alerts.causes.${value}`
  return value && te(key) ? t(key) : value ?? t('common.unknown')
}
</script>
