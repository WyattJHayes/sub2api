<template>
  <AppLayout>
    <section class="space-y-5">
      <header class="flex flex-col gap-3 border-b border-gray-200 pb-4 dark:border-dark-700 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">{{ t('modelHealth.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500">{{ t('modelHealth.description') }}</p>
        </div>
        <button
          type="button"
          class="rounded-md bg-primary-600 px-3 py-2 text-sm font-medium text-white hover:bg-primary-700 disabled:cursor-not-allowed disabled:opacity-60"
          :disabled="loading"
          @click="loadModels"
        >
          {{ t('common.refresh') }}
        </button>
      </header>

      <div v-if="loading" class="rounded-lg border border-dashed border-gray-300 p-8 text-center text-sm text-gray-500 dark:border-dark-700">
        {{ t('common.loading') }}
      </div>

      <div v-else-if="loadError" role="alert" class="rounded-lg border border-red-200 bg-red-50 p-5 text-sm text-red-700 dark:border-red-900/60 dark:bg-red-900/20 dark:text-red-300">
        {{ loadError }}
      </div>

      <div v-else-if="models.length" class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        <RouterLink
          v-for="model in models"
          :key="model.model_alias"
          :to="{ name: 'ModelQualityReport', params: { alias: model.model_alias } }"
          class="block"
        >
        <article
          :data-test="`model-quality-report-${model.model_alias}`"
          class="rounded-lg border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-900"
        >
          <div class="flex items-center justify-between gap-3">
            <h2 class="font-medium text-gray-900 dark:text-white">{{ model.model_alias }}</h2>
            <span class="rounded-full px-2 py-1 text-xs font-medium" :class="statusClass(model.health_state)">
              {{ statusLabel(model.health_state) }}
            </span>
          </div>
          <div class="mt-4 flex flex-wrap gap-2">
            <span class="rounded-full px-2 py-1 text-xs font-medium" :class="qualityClass(model.overall_conclusion)">
              {{ qualityLabel(model.overall_conclusion) }}
            </span>
            <span v-if="model.adulteration_risk" class="rounded-full bg-amber-100 px-2 py-1 text-xs font-medium text-amber-800 dark:bg-amber-900/30 dark:text-amber-200">
              {{ t('modelHealth.quality.adulteration') }} {{ qualityLabel(model.adulteration_risk) }}
            </span>
            <span v-if="model.degradation_risk" class="rounded-full bg-sky-100 px-2 py-1 text-xs font-medium text-sky-800 dark:bg-sky-900/30 dark:text-sky-200">
              {{ t('modelHealth.quality.degradation') }} {{ qualityLabel(model.degradation_risk) }}
            </span>
          </div>
          <p v-if="isValidDate(model.freshness)" class="mt-4 text-sm text-gray-500">
            {{ t('modelHealth.updated') }}
            <time :datetime="model.freshness">{{ formatFreshness(model.freshness) }}</time>
          </p>
          <p :class="isValidDate(model.freshness) ? 'mt-1' : 'mt-4'" class="text-sm text-gray-500">
            {{ t('modelHealth.p99') }} {{ model.p99_ms == null ? t('common.notAvailable') : `${model.p99_ms} ms` }}
          </p>
          <p class="mt-1 text-sm text-gray-500">
            {{ t('modelHealth.errorRate') }} {{ model.error_rate == null ? t('common.notAvailable') : `${(model.error_rate * 100).toFixed(2)}%` }}
          </p>
        </article>
        </RouterLink>
      </div>

      <div v-else class="rounded-lg border border-dashed border-gray-300 p-8 text-center text-sm text-gray-500 dark:border-dark-700">
        {{ t('modelHealth.empty') }}
      </div>
    </section>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import { getModelHealth, type QualityConclusion, type RadarPublicHealth } from '@/api/radar'

const { t, te } = useI18n()
const models = ref<RadarPublicHealth[]>([])
const loading = ref(false)
const loadError = ref<string | null>(null)

async function loadModels() {
  loading.value = true
  loadError.value = null
  try {
    models.value = await getModelHealth()
  } catch {
    models.value = []
    loadError.value = t('modelHealth.loadFailed')
  } finally {
    loading.value = false
  }
}

function statusLabel(value: RadarPublicHealth['health_state']): string {
  const key = `modelHealth.status.${value}`
  return te(key) ? t(key) : value
}

function statusClass(value: RadarPublicHealth['health_state']): string {
  if (value === 'healthy') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
  if (value === 'degraded') return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
  return 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
}

function qualityLabel(value: RadarPublicHealth['overall_conclusion']): string {
  const keys: Record<QualityConclusion, string> = {
    no_significant_anomaly: 'modelHealth.quality.normal',
    observe: 'modelHealth.quality.observe',
    suspected: 'modelHealth.quality.suspected',
    high_risk: 'modelHealth.quality.highRisk',
    insufficient_coverage: 'modelHealth.quality.insufficient'
  }
  return value ? t(keys[value]) : t('modelHealth.quality.insufficient')
}

function qualityClass(value: RadarPublicHealth['overall_conclusion']): string {
  if (value === 'high_risk' || value === 'suspected') return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
  if (value === 'observe' || value === 'insufficient_coverage') return 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
  return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
}

function isValidDate(value?: string): value is string {
  return typeof value === 'string' && !Number.isNaN(new Date(value).valueOf())
}

function formatFreshness(value: string): string {
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'short'
  }).format(new Date(value))
}

onMounted(loadModels)
</script>
