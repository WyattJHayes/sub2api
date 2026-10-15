<template>
  <div class="space-y-5">
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
import { useI18n } from 'vue-i18n'
import type { ModelPlazaResponse } from '@/api/modelPlaza'
import ReferencePriceContent from './ReferencePriceContent.vue'

defineProps<{
  response: ModelPlazaResponse | null
  loading: boolean
  error?: boolean
  embedded?: boolean
  referenceEnabled: boolean
}>()
const { t } = useI18n()
</script>
