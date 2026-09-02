import { beforeEach, describe, expect, it, vi } from 'vitest'
import radarAdminAPI from '../admin/radar'
import { apiClient } from '../client'

vi.mock('../client', () => ({ apiClient: { get: vi.fn(), post: vi.fn(), delete: vi.fn() } }))

describe('radar admin API', () => {
  beforeEach(() => vi.clearAllMocks())

  it('uses the sanitized overview endpoint', async () => {
    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: { models: [] } })
    await radarAdminAPI.overview()
    expect(apiClient.get).toHaveBeenCalledWith('/admin/radar/overview', { params: undefined })
  })

  it('uses the conflict-free gate evaluation endpoint', async () => {
    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { id: 'gate-1', status: 'passed' } })
    await radarAdminAPI.evaluateGate({ run_id: 'run-1' })
    expect(apiClient.post).toHaveBeenCalledWith('/admin/radar/gates/evaluate', { run_id: 'run-1' })
  })

  it('creates and publishes a versioned evaluation dataset', async () => {
    const payload = {
      dataset_key: 'smoke-reasoning',
      version: '2026-07-27',
      source_type: 'synthetic',
      cases: [{
        case_key: 'addition-1',
        capability_domain: 'reasoning',
        priority: 'P1',
        weight: '1',
        sample_count: 1,
        prompt_spec: { input: 'Return 4' },
        expected_spec: '4',
        execution_spec: { url: '/v1/responses' },
        grader_id: 'exact',
        grader_version: 'v1',
        confidentiality: 'synthetic',
        estimated_cost: '0.01'
      }]
    }
    vi.mocked(apiClient.post)
      .mockResolvedValueOnce({ data: { id: 'dataset-1', ...payload, status: 'draft' } })
      .mockResolvedValueOnce({ data: { id: 'dataset-1', ...payload, status: 'published' } })

    await radarAdminAPI.createDataset(payload)
    await radarAdminAPI.publishDataset('dataset-1')

    expect(apiClient.post).toHaveBeenNthCalledWith(1, '/admin/radar/datasets', payload)
    expect(apiClient.post).toHaveBeenNthCalledWith(2, '/admin/radar/datasets/dataset-1/publish')
  })

  it('creates an evaluation plan bound to a dedicated gateway key', async () => {
    const payload = {
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
    }
    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { id: 'plan-1', ...payload } })

    await radarAdminAPI.createPlan(payload)

    expect(apiClient.post).toHaveBeenCalledWith('/admin/radar/plans', payload)
  })

  it('starts a paired evaluation run from a plan', async () => {
    const payload = {
      plan_id: 'plan-1',
      trigger_source: 'manual',
      baseline_ref: { release: 'baseline' },
      candidate_ref: { release: 'candidate' }
    }
    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { id: 'run-1', status: 'pending' } })

    await radarAdminAPI.startRun(payload)

    expect(apiClient.post).toHaveBeenCalledWith('/admin/radar/runs', payload)
  })

  it('enables an existing API key for signed evaluation traffic', async () => {
    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { id: 42, is_evaluation: true } })

    await radarAdminAPI.enableEvaluationKey(42)

    expect(apiClient.post).toHaveBeenCalledWith('/admin/radar/evaluation-keys/42/enable')
  })

  it('registers a model in the tenant Radar inventory', async () => {
    vi.mocked(apiClient.post).mockResolvedValueOnce({
      data: { model_alias: 'gpt-5.6-sol', created_by: 77, created_at: '2026-08-10T12:00:00Z' }
    })

    await radarAdminAPI.registerModel({ model_alias: 'gpt-5.6-sol' })

    expect(apiClient.post).toHaveBeenCalledWith('/admin/radar/models', { model_alias: 'gpt-5.6-sol' })
  })

  it('sends an untrack request for a model alias', async () => {
    vi.mocked(apiClient.delete).mockResolvedValueOnce({ data: { model_alias: 'gpt-4-1', status: 'untracked' } })

    await radarAdminAPI.untrackModel('gpt-4-1')

    expect(apiClient.delete).toHaveBeenCalledWith('/admin/radar/models/gpt-4-1')
  })

  it('URL-encodes aliases instead of using the visible aggregate route', async () => {
    vi.mocked(apiClient.delete).mockResolvedValueOnce({ data: { model_alias: 'global/tenant:model', status: 'untracked' } })

    await radarAdminAPI.untrackModel('global/tenant:model')

    expect(apiClient.delete).toHaveBeenCalledWith('/admin/radar/models/global%2Ftenant%3Amodel')
  })
})
