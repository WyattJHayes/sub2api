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
      <label class="text-sm text-gray-600 dark:text-dark-300 sm:w-56">
        {{ t('modelPlaza.filters.groupLabel') }}
        <select v-model="groupFilter" data-group-filter class="input mt-1 w-full">
          <option value="all">{{ t('modelPlaza.filters.all') }}</option>
          <option v-for="group in groups" :key="group.id" :value="String(group.id)">{{ group.name }}</option>
        </select>
      </label>
      <label v-if="effortOptions.length" class="text-sm text-gray-600 dark:text-dark-300 sm:w-40">
        {{ t('modelPlaza.reference.effort') }}
        <select v-model="effort" data-effort class="input mt-1 w-full">
          <option value="default">{{ t('modelPlaza.reference.standard') }}</option>
          <option v-for="option in effortOptions" :key="option" :value="option">{{ option }}</option>
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
        <div v-for="quote in quotesFor(model)" :key="quote.group.id" :data-group-quote="quote.group.id" class="mt-4 min-w-0 rounded-xl border border-primary-100 bg-primary-50/40 p-3 dark:border-dark-600 dark:bg-dark-900/30">
          <h4 class="break-words text-sm font-medium text-gray-900 dark:text-white">{{ quote.group.name }}</h4>
          <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ t('modelPlaza.reference.groupRate', { rate: quote.group.user_rate_multiplier ?? quote.group.rate_multiplier }) }}<span v-if="quote.model.group_pricing_override"> · {{ t('modelPlaza.reference.modelAdjustment') }}</span><span v-if="effort !== 'default'"> · {{ effort }} ×{{ effortRate(quote.model) }}</span></p>
          <div v-if="quote.model.time_pricing || hasPeak(quote.group)" class="mt-3 space-y-2 text-xs text-gray-600 dark:text-dark-300">
            <p>{{ t('modelPlaza.reference.timeConditions') }}</p>
            <label v-if="quote.model.time_pricing" class="block">
              {{ t('modelPlaza.reference.timePeriod') }} · {{ quote.model.time_pricing.timezone }}<span v-if="quote.model.time_pricing.weekdays_only"> · {{ t('modelPlaza.reference.weekdaysOnly') }}</span>
              <select :value="timeSelections[quoteKey(quote.group, quote.model)] ?? 'standard'" data-time-period class="input mt-1 w-full text-xs" @change="timeSelections[quoteKey(quote.group, quote.model)] = ($event.target as HTMLSelectElement).value">
                <option value="standard">{{ t('modelPlaza.reference.standardTime') }}</option>
                <option v-for="(period, index) in quote.model.time_pricing.periods" :key="index" :value="String(index)">{{ period.start_time }}–{{ period.end_time }} ×{{ period.multiplier }}</option>
              </select>
            </label>
            <label v-if="hasPeak(quote.group)" class="block">
              {{ t('modelPlaza.reference.peakPeriod') }}
              <select :value="peakSelections[quote.group.id] ?? 'standard'" data-peak-period class="input mt-1 w-full text-xs" @change="peakSelections[quote.group.id] = ($event.target as HTMLSelectElement).value">
                <option value="standard">{{ t('modelPlaza.reference.offPeak') }} ×1</option>
                <option value="peak">{{ quote.group.peak_start }}–{{ quote.group.peak_end }} ×{{ quote.group.peak_rate_multiplier }}</option>
              </select>
            </label>
          </div>
          <div v-if="quote.tiers" class="mt-2 overflow-x-auto" role="region" :aria-label="`${quote.group.name} ${model.id}`" tabindex="0">
            <table class="w-full text-left text-xs tabular-nums">
              <caption class="sr-only">{{ t('modelPlaza.reference.calculatedPrice') }} · {{ model.currency }} / 1M tokens</caption>
              <thead class="text-gray-500 dark:text-dark-400"><tr>
                <th scope="col" class="whitespace-nowrap px-2 py-2">{{ t('modelPlaza.reference.context') }}</th>
                <th v-for="column in columnsFor(model, quote.tiers)" :key="column" scope="col" class="whitespace-nowrap px-2 py-2 text-right">{{ columnLabel(model, column) }}</th>
              </tr></thead>
              <tbody class="text-gray-800 dark:text-dark-100"><tr v-for="tier in quote.tiers" :key="tier.context">
                <th scope="row" class="whitespace-nowrap px-2 py-2 font-normal">{{ contextLabel(tier.context) }}</th>
                <td v-for="column in columnsFor(model, quote.tiers)" :key="column" class="whitespace-nowrap px-2 py-2 text-right">{{ formatPrice(tier[column], model.currency) }}</td>
              </tr></tbody>
            </table>
          </div>
          <p v-else class="mt-2 text-xs text-amber-700 dark:text-amber-300">{{ t('modelPlaza.reference.quoteUnavailable') }}</p>
          <p v-if="!quote.group.long_context_pricing_enabled && model.tiers.length > 1 && model.note !== 'deepseekHours'" class="mt-2 text-xs text-gray-500">{{ t('modelPlaza.detail.longContextDisabledNote') }}</p>
        </div>
        <p v-if="!quotesFor(model).length" class="mt-3 text-xs text-gray-500 dark:text-dark-400">{{ t('modelPlaza.reference.noGroup') }}</p>
      </article>
    </div>
    <p v-if="!filteredModels.length" class="rounded-2xl border border-dashed border-gray-300 p-10 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-dark-400">{{ t('modelPlaza.noSearchResult') }}</p>
    <p role="status" class="text-sm text-gray-600 dark:text-dark-300">{{ copyStatus }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useClipboard } from '@/composables/useClipboard'
import { referenceModels, referenceModelKey, findReferenceModel, type ReferenceModel, type ReferenceTier } from '@/data/modelReferencePrices'
import type { ModelPlazaGroup, PlazaModel } from '@/api/modelPlaza'
import { calculateGroupTiers, formatReferencePrice, referencePriceColumns } from './referencePriceQuotes'

const props = defineProps<{ groups: ModelPlazaGroup[] }>()

const { t } = useI18n()
const { copyToClipboard } = useClipboard()
const search = ref('')
const provider = ref('all')
const groupFilter = ref('all')
const effort = ref('default')
const copyStatus = ref('')
const timeSelections = ref<Record<string, string>>({})
const peakSelections = ref<Record<number, string>>({})
watch(() => props.groups, () => { timeSelections.value = {}; peakSelections.value = {} })
const catalog = computed(() => {
  const models = new Map(referenceModels.map(m => [referenceModelKey(m.id), m]))
  for (const group of props.groups) for (const entry of group.models) {
    const key = referenceModelKey(entry.name)
    if (!models.has(key)) models.set(key, findReferenceModel(entry.name) ?? {
      id: entry.name, provider: entry.platform, currency: 'USD', status: 'unverified', source: '',
      tiers: [{ context: 'all', input: null, output: null, cacheRead: null, cacheWrite: null, cacheWrite1h: null, explicitCacheRead: null }]
    })
  }
  return [...models.values()]
})
const providers = computed(() => [...new Set(catalog.value.map(model => model.provider))])
const effortOptions = computed(() => [...new Set(props.groups.flatMap(g => g.models.flatMap(m => Object.keys(m.group_pricing_override?.reasoning_effort_multipliers ?? m.pricing?.reasoning_effort_multipliers ?? {}))))].sort())
const providerKeys: Record<string, string> = { GLM: 'modelPlaza.reference.glm', Qwen: 'modelPlaza.reference.qwen', Doubao: 'modelPlaza.reference.doubao' }
const providerName = (name: string) => providerKeys[name] ? t(providerKeys[name]!) : name
const filteredModels = computed(() => {
  const query = search.value.trim().toLowerCase()
  return catalog.value.filter((model) =>
    (provider.value === 'all' || provider.value === model.provider) &&
    (groupFilter.value === 'all' || quotesFor(model).length > 0) &&
    (!query || `${model.id} ${model.provider} ${providerName(model.provider)}`.toLowerCase().includes(query))
  )
})

function quotesFor(model: ReferenceModel) {
  return props.groups.filter(g => groupFilter.value === 'all' || String(g.id) === groupFilter.value).flatMap(group => {
    const entry = group.models.find(m => referenceModelKey(m.name) === referenceModelKey(model.id))
    return entry ? [{ group, model: entry, tiers: calculateGroupTiers(model, group, entry, effort.value, selectedTimeRate(group, entry)) }] : []
  })
}
function quoteKey(group: ModelPlazaGroup, model: PlazaModel) { return `${group.id}:${model.platform}:${model.name}` }
function hasPeak(group: ModelPlazaGroup) { return group.peak_rate_enabled && group.subscription_type === 'subscription' }
function selectedTimeRate(group: ModelPlazaGroup, model: PlazaModel) {
  const key = quoteKey(group, model)
  // Preview explicit conditions; never infer the user's billing time.
  const selected = timeSelections.value[key] ?? 'standard'
  const period = selected === 'standard' ? undefined : model.time_pricing?.periods[Number(selected)]
  const peak = hasPeak(group) && peakSelections.value[group.id] === 'peak' ? group.peak_rate_multiplier : 1
  return (period?.multiplier ?? 1) * peak
}
function effortRate(model: PlazaModel) { return (model.group_pricing_override?.reasoning_effort_multipliers ?? model.pricing?.reasoning_effort_multipliers)?.[effort.value] ?? 1 }
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
