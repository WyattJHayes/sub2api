<template>
  <section class="overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900">
    <div class="border-b border-gray-200 px-4 py-3 text-sm font-semibold dark:border-dark-700 dark:text-white">{{ t('admin.radar.workers.title') }}</div>
    <div class="grid gap-3 p-4 md:grid-cols-2 xl:grid-cols-3">
      <div v-for="worker in store.workers" :key="worker.id" class="rounded-md border border-gray-200 p-4 dark:border-dark-700">
        <div class="flex items-center justify-between">
          <strong class="dark:text-white">{{ worker.name }}</strong>
          <Status :value="worker.status" />
        </div>
        <p class="mt-2 text-sm text-gray-500">{{ workerKindLabel(worker.worker_kind) }}</p>
        <p class="mt-1 text-xs text-gray-400">{{ t('admin.radar.workers.heartbeat', { time: formatDate(worker.last_heartbeat_at) }) }}</p>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRadarStore } from '@/stores/radar'
import Status from './components/Status.vue'

const store = useRadarStore()
const { t, te, locale } = useI18n()

function workerKindLabel(value?: string): string {
  const key = `admin.radar.workers.kinds.${value}`
  return value && te(key) ? t(key) : value ?? t('common.unknown')
}

function formatDate(value?: string): string {
  if (!value) return t('common.unknown')
  return new Intl.DateTimeFormat(locale.value, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value))
}

onMounted(() => store.loadSection('workers'))
</script>
