<template>
  <section class="space-y-4">
    <div v-for="group in groups" :key="group.title" class="overflow-hidden rounded-lg border border-gray-200 dark:border-dark-700">
      <h2 class="border-b border-gray-200 bg-gray-50 px-4 py-3 text-sm font-semibold text-gray-900 dark:border-dark-700 dark:bg-dark-800 dark:text-white">{{ group.title }}</h2>
      <div v-for="dimension in group.items" :key="dimension.key" data-test="quality-dimension-row" class="grid gap-2 border-b border-gray-100 px-4 py-3 text-sm last:border-b-0 dark:border-dark-800 md:grid-cols-[minmax(10rem,1.3fr)_minmax(6rem,.8fr)_minmax(6rem,.8fr)_minmax(6rem,.8fr)_minmax(12rem,1.5fr)] md:items-center">
        <div class="font-medium text-gray-900 dark:text-white">{{ dimensionLabel(dimension.key) }}</div>
        <div class="text-xs font-medium" :class="statusClass(dimension.status)">{{ conclusionLabel(dimension.status) }}</div>
        <div class="text-gray-600 dark:text-gray-300">{{ t('modelHealth.report.samples') }} {{ dimension.sample_count }}</div>
        <div class="text-gray-600 dark:text-gray-300">{{ t('modelHealth.report.confidence') }} {{ Math.round(dimension.confidence * 100) }}%</div>
        <div data-test="quality-dimension-evidence" class="text-xs leading-5 text-gray-500 dark:text-gray-400">{{ evidenceLabel(dimension.evidence_code) }}</div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { ModelQualityDimensionResult, QualityConclusion, QualityDimension } from '@/api/radar'

const props = defineProps<{ dimensions: ModelQualityDimensionResult[] }>()
const { t } = useI18n()
const behaviorKeys: QualityDimension[] = ['knowledge_freshness', 'model_fingerprint', 'reasoning_stability', 'structure_compliance']
const groups = computed(() => [
  { title: t('modelHealth.report.behaviorGroup'), items: props.dimensions.filter((item) => behaviorKeys.includes(item.key)) },
  { title: t('modelHealth.report.protocolGroup'), items: props.dimensions.filter((item) => !behaviorKeys.includes(item.key)) }
])

function dimensionLabel(key: QualityDimension): string { return t(`modelHealth.dimension.${key}`) }
function conclusionLabel(value: QualityConclusion): string { return t(`modelHealth.quality.${({ no_significant_anomaly: 'normal', observe: 'observe', suspected: 'suspected', high_risk: 'highRisk', insufficient_coverage: 'insufficient' } as Record<QualityConclusion, string>)[value]}`) }
function statusClass(value: QualityConclusion): string { return value === 'high_risk' || value === 'suspected' ? 'text-red-700 dark:text-red-300' : value === 'observe' || value === 'insufficient_coverage' ? 'text-amber-700 dark:text-amber-300' : 'text-emerald-700 dark:text-emerald-300' }
function evidenceLabel(code: ModelQualityDimensionResult['evidence_code']): string { return t(`modelHealth.evidence.${code}`) }
</script>
