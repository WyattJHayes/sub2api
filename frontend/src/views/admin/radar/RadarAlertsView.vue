<template>
  <section class="overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900">
    <div class="border-b border-gray-200 px-4 py-3 text-sm font-semibold dark:border-dark-700 dark:text-white">{{ t('admin.radar.alerts.title') }}</div>
    <div class="overflow-x-auto">
      <table class="min-w-full text-left text-sm">
        <thead>
          <tr class="border-b border-gray-200 text-xs uppercase text-gray-500 dark:border-dark-700">
            <th class="px-4 py-3">{{ t('admin.radar.table.model') }}</th>
            <th class="px-4 py-3">{{ t('admin.radar.table.domain') }}</th>
            <th class="px-4 py-3">{{ t('admin.radar.alerts.cause') }}</th>
            <th class="px-4 py-3">{{ t('admin.radar.alerts.severity') }}</th>
            <th class="px-4 py-3">{{ t('admin.radar.table.status') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="alert in store.alerts" :key="alert.id" class="border-b border-gray-100 dark:border-dark-800">
            <td class="px-4 py-3 font-medium dark:text-white">{{ alert.model_route }}</td>
            <td class="px-4 py-3">{{ domainLabel(alert.capability_domain) }}</td>
            <td class="px-4 py-3">{{ causeLabel(alert.cause) }}</td>
            <td class="px-4 py-3">{{ alert.severity ?? 'P1' }}</td>
            <td class="px-4 py-3"><Status :value="alert.status" /></td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRadarStore } from '@/stores/radar'
import Status from './components/Status.vue'

const store = useRadarStore()
const { t, te } = useI18n()

function domainLabel(value?: string): string {
  const normalized = value || 'aggregate'
  const key = `admin.radar.domains.${normalized}`
  return te(key) ? t(key) : normalized
}

function causeLabel(value?: string): string {
  const key = `admin.radar.alerts.causes.${value}`
  return value && te(key) ? t(key) : value ?? t('common.unknown')
}

onMounted(() => store.loadSection('alerts'))
</script>
