<template>
  <section class="overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900">
    <div class="flex flex-col gap-3 border-b border-gray-200 px-4 py-3 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between">
      <div>
        <h2 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.radar.datasets.title') }}</h2>
        <p class="mt-1 text-xs text-gray-500">{{ t('admin.radar.datasets.description') }}</p>
      </div>
      <button data-test="new-dataset" type="button" class="btn btn-primary inline-flex items-center gap-2" @click="showCreate = true">
        <Icon name="plus" size="sm" />
        {{ t('admin.radar.datasets.newDataset') }}
      </button>
    </div>

    <div class="overflow-x-auto">
      <table class="min-w-[920px] w-full text-left text-sm">
        <thead>
          <tr class="border-b border-gray-200 text-xs uppercase text-gray-500 dark:border-dark-700">
            <th class="px-4 py-3">{{ t('admin.radar.datasets.table.dataset') }}</th>
            <th class="px-4 py-3">{{ t('admin.radar.datasets.table.version') }}</th>
            <th class="px-4 py-3">{{ t('admin.radar.datasets.table.cases') }}</th>
            <th class="px-4 py-3">{{ t('admin.radar.datasets.table.source') }}</th>
            <th class="px-4 py-3">{{ t('admin.radar.datasets.table.creator') }}</th>
            <th class="px-4 py-3">{{ t('admin.radar.datasets.table.tenant') }}</th>
            <th class="px-4 py-3">{{ t('admin.radar.table.status') }}</th>
            <th class="px-4 py-3 text-right">{{ t('common.actions') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="dataset in store.datasets" :key="dataset.id" class="border-b border-gray-100 last:border-0 dark:border-dark-800">
            <td class="px-4 py-3 font-medium text-gray-900 dark:text-white">{{ dataset.name ?? dataset.dataset_key ?? dataset.id }}</td>
            <td class="px-4 py-3 text-gray-600 dark:text-dark-200">{{ dataset.version ?? t('common.notAvailable') }}</td>
            <td class="px-4 py-3 text-gray-600 dark:text-dark-200">{{ dataset.cases ?? dataset.case_count ?? 0 }}</td>
            <td class="px-4 py-3 text-gray-600 dark:text-dark-200">{{ dataset.source_type == null ? '-' : sourceTypeLabel(dataset.source_type) }}</td>
            <td class="px-4 py-3 text-gray-600 dark:text-dark-200">{{ dataset.created_by ?? '-' }}</td>
            <td class="px-4 py-3 text-gray-600 dark:text-dark-200">{{ dataset.tenant_id ?? '-' }}</td>
            <td class="px-4 py-3"><Status :value="dataset.status ?? 'draft'" /></td>
            <td class="px-4 py-3 text-right">
              <button
                v-if="dataset.status === 'draft'"
                :data-test="`publish-${dataset.id}`"
                type="button"
                class="btn btn-secondary btn-sm"
                :disabled="publishingId === dataset.id"
                @click="publish(dataset.id)"
              >
                {{ publishingId === dataset.id ? t('admin.radar.datasets.publishing') : t('admin.radar.datasets.publish') }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
      <div v-if="!store.datasets.length" class="p-5 text-sm text-gray-500">{{ t('admin.radar.datasets.empty') }}</div>
    </div>
  </section>

  <BaseDialog :show="showCreate" :title="t('admin.radar.datasets.createDialog.title')" width="wide" @close="closeCreate">
    <form class="space-y-5" @submit.prevent="createDataset">
      <div class="grid gap-4 sm:grid-cols-3">
        <label class="block">
          <span class="input-label">{{ t('admin.radar.datasets.createDialog.datasetKey') }}</span>
          <input v-model.trim="form.datasetKey" data-test="dataset-key" class="input w-full" autocomplete="off" :placeholder="t('admin.radar.datasets.createDialog.datasetKeyPlaceholder')" />
        </label>
        <label class="block">
          <span class="input-label">{{ t('admin.radar.datasets.table.version') }}</span>
          <input v-model.trim="form.version" data-test="dataset-version" class="input w-full" autocomplete="off" :placeholder="t('admin.radar.datasets.createDialog.versionPlaceholder')" />
        </label>
        <label class="block">
          <span class="input-label">{{ t('admin.radar.datasets.createDialog.source') }}</span>
          <select v-model="form.sourceType" class="input w-full">
            <option value="synthetic">{{ sourceTypeLabel('synthetic') }}</option>
            <option value="public">{{ sourceTypeLabel('public') }}</option>
            <option value="imported">{{ sourceTypeLabel('imported') }}</option>
          </select>
        </label>
      </div>

      <div class="border-t border-gray-200 pt-5 dark:border-dark-700">
        <h3 class="mb-4 text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.radar.datasets.createDialog.initialCase') }}</h3>
        <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <label class="block sm:col-span-2">
            <span class="input-label">{{ t('admin.radar.datasets.createDialog.caseKey') }}</span>
            <input v-model.trim="form.caseKey" data-test="case-key" class="input w-full" autocomplete="off" :placeholder="t('admin.radar.datasets.createDialog.caseKeyPlaceholder')" />
          </label>
          <label class="block">
            <span class="input-label">{{ t('admin.radar.datasets.createDialog.capability') }}</span>
            <select v-model="form.capabilityDomain" class="input w-full">
              <option v-for="domain in capabilityDomains" :key="domain" :value="domain">{{ domainLabel(domain) }}</option>
            </select>
          </label>
          <label class="block">
            <span class="input-label">{{ t('admin.radar.datasets.createDialog.priority') }}</span>
            <select v-model="form.priority" class="input w-full">
              <option value="P0">P0</option>
              <option value="P1">P1</option>
              <option value="P2">P2</option>
            </select>
          </label>
          <label class="block sm:col-span-2">
            <span class="input-label">{{ t('admin.radar.datasets.createDialog.prompt') }}</span>
            <textarea v-model.trim="form.prompt" data-test="prompt" rows="4" class="input w-full resize-y" :placeholder="t('admin.radar.datasets.createDialog.promptPlaceholder')" />
          </label>
          <label class="block sm:col-span-2">
            <span class="input-label">{{ t('admin.radar.datasets.createDialog.expectedOutput') }}</span>
            <textarea v-model.trim="form.expectedOutput" data-test="expected-output" rows="4" class="input w-full resize-y" :placeholder="t('admin.radar.datasets.createDialog.expectedOutputPlaceholder')" />
          </label>
          <label class="block">
            <span class="input-label">{{ t('admin.radar.datasets.createDialog.weight') }}</span>
            <input v-model="form.weight" type="number" min="0.01" step="0.01" class="input w-full" />
          </label>
          <label class="block">
            <span class="input-label">{{ t('admin.radar.datasets.createDialog.samples') }}</span>
            <input v-model.number="form.sampleCount" type="number" min="1" max="10" class="input w-full" />
          </label>
          <label class="block">
            <span class="input-label">{{ t('admin.radar.datasets.createDialog.grader') }}</span>
            <select v-model="form.graderId" class="input w-full">
              <option value="exact">{{ graderLabel('exact') }}</option>
              <option value="llm_judge">{{ graderLabel('llm_judge') }}</option>
              <option value="json_schema">{{ graderLabel('json_schema') }}</option>
            </select>
          </label>
          <label class="block">
            <span class="input-label">{{ t('admin.radar.datasets.createDialog.estimatedCost') }}</span>
            <input v-model="form.estimatedCost" type="number" min="0" step="0.01" class="input w-full" />
          </label>
          <label class="block sm:col-span-2">
            <span class="input-label">{{ t('admin.radar.datasets.createDialog.confidentiality') }}</span>
            <select v-model="form.confidentiality" class="input w-full">
              <option value="synthetic">{{ sourceTypeLabel('synthetic') }}</option>
              <option value="public">{{ sourceTypeLabel('public') }}</option>
            </select>
          </label>
          <label class="block sm:col-span-2">
            <span class="input-label">{{ t('admin.radar.datasets.createDialog.gatewayPath') }}</span>
            <input value="/v1/responses" readonly class="input w-full bg-gray-50 font-mono text-xs dark:bg-dark-800" />
          </label>
        </div>
      </div>
    </form>

    <template #footer>
      <button type="button" class="btn btn-secondary" @click="closeCreate">{{ t('common.cancel') }}</button>
      <button data-test="create-dataset" type="button" class="btn btn-primary" :disabled="saving" @click="createDataset">
        {{ saving ? t('admin.radar.datasets.creating') : t('admin.radar.datasets.createDataset') }}
      </button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import radarAdminAPI from '@/api/admin/radar'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore } from '@/stores/app'
import { useRadarStore } from '@/stores/radar'
import Status from './components/Status.vue'

const capabilityDomains = ['coding', 'reasoning', 'instruction', 'long_context', 'tool_call', 'protocol', 'safety', 'performance', 'cost']
const store = useRadarStore()
const appStore = useAppStore()
const { t, te } = useI18n()
const showCreate = ref(false)
const saving = ref(false)
const publishingId = ref<string | null>(null)

const defaults = () => ({
  datasetKey: '',
  version: '',
  sourceType: 'synthetic',
  caseKey: '',
  capabilityDomain: 'reasoning',
  priority: 'P1',
  weight: '1',
  sampleCount: 1,
  prompt: '',
  expectedOutput: '',
  graderId: 'exact',
  confidentiality: 'synthetic',
  estimatedCost: '0.01'
})
const form = reactive(defaults())

function errorMessage(error: unknown, fallback: string): string {
  const value = error as { response?: { data?: { detail?: string } }; message?: string }
  return value.response?.data?.detail ?? value.message ?? fallback
}

function domainLabel(value: string): string {
  const key = `admin.radar.domains.${value}`
  return te(key) ? t(key) : value
}

function sourceTypeLabel(value: string): string {
  const key = `admin.radar.datasets.sourceTypes.${value}`
  return te(key) ? t(key) : value
}

function graderLabel(value: string): string {
  const key = `admin.radar.datasets.graders.${value}`
  return te(key) ? t(key) : value
}

function closeCreate(): void {
  showCreate.value = false
  Object.assign(form, defaults())
}

async function createDataset(): Promise<void> {
  if (!form.datasetKey || !form.version || !form.caseKey || !form.prompt || !form.expectedOutput) {
    appStore.showError(t('admin.radar.messages.datasetFieldsRequired'))
    return
  }
  saving.value = true
  try {
    await radarAdminAPI.createDataset({
      dataset_key: form.datasetKey,
      version: form.version,
      source_type: form.sourceType,
      cases: [{
        case_key: form.caseKey,
        capability_domain: form.capabilityDomain,
        priority: form.priority,
        weight: form.weight,
        sample_count: form.sampleCount,
        prompt_spec: { input: form.prompt },
        expected_spec: { output: form.expectedOutput },
        execution_spec: { url: '/v1/responses' },
        grader_id: form.graderId,
        grader_version: 'v1',
        confidentiality: form.confidentiality,
        estimated_cost: form.estimatedCost
      }]
    })
    closeCreate()
    await store.loadSection('datasets')
    appStore.showSuccess(t('admin.radar.messages.datasetCreated'))
  } catch (error) {
    appStore.showError(errorMessage(error, t('admin.radar.messages.datasetCreateFailed')))
  } finally {
    saving.value = false
  }
}

async function publish(id: string): Promise<void> {
  publishingId.value = id
  try {
    await radarAdminAPI.publishDataset(id)
    await store.loadSection('datasets')
    appStore.showSuccess(t('admin.radar.messages.datasetPublished'))
  } catch (error) {
    appStore.showError(errorMessage(error, t('admin.radar.messages.datasetPublishFailed')))
  } finally {
    publishingId.value = null
  }
}

onMounted(() => store.loadSection('datasets'))
</script>
