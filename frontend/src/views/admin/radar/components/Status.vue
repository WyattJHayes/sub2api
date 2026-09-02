<template>
  <span class="inline-flex rounded-full px-2 py-1 text-xs font-medium" :class="tone">{{ label }}</span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

const props = defineProps<{ value: string }>()
const { t, te } = useI18n()

const normalized = computed(() => props.value || 'insufficient_evidence')
const tone = computed(() => {
  if (['healthy', 'passed', 'active', 'resolved', 'published', 'bound', 'completed'].includes(normalized.value)) {
    return 'bg-emerald-100 text-emerald-700'
  }
  if (['blocked', 'open', 'degraded', 'stale', 'failed', 'cancelled'].includes(normalized.value)) {
    return 'bg-red-100 text-red-700'
  }
  return 'bg-amber-100 text-amber-700'
})
const label = computed(() => {
  const key = `admin.radar.status.${normalized.value}`
  return te(key) ? t(key) : normalized.value
})
</script>
