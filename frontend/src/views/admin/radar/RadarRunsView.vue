<template>
  <section class="overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900">
    <div class="flex flex-col gap-3 border-b border-gray-200 px-4 py-3 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between">
      <div>
        <h2 class="text-sm font-semibold text-gray-900 dark:text-white">Evaluation runs</h2>
        <p class="mt-1 text-xs text-gray-500">Paired baseline and candidate executions with frozen configuration.</p>
      </div>
      <div class="flex flex-wrap gap-2">
        <button data-test="new-plan" type="button" class="btn btn-secondary inline-flex items-center gap-2" @click="showPlan = true">
          <Icon name="plus" size="sm" />
          New plan
        </button>
        <button data-test="start-run" type="button" class="btn btn-primary inline-flex items-center gap-2" @click="openRun()">
          <Icon name="play" size="sm" />
          Start run
        </button>
      </div>
    </div>

    <div class="overflow-x-auto">
      <table class="min-w-[800px] w-full text-left text-sm">
        <thead>
          <tr class="border-b border-gray-200 text-xs uppercase text-gray-500 dark:border-dark-700">
            <th class="px-4 py-3">Run</th>
            <th class="px-4 py-3">Plan</th>
            <th class="px-4 py-3">Trigger</th>
            <th class="px-4 py-3">Status</th>
            <th class="px-4 py-3">Reserved cost</th>
            <th class="px-4 py-3">Created</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="run in store.runs" :key="run.id" class="border-b border-gray-100 last:border-0 dark:border-dark-800">
            <td class="px-4 py-3 font-mono text-xs text-gray-900 dark:text-white">{{ run.id }}</td>
            <td class="px-4 py-3 font-mono text-xs text-gray-600 dark:text-dark-200">{{ run.plan_id }}</td>
            <td class="px-4 py-3 text-gray-600 dark:text-dark-200">{{ run.trigger_source ?? 'manual' }}</td>
            <td class="px-4 py-3"><Status :value="run.status" /></td>
            <td class="px-4 py-3 text-gray-600 dark:text-dark-200">{{ run.reserved_cost ?? '0' }}</td>
            <td class="px-4 py-3 text-gray-500">{{ formatDate(run.created_at) }}</td>
          </tr>
        </tbody>
      </table>
      <div v-if="!store.runs.length" class="p-5 text-sm text-gray-500">No evaluation runs recorded</div>
    </div>
  </section>

  <BaseDialog :show="showPlan" title="New evaluation plan" width="wide" @close="showPlan = false">
    <form class="space-y-5" @submit.prevent="createPlan">
      <div class="grid gap-4 sm:grid-cols-2">
        <label class="block">
          <span class="input-label">Plan name</span>
          <input v-model.trim="planForm.name" data-test="plan-name" class="input w-full" autocomplete="off" placeholder="DeepSeek regression" />
        </label>
        <label class="block">
          <span class="input-label">Published dataset</span>
          <select v-model="planForm.datasetVersionId" data-test="plan-dataset" class="input w-full">
            <option value="" disabled>Select a dataset</option>
            <option v-for="dataset in publishedDatasets" :key="dataset.id" :value="dataset.id">
              {{ dataset.name ?? dataset.dataset_key ?? dataset.id }} / {{ dataset.version }}
            </option>
          </select>
        </label>
        <label class="block sm:col-span-2">
          <span class="input-label">Gateway API key ID</span>
          <span class="flex gap-2">
            <input v-model.number="planForm.gatewayAPIKeyId" data-test="gateway-api-key" type="number" min="1" class="input min-w-0 flex-1" />
            <button data-test="enable-evaluation-key" type="button" class="btn btn-secondary shrink-0" :disabled="enablingKey" @click="enableKey">
              {{ enablingKey ? 'Enabling...' : 'Enable for Radar' }}
            </button>
          </span>
        </label>
      </div>

      <div class="border-t border-gray-200 pt-5 dark:border-dark-700">
        <h3 class="mb-4 text-sm font-semibold text-gray-900 dark:text-white">Paired model matrix</h3>
        <div class="grid gap-4 sm:grid-cols-3">
          <label class="block">
            <span class="input-label">Logical route</span>
            <input v-model.trim="planForm.modelRoute" data-test="model-route" class="input w-full" placeholder="deepseek-chat" />
          </label>
          <label class="block">
            <span class="input-label">Baseline route</span>
            <input v-model.trim="planForm.baselineRoute" data-test="baseline-route" class="input w-full" placeholder="deepseek-chat-v1" />
          </label>
          <label class="block">
            <span class="input-label">Candidate route</span>
            <input v-model.trim="planForm.candidateRoute" data-test="candidate-route" class="input w-full" placeholder="deepseek-chat-v2" />
          </label>
        </div>
      </div>

      <div class="grid gap-4 border-t border-gray-200 pt-5 dark:border-dark-700 sm:grid-cols-3">
        <label class="block">
          <span class="input-label">Run cost limit</span>
          <input v-model="planForm.maxRunCost" type="number" min="0" step="0.01" class="input w-full" />
        </label>
        <label class="block">
          <span class="input-label">Daily cost limit</span>
          <input v-model="planForm.dailyCostLimit" type="number" min="0" step="0.01" class="input w-full" />
        </label>
        <label class="block">
          <span class="input-label">Max concurrency</span>
          <input v-model.number="planForm.maxConcurrency" type="number" min="1" max="1000" class="input w-full" />
        </label>
      </div>
    </form>
    <template #footer>
      <button type="button" class="btn btn-secondary" @click="showPlan = false">Cancel</button>
      <button data-test="create-plan" type="button" class="btn btn-primary" :disabled="savingPlan" @click="createPlan">
        {{ savingPlan ? 'Creating...' : 'Create plan' }}
      </button>
    </template>
  </BaseDialog>

  <BaseDialog :show="showRun" title="Start evaluation run" @close="showRun = false">
    <form class="space-y-4" @submit.prevent="startRun">
      <label class="block">
        <span class="input-label">Plan ID</span>
        <input v-model.trim="runForm.planId" data-test="run-plan-id" class="input w-full font-mono text-xs" autocomplete="off" placeholder="Plan UUID" />
      </label>
      <label class="block">
        <span class="input-label">Baseline release reference</span>
        <input v-model.trim="runForm.baselineRef" data-test="baseline-ref" class="input w-full" autocomplete="off" placeholder="baseline-2026-07-20" />
      </label>
      <label class="block">
        <span class="input-label">Candidate release reference</span>
        <input v-model.trim="runForm.candidateRef" data-test="candidate-ref" class="input w-full" autocomplete="off" placeholder="candidate-2026-07-27" />
      </label>
    </form>
    <template #footer>
      <button type="button" class="btn btn-secondary" @click="showRun = false">Cancel</button>
      <button data-test="start-run-submit" type="button" class="btn btn-primary" :disabled="startingRun" @click="startRun">
        {{ startingRun ? 'Starting...' : 'Start run' }}
      </button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import radarAdminAPI from '@/api/admin/radar'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore } from '@/stores/app'
import { useRadarStore } from '@/stores/radar'
import Status from './components/Status.vue'

const store = useRadarStore()
const appStore = useAppStore()
const showPlan = ref(false)
const showRun = ref(false)
const savingPlan = ref(false)
const startingRun = ref(false)
const enablingKey = ref(false)
const publishedDatasets = computed(() => store.datasets.filter((dataset) => dataset.status === 'published'))

const planForm = reactive({
  name: '',
  datasetVersionId: '',
  gatewayAPIKeyId: 0,
  modelRoute: '',
  baselineRoute: '',
  candidateRoute: '',
  maxRunCost: '10',
  dailyCostLimit: '50',
  maxConcurrency: 4
})
const runForm = reactive({ planId: '', baselineRef: '', candidateRef: '' })

function errorMessage(error: unknown, fallback: string): string {
  const value = error as { response?: { data?: { detail?: string } }; message?: string }
  return value.response?.data?.detail ?? value.message ?? fallback
}

function formatDate(value?: string): string {
  if (!value) return 'n/a'
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value))
}

function openRun(planId = ''): void {
  runForm.planId = planId
  showRun.value = true
}

async function enableKey(): Promise<void> {
  if (planForm.gatewayAPIKeyId <= 0) {
    appStore.showError('Enter a valid API key ID')
    return
  }
  enablingKey.value = true
  try {
    await radarAdminAPI.enableEvaluationKey(planForm.gatewayAPIKeyId)
    appStore.showSuccess('API key enabled for Radar')
  } catch (error) {
    appStore.showError(errorMessage(error, 'Failed to enable API key'))
  } finally {
    enablingKey.value = false
  }
}

async function createPlan(): Promise<void> {
  if (!planForm.name || !planForm.datasetVersionId || planForm.gatewayAPIKeyId <= 0 || !planForm.modelRoute || !planForm.baselineRoute || !planForm.candidateRoute) {
    appStore.showError('Complete all required plan fields')
    return
  }
  savingPlan.value = true
  try {
    const plan = await radarAdminAPI.createPlan({
      name: planForm.name,
      dataset_version_id: planForm.datasetVersionId,
      gateway_api_key_id: planForm.gatewayAPIKeyId,
      trigger_type: 'manual',
      model_matrix: [{
        route: planForm.modelRoute,
        baseline: { route: planForm.baselineRoute, temperature: 0, max_tokens: 256 },
        candidate: { route: planForm.candidateRoute, temperature: 0, max_tokens: 256 }
      }],
      max_run_cost: planForm.maxRunCost,
      daily_cost_limit: planForm.dailyCostLimit,
      max_concurrency: planForm.maxConcurrency
    })
    showPlan.value = false
    appStore.showSuccess('Evaluation plan created')
    openRun(plan.id)
  } catch (error) {
    appStore.showError(errorMessage(error, 'Failed to create evaluation plan'))
  } finally {
    savingPlan.value = false
  }
}

async function startRun(): Promise<void> {
  if (!runForm.planId || !runForm.baselineRef || !runForm.candidateRef) {
    appStore.showError('Complete all run references')
    return
  }
  startingRun.value = true
  try {
    await radarAdminAPI.startRun({
      plan_id: runForm.planId,
      trigger_source: 'manual',
      baseline_ref: { release: runForm.baselineRef },
      candidate_ref: { release: runForm.candidateRef }
    })
    showRun.value = false
    await store.loadSection('runs')
    appStore.showSuccess('Evaluation run started')
  } catch (error) {
    appStore.showError(errorMessage(error, 'Failed to start evaluation run'))
  } finally {
    startingRun.value = false
  }
}

onMounted(() => Promise.all([store.loadSection('runs'), store.loadSection('datasets')]))
</script>
