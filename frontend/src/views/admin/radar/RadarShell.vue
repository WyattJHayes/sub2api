<template>
  <AppLayout>
    <section class="space-y-5">
      <header class="flex flex-col gap-3 border-b border-gray-200 pb-4 dark:border-dark-700 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">{{ t('admin.radar.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500">{{ t('admin.radar.description') }}</p>
        </div>
        <button
          class="rounded-md bg-primary-600 px-3 py-2 text-sm font-medium text-white hover:bg-primary-700 disabled:cursor-not-allowed disabled:opacity-60"
          :disabled="store.loading"
          @click="store.refresh()"
        >
          {{ t('admin.radar.actions.refresh') }}
        </button>
      </header>

      <nav class="flex gap-1 overflow-x-auto border-b border-gray-200 dark:border-dark-700">
        <RouterLink
          v-for="tab in tabs"
          :key="tab.section"
          :to="tab.path"
          class="whitespace-nowrap border-b-2 px-3 py-2 text-sm"
          :class="activeSection === tab.section
            ? 'border-primary-600 text-primary-600'
            : 'border-transparent text-gray-500 hover:text-gray-800 dark:hover:text-white'"
        >
          {{ tab.label }}
        </RouterLink>
      </nav>

      <div v-if="store.error" class="rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
        {{ store.error }}
      </div>

      <RouterView />
    </section>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import { useRadarStore, type RadarSection } from '@/stores/radar'

const route = useRoute()
const router = useRouter()
const store = useRadarStore()
const { t } = useI18n()

const tabs = computed(() => [
  { section: 'overview' as const, path: '/admin/radar', label: t('admin.radar.pages.overview') },
  { section: 'models' as const, path: '/admin/radar/models', label: t('admin.radar.pages.models') },
  { section: 'runs' as const, path: '/admin/radar/runs', label: t('admin.radar.pages.runs') },
  { section: 'datasets' as const, path: '/admin/radar/datasets', label: t('admin.radar.pages.datasets') },
])

function normalizeSection(value: unknown): RadarSection | null {
  const raw = Array.isArray(value) ? value[0] : value
  if (
    raw === 'overview' ||
    raw === 'models' ||
    raw === 'runs' ||
    raw === 'datasets'
  ) {
    return raw
  }
  return null
}

function pathForSection(section: RadarSection): string {
  return section === 'overview' ? '/admin/radar' : `/admin/radar/${section}`
}

const activeSection = computed<RadarSection>(() => {
  const routeSection = route.path.startsWith('/admin/radar/')
    ? normalizeSection(route.path.slice('/admin/radar/'.length))
    : null
  return routeSection ?? normalizeSection(route.query.section) ?? 'overview'
})

function canonicalizeLegacyQuery() {
  if (route.query.section == null) return

  const section = normalizeSection(route.query.section) ?? 'overview'
  void router.replace({ path: pathForSection(section) })
}

watch(() => route.query.section, canonicalizeLegacyQuery, { immediate: true })

onMounted(() => {
  if (!store.overview) store.refresh()
})
</script>
