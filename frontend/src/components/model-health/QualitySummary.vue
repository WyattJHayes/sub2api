<template>
  <section class="grid gap-3 border-b border-gray-200 pb-5 dark:border-dark-700 sm:grid-cols-2 xl:grid-cols-3">
    <div>
      <p class="text-xs text-gray-500">{{ t('modelHealth.report.declaredModel') }}</p>
      <p class="mt-1 break-all text-base font-semibold text-gray-900 dark:text-white">{{ report.model_alias }}</p>
    </div>
    <div>
      <p class="text-xs text-gray-500">{{ t('modelHealth.report.overall') }}</p>
      <p class="mt-1 text-sm font-medium" :class="conclusionClass(report.overall_conclusion)">{{ conclusionLabel(report.overall_conclusion) }}</p>
    </div>
    <div>
      <p class="text-xs text-gray-500">{{ t('modelHealth.quality.adulteration') }}</p>
      <p class="mt-1 text-sm font-medium" :class="conclusionClass(report.adulteration_risk)">{{ conclusionLabel(report.adulteration_risk) }}</p>
      <p class="mt-2 text-xs text-gray-500">{{ t('modelHealth.quality.degradation') }} {{ conclusionLabel(report.degradation_risk) }}</p>
    </div>
    <div>
      <p class="text-xs text-gray-500">{{ t('modelHealth.report.generatedAt') }}</p>
      <p class="mt-1 text-sm text-gray-800 dark:text-gray-100">{{ formatDate(report.generated_at) }}</p>
      <p class="mt-2 text-xs text-gray-500">{{ t('modelHealth.report.freshUntil') }} {{ formatDate(report.fresh_until) }}</p>
    </div>
    <div>
      <p class="text-xs text-gray-500">{{ t('modelHealth.report.source') }}</p>
      <p class="mt-1 text-sm font-medium text-gray-800 dark:text-gray-100">{{ sourceLabel }}</p>
      <p v-if="report.source_attribution.state === 'inferred' && report.source_attribution.confidence != null" class="mt-1 text-xs text-gray-500">
        {{ Math.round(report.source_attribution.confidence * 100) }}%
      </p>
      <div v-if="report.source_attribution.state === 'inferred' && report.source_attribution.alternate_candidates?.length" class="mt-2 space-y-1 text-xs text-gray-500">
        <p>{{ t('modelHealth.report.alternateCandidates') }}</p>
        <p v-for="candidate in report.source_attribution.alternate_candidates" :key="candidate.display_name">{{ candidate.display_name }} {{ Math.round(candidate.confidence * 100) }}%</p>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { ModelQualityReport, QualityConclusion } from '@/api/radar'

const props = defineProps<{ report: ModelQualityReport }>()
const { t } = useI18n()

const sourceLabel = computed(() => {
  const source = props.report.source_attribution
  if (source.state === 'confirmed') return `${t('modelHealth.report.confirmedSource')} ${source.display_name ?? ''}`.trim()
  if (source.state === 'inferred') return `${t('modelHealth.report.inferredSource')} ${source.display_name ?? ''}`.trim()
  return t('modelHealth.report.sourceUnavailable')
})

function conclusionLabel(value: QualityConclusion): string {
  return t(`modelHealth.quality.${({
    no_significant_anomaly: 'normal', observe: 'observe', suspected: 'suspected', high_risk: 'highRisk', insufficient_coverage: 'insufficient'
  } as Record<QualityConclusion, string>)[value]}`)
}

function conclusionClass(value: QualityConclusion): string {
  if (value === 'high_risk' || value === 'suspected') return 'text-red-700 dark:text-red-300'
  if (value === 'observe' || value === 'insufficient_coverage') return 'text-amber-700 dark:text-amber-300'
  return 'text-emerald-700 dark:text-emerald-300'
}

function formatDate(value: string): string {
  const timestamp = Date.parse(value)
  return Number.isNaN(timestamp) ? t('common.notAvailable') : new Date(timestamp).toLocaleString()
}
</script>
