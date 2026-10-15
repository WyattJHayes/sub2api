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
      <article v-for="model in filteredModels" :key="referenceModelKey(model.id)" class="min-w-0 rounded-2xl border border-gray-200 bg-white p-4 shadow-sm dark:border-dark-700 dark:bg-dark-800">
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0">
            <span class="text-xs font-medium text-primary-600 dark:text-primary-300">{{ model.providers.map(providerName).join(' / ') }}</span>
            <h3 class="mt-1 break-all font-mono text-base font-semibold text-gray-900 dark:text-white">{{ model.id }}</h3>
          </div>
          <button type="button" class="shrink-0 rounded-lg border border-gray-200 px-2 py-1 text-xs text-gray-600 hover:bg-gray-50 dark:border-dark-600 dark:text-dark-200 dark:hover:bg-dark-700" :aria-label="t('modelPlaza.reference.copyModel', { model: model.id })" @click="copyModel(model.id)">{{ t('modelPlaza.reference.copy') }}</button>
        </div>
        <p class="mt-3 text-xs text-gray-500 dark:text-dark-400">{{ t(`modelPlaza.reference.${model.status === 'official' ? 'officialBase' : model.status === 'reference' ? 'referenceBase' : 'unverified'}`) }}<span v-if="model.status !== 'unverified'"> · {{ model.currency }} / 1M tokens</span><span v-if="model.region"> · {{ t(`modelPlaza.reference.${model.region}`) }}</span></p>
        <p v-if="model.note" class="mt-2 text-xs leading-relaxed text-gray-500 dark:text-dark-400">{{ t(`modelPlaza.reference.${model.note}`) }}</p>
        <div class="mt-4 overflow-x-auto" role="region" :aria-label="t('modelPlaza.reference.pricesFor', { model: model.id })" tabindex="0">
          <table class="w-full text-left text-xs tabular-nums">
            <caption class="sr-only">{{ t('modelPlaza.reference.unit') }}</caption>
            <thead class="border-b border-gray-100 text-gray-500 dark:border-dark-700 dark:text-dark-400">
              <tr>
                <th scope="col" class="whitespace-nowrap px-2 py-2">{{ t('modelPlaza.reference.context') }}</th>
                <th v-for="column in columnsFor(model)" :key="column" scope="col" class="whitespace-nowrap px-2 py-2 text-right">{{ columnLabel(model, column) }}</th>
              </tr>
            </thead>
            <tbody class="text-gray-700 dark:text-dark-200">
              <tr v-for="tier in model.tiers" :key="tier.context" class="border-b border-gray-100 last:border-0 dark:border-dark-700">
                <th scope="row" class="whitespace-nowrap px-2 py-3 font-normal">{{ contextLabel(tier.context) }}</th>
                <td v-for="column in columnsFor(model)" :key="column" class="whitespace-nowrap px-2 py-3 text-right">{{ formatPrice(tier[column], model.currency) }}</td>
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
import { platformDisplayName } from '@/constants/platformCatalog'
import { referenceModels, referenceModelKey, findReferenceModel, type ReferenceModel, type ReferenceTier } from '@/data/modelReferencePrices'
import type { ModelPlazaGroup } from '@/api/modelPlaza'
import { formatReferencePrice, referencePriceColumns } from './referencePriceQuotes'

const props = defineProps<{ groups: ModelPlazaGroup[] }>()

const { t } = useI18n()
const { copyToClipboard } = useClipboard()
const search = ref('')
const provider = ref('all')
const copyStatus = ref('')
type CatalogModel = ReferenceModel & { providers: string[] }
const catalog = computed<CatalogModel[]>(() => {
  const models = new Map<string, CatalogModel>(referenceModels.map(m => [referenceModelKey(m.id), { ...m, providers: [m.provider] }]))
  for (const group of props.groups) for (const entry of group.models) {
    const key = referenceModelKey(entry.name)
    const existing = models.get(key)
    if (existing) {
      if (existing.status === 'unverified' && !existing.providers.includes(entry.platform)) {
        existing.providers.push(entry.platform)
        existing.providers.sort()
      }
      continue
    }
    const model = findReferenceModel(entry.name) ?? {
      id: entry.name, provider: entry.platform, currency: 'USD', status: 'unverified', source: '',
      tiers: [{ context: 'all', input: null, output: null, cacheRead: null, cacheWrite: null, cacheWrite1h: null, explicitCacheRead: null }]
    }
    models.set(key, { ...model, providers: [model.provider] })
  }
  return [...models.values()]
})
const providers = computed(() => [...new Set(catalog.value.flatMap(model => model.providers))])
const providerKeys: Record<string, string> = { GLM: 'modelPlaza.reference.glm', Qwen: 'modelPlaza.reference.qwen', Doubao: 'modelPlaza.reference.doubao' }
const providerName = (name: string) => providerKeys[name] ? t(providerKeys[name]!) : platformDisplayName(name)
const filteredModels = computed(() => {
  const query = search.value.trim().toLowerCase()
  return catalog.value.filter((model) =>
    (provider.value === 'all' || model.providers.includes(provider.value)) &&
    (!query || `${model.id} ${model.providers.join(' ')} ${model.providers.map(providerName).join(' ')}`.toLowerCase().includes(query))
  )
})

function columnsFor(model: ReferenceModel, tiers: ReferenceTier[] = model.tiers) { return referencePriceColumns.filter(column => column === 'input' || column === 'output' || column === 'cacheRead' || column === 'cacheWrite' || tiers.some(tier => tier[column] !== null)) }
function columnLabel(model: ReferenceModel, column: typeof referencePriceColumns[number]) {
  if (column === 'cacheRead' && model.tiers.some(tier => tier.explicitCacheRead !== null)) return t('modelPlaza.reference.implicitCacheRead')
  return t(`modelPlaza.reference.${column}`)
}
function contextLabel(context: string) { return context === 'all' ? t('modelPlaza.filters.all') : context === 'peak' || context === 'offPeak' ? t(`modelPlaza.reference.${context}`) : context }
function formatPrice(value: number | null, currency: ReferenceModel['currency']) { return formatReferencePrice(value, currency) ?? t('modelPlaza.reference.notProvided') }

async function copyModel(id: string) {
  const success = await copyToClipboard(id)
  copyStatus.value = success ? t('modelPlaza.reference.copied', { model: id }) : t('common.copyFailed')
}
</script>
