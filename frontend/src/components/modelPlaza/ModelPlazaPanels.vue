<template>
  <div class="space-y-5">
    <div v-if="referenceEnabled && descriptionHtml" data-plaza-description class="break-words rounded-2xl border border-gray-100 bg-white px-5 py-4 text-sm leading-relaxed shadow-card dark:border-dark-700/50 dark:bg-dark-800/50 [&_a]:underline [&_ol]:list-decimal [&_ol]:pl-5 [&_ul]:list-disc [&_ul]:pl-5" v-html="descriptionHtml"></div>
    <ReferencePriceContent v-if="referenceEnabled" />
    <div v-else-if="loading" class="flex min-h-[240px] items-center justify-center">
      <div class="h-8 w-8 animate-spin rounded-full border-2 border-primary-600/25 border-t-primary-600 dark:border-primary-400/25 dark:border-t-primary-400"></div>
    </div>
    <div v-else role="alert" class="rounded-2xl border border-red-200 bg-red-50 px-5 py-8 text-center text-sm text-red-600 dark:border-red-500/30 dark:bg-red-500/10 dark:text-red-300">
      {{ t('modelPlaza.loadFailed') }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import type { ModelPlazaResponse } from '@/api/modelPlaza'
import ReferencePriceContent from './ReferencePriceContent.vue'

const props = defineProps<{
  response: ModelPlazaResponse | null
  loading: boolean
  error?: boolean
  embedded?: boolean
  referenceEnabled: boolean
}>()
const { t } = useI18n()
const descriptionHtml = computed(() => {
  const description = props.response?.description?.trim()
  return description ? DOMPurify.sanitize(marked.parse(description) as string) : ''
})
</script>
