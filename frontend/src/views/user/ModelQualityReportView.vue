<template>
  <AppLayout>
    <section class="space-y-5">
      <header class="border-b border-gray-200 pb-4 dark:border-dark-700">
        <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">{{ t('modelHealth.report.title') }}</h1>
        <p v-if="report" class="mt-1 break-all text-sm text-gray-500">{{ report.model_alias }}</p>
      </header>
      <div v-if="loading" class="rounded-lg border border-dashed border-gray-300 p-8 text-center text-sm text-gray-500 dark:border-dark-700">{{ t('common.loading') }}</div>
      <div v-else-if="notFound" class="rounded-lg border border-amber-200 bg-amber-50 p-5 text-sm text-amber-800 dark:border-amber-900/60 dark:bg-amber-900/20 dark:text-amber-200">{{ t('modelHealth.report.notFound') }}</div>
      <div v-else-if="loadError" role="alert" class="rounded-lg border border-red-200 bg-red-50 p-5 text-sm text-red-700 dark:border-red-900/60 dark:bg-red-900/20 dark:text-red-300">{{ loadError }}</div>
      <template v-else-if="report">
        <div v-if="report.overall_conclusion === 'insufficient_coverage'" class="rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 text-sm font-medium text-amber-800 dark:border-amber-900/60 dark:bg-amber-900/20 dark:text-amber-200">
          {{ t('modelHealth.report.coverageInsufficient') }}
        </div>
        <div class="grid gap-2 border-b border-gray-200 pb-4 text-xs text-gray-500 dark:border-dark-700 sm:grid-cols-2">
          <p>{{ t('modelHealth.report.generatedAt') }} {{ formatDate(report.generated_at) }}</p>
          <p>
            {{ t('modelHealth.report.freshUntil') }} {{ formatDate(report.fresh_until) }}
            <span v-if="isStale" class="ml-2 font-medium text-amber-700 dark:text-amber-300">{{ t('modelHealth.report.stale') }}</span>
          </p>
        </div>
        <QualitySummary :report="report" />
        <QualityDetectionMatrix :dimensions="report.dimension_results" />
        <QualityEvidencePanels :evidence="report.evidence" :source="report.source_attribution" />
      </template>
    </section>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import QualitySummary from '@/components/model-health/QualitySummary.vue'
import QualityDetectionMatrix from '@/components/model-health/QualityDetectionMatrix.vue'
import QualityEvidencePanels from '@/components/model-health/QualityEvidencePanels.vue'
import { getModelQualityReport, type ModelQualityReport } from '@/api/radar'

const route = useRoute()
const { t } = useI18n()
const report = ref<ModelQualityReport | null>(null)
const loading = ref(true)
const notFound = ref(false)
const loadError = ref<string | null>(null)
const isStale = computed(() => {
  if (!report.value) return false
  const timestamp = Date.parse(report.value.fresh_until)
  return !Number.isNaN(timestamp) && timestamp < Date.now()
})

onMounted(async () => {
  try {
    report.value = await getModelQualityReport(String(route.params.alias ?? ''))
  } catch (error: any) {
    const status = error?.status ?? error?.response?.status
    if (status === 404) {
      notFound.value = true
    } else {
      loadError.value = t('modelHealth.report.loadFailed')
    }
  } finally {
    loading.value = false
  }
})

function formatDate(value: string): string {
  const timestamp = Date.parse(value)
  return Number.isNaN(timestamp) ? t('common.notAvailable') : new Date(timestamp).toLocaleString()
}
</script>
