import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import RadarDatasetsView from '../RadarDatasetsView.vue'
import RadarRunsView from '../RadarRunsView.vue'

const { radarAPI, showError, showSuccess } = vi.hoisted(() => ({
  radarAPI: {
    overview: vi.fn(),
    models: vi.fn(),
    runs: vi.fn(),
    alerts: vi.fn(),
    gates: vi.fn(),
    workers: vi.fn(),
    datasets: vi.fn(),
    createDataset: vi.fn(),
    publishDataset: vi.fn(),
    createPlan: vi.fn(),
    enableEvaluationKey: vi.fn(),
    startRun: vi.fn(),
    evaluateGate: vi.fn()
  },
  showError: vi.fn(),
  showSuccess: vi.fn()
}))

vi.mock('@/api/admin/radar', () => ({ default: radarAPI }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError, showSuccess }) }))
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()

  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => ({
        'admin.radar.messages.datasetPublished': '数据集已发布',
        'admin.radar.messages.runStarted': '评测运行已启动'
      }[key] ?? key),
      te: () => true,
      locale: { value: 'zh' }
    })
  }
})

const BaseDialogStub = {
  props: ['show', 'title'],
  emits: ['close'],
  template: '<div v-if="show" role="dialog"><h2>{{ title }}</h2><slot /><slot name="footer" /></div>'
}

function mountView(component: typeof RadarDatasetsView | typeof RadarRunsView) {
  return mount(component, {
    global: {
      plugins: [createPinia()],
      stubs: {
        BaseDialog: BaseDialogStub,
        Icon: { template: '<span />' },
        Status: { props: ['value'], template: '<span>{{ value }}</span>' }
      }
    }
  })
}

describe('Radar management views', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    radarAPI.datasets.mockResolvedValue([])
    radarAPI.runs.mockResolvedValue([])
  })

  it('creates a controlled dataset and publishes a draft version', async () => {
    radarAPI.datasets
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([{ id: 'dataset-1', name: 'reasoning-smoke', version: '2026-07-27', status: 'draft' }])
    radarAPI.createDataset.mockResolvedValue({ id: 'dataset-1', status: 'draft' })
    radarAPI.publishDataset.mockResolvedValue({ id: 'dataset-1', status: 'published' })
    const wrapper = mountView(RadarDatasetsView)
    await flushPromises()

    await wrapper.get('[data-test="new-dataset"]').trigger('click')
    await wrapper.get('[data-test="dataset-key"]').setValue('reasoning-smoke')
    await wrapper.get('[data-test="dataset-version"]').setValue('2026-07-27')
    await wrapper.get('[data-test="case-key"]').setValue('addition-1')
    await wrapper.get('[data-test="prompt"]').setValue('Return only the number 4')
    await wrapper.get('[data-test="expected-output"]').setValue('4')
    await wrapper.get('[data-test="create-dataset"]').trigger('click')
    await flushPromises()

    expect(radarAPI.createDataset).toHaveBeenCalledWith({
      dataset_key: 'reasoning-smoke',
      version: '2026-07-27',
      source_type: 'synthetic',
      cases: [{
        case_key: 'addition-1',
        capability_domain: 'reasoning',
        priority: 'P1',
        weight: '1',
        sample_count: 1,
        prompt_spec: { input: 'Return only the number 4' },
        expected_spec: { output: '4' },
        execution_spec: { url: '/v1/responses' },
        grader_id: 'exact',
        grader_version: 'v1',
        confidentiality: 'synthetic',
        estimated_cost: '0.01'
      }]
    })

    await wrapper.get('[data-test="publish-dataset-1"]').trigger('click')
    await flushPromises()

    expect(radarAPI.publishDataset).toHaveBeenCalledWith('dataset-1')
    expect(showSuccess).toHaveBeenCalledWith('数据集已发布')
  })

  it('enables an evaluation key, creates a paired plan and starts its run', async () => {
    radarAPI.datasets.mockResolvedValue([{ id: 'dataset-1', name: 'reasoning-smoke', version: '2026-07-27', status: 'published' }])
    radarAPI.createPlan.mockResolvedValue({ id: 'plan-1' })
    radarAPI.enableEvaluationKey.mockResolvedValue({ id: 42, is_evaluation: true })
    radarAPI.startRun.mockResolvedValue({ id: 'run-1', plan_id: 'plan-1', status: 'pending' })
    const wrapper = mountView(RadarRunsView)
    await flushPromises()

    await wrapper.get('[data-test="new-plan"]').trigger('click')
    await wrapper.get('[data-test="plan-name"]').setValue('DeepSeek regression')
    await wrapper.get('[data-test="plan-dataset"]').setValue('dataset-1')
    await wrapper.get('[data-test="gateway-api-key"]').setValue('42')
    await wrapper.get('[data-test="enable-evaluation-key"]').trigger('click')
    await wrapper.get('[data-test="model-route"]').setValue('deepseek-chat')
    await wrapper.get('[data-test="baseline-route"]').setValue('deepseek-chat-v1')
    await wrapper.get('[data-test="candidate-route"]').setValue('deepseek-chat-v2')
    await wrapper.get('[data-test="create-plan"]').trigger('click')
    await flushPromises()

    expect(radarAPI.enableEvaluationKey).toHaveBeenCalledWith(42)
    expect(radarAPI.createPlan).toHaveBeenCalledWith({
      name: 'DeepSeek regression',
      dataset_version_id: 'dataset-1',
      gateway_api_key_id: 42,
      trigger_type: 'manual',
      model_matrix: [{
        route: 'deepseek-chat',
        baseline: { route: 'deepseek-chat-v1', temperature: 0, max_tokens: 256 },
        candidate: { route: 'deepseek-chat-v2', temperature: 0, max_tokens: 256 }
      }],
      max_run_cost: '10',
      daily_cost_limit: '50',
      max_concurrency: 4
    })

    expect((wrapper.get('[data-test="run-plan-id"]').element as HTMLInputElement).value).toBe('plan-1')
    await wrapper.get('[data-test="baseline-ref"]').setValue('baseline-2026-07-20')
    await wrapper.get('[data-test="candidate-ref"]').setValue('candidate-2026-07-27')
    await wrapper.get('[data-test="start-run-submit"]').trigger('click')
    await flushPromises()

    expect(radarAPI.startRun).toHaveBeenCalledWith({
      plan_id: 'plan-1',
      trigger_source: 'manual',
      baseline_ref: { release: 'baseline-2026-07-20' },
      candidate_ref: { release: 'candidate-2026-07-27' }
    })
    expect(showSuccess).toHaveBeenCalledWith('评测运行已启动')
  })
})
