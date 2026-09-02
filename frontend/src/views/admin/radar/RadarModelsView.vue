<template>
  <section class="overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900">
    <div class="flex flex-col gap-4 border-b border-gray-200 px-4 py-4 dark:border-dark-700 lg:flex-row lg:items-center lg:justify-between">
      <div>
        <h2 class="text-sm font-semibold dark:text-white">{{ t('admin.radar.models.title') }}</h2>
        <p class="mt-1 text-sm text-gray-500">{{ t('admin.radar.models.description') }}</p>
      </div>

      <form class="flex w-full flex-col gap-2 sm:flex-row lg:w-auto" @submit.prevent="registerModel">
        <label class="sr-only" for="radar-model-alias">{{ t('admin.radar.models.alias') }}</label>
        <input
          id="radar-model-alias"
          v-model="modelAlias"
          name="model-alias"
          type="text"
          autocomplete="off"
          :placeholder="t('admin.radar.models.aliasPlaceholder')"
          class="min-w-0 rounded-md border border-gray-300 bg-white px-3 py-2 text-sm text-gray-900 outline-none ring-primary-500 focus:ring-2 dark:border-dark-600 dark:bg-dark-800 dark:text-white sm:w-64"
        >
        <button
          type="submit"
          :disabled="submitting"
          class="rounded-md bg-primary-600 px-3 py-2 text-sm font-medium text-white hover:bg-primary-700 disabled:cursor-not-allowed disabled:opacity-60"
        >
          {{ submitting ? t('admin.radar.actions.addingModel') : t('admin.radar.actions.addModel') }}
        </button>
      </form>
    </div>

    <p
      v-if="feedback"
      class="border-b px-4 py-3 text-sm"
      :class="feedback.kind === 'success'
        ? 'border-emerald-200 bg-emerald-50 text-emerald-700 dark:border-emerald-900/60 dark:bg-emerald-900/20 dark:text-emerald-300'
        : 'border-red-200 bg-red-50 text-red-700 dark:border-red-900/60 dark:bg-red-900/20 dark:text-red-300'"
    >
      {{ feedback.message }}
    </p>

    <RadarTable :rows="trackedModels" :show-actions="true">
      <template #actions="{ row }">
        <button
          v-if="row.model_alias"
          :data-test="`untrack-model-${row.model_route}`"
          type="button"
          class="inline-flex h-8 w-8 items-center justify-center rounded-md text-red-600 hover:bg-red-50 disabled:cursor-not-allowed disabled:opacity-60 dark:text-red-300 dark:hover:bg-red-900/20"
          :title="t('admin.radar.actions.untrackHint')"
          :aria-label="t('admin.radar.actions.untrack')"
          :disabled="untrackingAlias === row.model_alias"
          @click="untrackModel(row.model_alias)"
        >
          <Icon name="trash" size="sm" />
        </button>
      </template>
    </RadarTable>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import radarAdminAPI from '@/api/admin/radar'
import Icon from '@/components/icons/Icon.vue'
import { useRadarStore } from '@/stores/radar'
import RadarTable from './components/RadarTable.vue'

const store = useRadarStore()
const { t } = useI18n()
const modelAlias = ref('')
const submitting = ref(false)
const untrackingAlias = ref<string | null>(null)
const feedback = ref<{ kind: 'success' | 'error'; message: string } | null>(null)
const trackedModels = computed(() => store.models.filter((row) => Boolean(row.model_alias)))

async function registerModel() {
  const alias = modelAlias.value.trim()
  if (!alias) {
    feedback.value = { kind: 'error', message: t('admin.radar.messages.modelAliasRequired') }
    return
  }

  submitting.value = true
  feedback.value = null
  try {
    await radarAdminAPI.registerModel({ model_alias: alias })
    modelAlias.value = ''
    await store.loadSection('models')
    feedback.value = { kind: 'success', message: t('admin.radar.messages.modelAdded') }
  } catch {
    feedback.value = { kind: 'error', message: t('admin.radar.messages.modelAddFailed') }
  } finally {
    submitting.value = false
  }
}

async function untrackModel(alias: string) {
  if (!window.confirm(t('admin.radar.actions.untrackConfirm'))) return
  untrackingAlias.value = alias
  feedback.value = null
  try {
    await radarAdminAPI.untrackModel(alias)
    await store.loadSection('models')
    feedback.value = { kind: 'success', message: t('admin.radar.messages.modelUntracked') }
  } catch {
    feedback.value = { kind: 'error', message: t('admin.radar.messages.modelUntrackFailed') }
  } finally {
    untrackingAlias.value = null
  }
}

onMounted(() => store.loadSection('models'))
</script>
