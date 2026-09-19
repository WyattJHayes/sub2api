<template>
  <section class="overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900">
    <div class="border-b border-gray-200 px-4 py-3 text-sm font-semibold dark:border-dark-700 dark:text-white">{{ t('admin.radar.gates.title') }}</div>
    <div class="divide-y divide-gray-100 dark:divide-dark-800">
      <div v-for="gate in store.gates" :key="gate.id" class="flex flex-wrap items-center justify-between gap-3 px-4 py-3 text-sm">
        <span class="dark:text-white">{{ gate.model_route ?? gate.run_id ?? gate.id }}</span>
        <Status :value="gate.status" />
      </div>
      <div v-if="!store.gates.length" class="p-5 text-sm text-gray-500">{{ t('admin.radar.gates.empty') }}</div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRadarStore } from '@/stores/radar'
import Status from './components/Status.vue'

const store = useRadarStore()
const { t } = useI18n()

onMounted(() => store.loadSection('gates'))
</script>
