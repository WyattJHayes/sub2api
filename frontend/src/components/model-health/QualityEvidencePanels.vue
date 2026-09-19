<template>
  <section class="grid gap-4 lg:grid-cols-2">
    <div class="rounded-lg border border-gray-200 dark:border-dark-700">
      <h2 class="border-b border-gray-200 px-4 py-3 text-sm font-semibold text-gray-900 dark:border-dark-700 dark:text-white">{{ t('modelHealth.report.keyEvidence') }}</h2>
      <ul class="divide-y divide-gray-100 dark:divide-dark-800">
        <li v-for="evidence in evidence" :key="`${evidence.dimension_key ?? 'source'}-${evidence.code}`" class="px-4 py-3 text-sm text-gray-600 dark:text-gray-300">{{ evidenceLabel(evidence.code) }}</li>
        <li v-if="!evidence.length" class="px-4 py-3 text-sm text-gray-500">{{ t('modelHealth.report.noEvidence') }}</li>
      </ul>
    </div>
    <div class="rounded-lg border border-gray-200 dark:border-dark-700">
      <h2 class="border-b border-gray-200 px-4 py-3 text-sm font-semibold text-gray-900 dark:border-dark-700 dark:text-white">{{ t('modelHealth.report.source') }}</h2>
      <div class="space-y-2 px-4 py-3 text-sm text-gray-600 dark:text-gray-300">
        <p>{{ sourceText }}</p>
        <p v-for="candidate in inferredCandidates" :key="candidate.display_name">{{ candidate.display_name }} {{ Math.round(candidate.confidence * 100) }}%</p>
      </div>
      <p class="border-t border-gray-100 px-4 py-3 text-xs text-gray-500 dark:border-dark-800">{{ t('modelHealth.report.sourceBoundary') }}</p>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { ModelQualityEvidence, ModelQualitySourceAttribution, QualityEvidenceCode } from '@/api/radar'

const props = defineProps<{ evidence: ModelQualityEvidence[]; source: ModelQualitySourceAttribution }>()
const { t } = useI18n()
const sourceText = computed(() => props.source.state === 'confirmed' ? `${t('modelHealth.report.confirmedSource')} ${props.source.display_name ?? ''}` : props.source.state === 'inferred' ? `${t('modelHealth.report.inferredSource')} ${props.source.display_name ?? ''}` : t('modelHealth.report.sourceUnavailable'))
const inferredCandidates = computed(() => props.source.state === 'inferred' ? props.source.alternate_candidates ?? [] : [])
function evidenceLabel(code: QualityEvidenceCode): string { return t(`modelHealth.evidence.${code}`) }
</script>
