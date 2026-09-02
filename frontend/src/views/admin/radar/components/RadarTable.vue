<template>
  <div class="overflow-x-auto">
    <table class="min-w-[720px] w-full text-left text-sm">
      <thead>
        <tr class="border-b border-gray-200 text-xs uppercase text-gray-500 dark:border-dark-700">
          <th class="px-4 py-3">{{ t('admin.radar.table.model') }}</th>
          <th class="px-4 py-3">{{ t('admin.radar.table.domain') }}</th>
          <th class="px-4 py-3">{{ t('admin.radar.table.health') }}</th>
          <th class="px-4 py-3">{{ t('admin.radar.table.delta') }}</th>
          <th class="px-4 py-3">{{ t('admin.radar.table.ci') }}</th>
          <th class="px-4 py-3">{{ t('admin.radar.table.samples') }}</th>
          <th class="px-4 py-3">{{ t('admin.radar.table.p99') }}</th>
          <th v-if="showActions" class="px-4 py-3 text-right">{{ t('common.actions') }}</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="row in rows" :key="row.model_route + row.capability_domain" class="border-b border-gray-100 dark:border-dark-800">
          <td class="px-4 py-3 font-medium dark:text-white">{{ row.model_route }}</td>
          <td class="px-4 py-3">{{ domainLabel(row.capability_domain) }}</td>
          <td class="px-4 py-3"><Status :value="row.health_state ?? 'insufficient_evidence'" /></td>
          <td class="px-4 py-3">{{ row.delta_pp == null ? t('common.notAvailable') : `${row.delta_pp.toFixed(2)} pp` }}</td>
          <td class="px-4 py-3">{{ row.ci_low_pp == null ? t('common.notAvailable') : `${row.ci_low_pp.toFixed(2)} / ${(row.ci_high_pp ?? 0).toFixed(2)}` }}</td>
          <td class="px-4 py-3">{{ row.sample_count ?? t('common.notAvailable') }}</td>
          <td class="px-4 py-3">{{ row.p99_ms == null ? t('common.notAvailable') : `${row.p99_ms} ms` }}</td>
          <td v-if="showActions" class="px-4 py-3 text-right"><slot name="actions" :row="row" /></td>
        </tr>
      </tbody>
    </table>
    <div v-if="!rows.length" class="p-5 text-sm text-gray-500">{{ t('admin.radar.empty.models') }}</div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { RadarModelHealth } from '@/api/admin/radar'
import Status from './Status.vue'

defineProps<{ rows: RadarModelHealth[]; showActions?: boolean }>()

const { t, te } = useI18n()

function domainLabel(value?: string): string {
  const normalized = value || 'aggregate'
  const key = `admin.radar.domains.${normalized}`
  return te(key) ? t(key) : normalized
}
</script>
