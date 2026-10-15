<template>
  <section class="space-y-5" :aria-label="t('modelPlaza.reference.title')">
    <div class="rounded-2xl border border-primary-200 bg-primary-50 p-5 text-sm dark:border-primary-700 dark:bg-primary-950/30">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('modelPlaza.reference.title') }}</h2>
      <p class="mt-2 text-gray-700 dark:text-dark-200">{{ t('modelPlaza.reference.disclaimer') }}</p>
      <p class="mt-2 font-medium text-gray-700 dark:text-dark-200">{{ t('modelPlaza.reference.unit') }}</p>
    </div>

    <div class="flex flex-col gap-3 sm:flex-row">
      <label class="flex-1 text-sm text-gray-600 dark:text-dark-300">
        {{ t('modelPlaza.reference.search') }}
        <input v-model="search" type="search" maxlength="200" :placeholder="t('modelPlaza.reference.search')" class="input mt-1 w-full" />
      </label>
      <label class="text-sm text-gray-600 dark:text-dark-300 sm:w-48">
        {{ t('modelPlaza.reference.provider') }}
        <select v-model="provider" class="input mt-1 w-full">
          <option value="all">{{ t('modelPlaza.filters.all') }}</option>
          <option v-for="item in providers" :key="item" :value="item">{{ providerName(item) }}</option>
        </select>
      </label>
    </div>
    <p class="text-sm text-gray-500 dark:text-dark-400" aria-live="polite">{{ t('modelPlaza.reference.count', { count: filteredModels.length }) }}</p>

    <div class="grid min-w-0 gap-4 lg:grid-cols-2">
      <article v-for="model in filteredModels" :key="model.id" class="min-w-0 rounded-2xl border border-gray-200 bg-white p-4 shadow-sm dark:border-dark-700 dark:bg-dark-800">
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0">
            <span class="text-xs font-medium text-primary-600 dark:text-primary-300">{{ providerName(model.provider) }}</span>
            <h3 class="mt-1 break-all font-mono text-base font-semibold text-gray-900 dark:text-white">{{ model.id }}</h3>
          </div>
          <button type="button" class="shrink-0 rounded-lg border border-gray-200 px-2 py-1 text-xs text-gray-600 hover:bg-gray-50 dark:border-dark-600 dark:text-dark-200 dark:hover:bg-dark-700" :aria-label="t('modelPlaza.reference.copyModel', { model: model.id })" @click="copyModel(model.id)">{{ t('modelPlaza.reference.copy') }}</button>
        </div>
        <p v-if="model.note === 'wholeRequest'" class="mt-3 text-xs leading-relaxed text-gray-500 dark:text-dark-400">{{ t('modelPlaza.reference.wholeRequest') }}</p>
        <div class="mt-4 overflow-x-auto" role="region" :aria-label="t('modelPlaza.reference.pricesFor', { model: model.id })" tabindex="0">
          <table class="w-full text-left text-xs tabular-nums">
            <caption class="sr-only">{{ t('modelPlaza.reference.unit') }}</caption>
            <thead class="border-b border-gray-100 text-gray-500 dark:border-dark-700 dark:text-dark-400">
              <tr>
                <th scope="col" class="whitespace-nowrap px-2 py-2">{{ t('modelPlaza.reference.context') }}</th>
                <th v-for="column in priceColumns" :key="column" scope="col" class="whitespace-nowrap px-2 py-2 text-right">{{ t(`modelPlaza.reference.${column}`) }}</th>
              </tr>
            </thead>
            <tbody class="text-gray-700 dark:text-dark-200">
              <tr v-for="tier in model.tiers" :key="tier.context" class="border-b border-gray-100 last:border-0 dark:border-dark-700">
                <th scope="row" class="whitespace-nowrap px-2 py-3 font-normal">{{ tier.context === 'all' ? t('modelPlaza.filters.all') : tier.context }}</th>
                <td v-for="column in priceColumns" :key="column" class="whitespace-nowrap px-2 py-3 text-right">{{ formatPrice(tier[column]) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </article>
    </div>
    <p v-if="!filteredModels.length" class="rounded-2xl border border-dashed border-gray-300 p-10 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-dark-400">{{ t('modelPlaza.noSearchResult') }}</p>
    <p role="status" class="text-sm text-gray-600 dark:text-dark-300">{{ copyStatus }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useClipboard } from '@/composables/useClipboard'
import { referenceModels } from '@/data/modelReferencePrices'

const { t } = useI18n()
const { copyToClipboard } = useClipboard()
const search = ref('')
const provider = ref('all')
const copyStatus = ref('')
const priceColumns = ['input', 'output', 'cacheRead', 'cacheWrite'] as const
const providers = [...new Set(referenceModels.map((model) => model.provider))]
const providerKeys: Record<string, string> = { GLM: 'modelPlaza.reference.glm', Qwen: 'modelPlaza.reference.qwen', Doubao: 'modelPlaza.reference.doubao' }
const providerName = (name: string) => providerKeys[name] ? t(providerKeys[name]!) : name
const filteredModels = computed(() => {
  const query = search.value.trim().toLowerCase()
  return referenceModels.filter((model) =>
    (provider.value === 'all' || provider.value === model.provider) &&
    (!query || `${model.id} ${model.provider} ${providerName(model.provider)}`.toLowerCase().includes(query))
  )
})

// Snapshot values already use USD / 1M tokens; no billing or group rate applies.
const dollarFormat = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 5 })
function formatPrice(value: number | null) {
  return value === null ? t('modelPlaza.reference.notProvided') : dollarFormat.format(value)
}

async function copyModel(id: string) {
  const success = await copyToClipboard(id)
  copyStatus.value = success ? t('modelPlaza.reference.copied', { model: id }) : t('common.copyFailed')
}
</script>
