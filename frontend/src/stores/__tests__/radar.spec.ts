import { describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useRadarStore } from '../radar'
import radarAdminAPI from '@/api/admin/radar'

vi.mock('@/api/admin/radar', () => ({ default: { overview: vi.fn(), models: vi.fn(), runs: vi.fn(), alerts: vi.fn(), gates: vi.fn(), workers: vi.fn(), datasets: vi.fn() } }))

describe('radar store', () => {
  it('keeps overview projections in section state', async () => {
    setActivePinia(createPinia())
    vi.mocked(radarAdminAPI.overview).mockResolvedValueOnce({ models: [{ model_route: 'qwen', health_state: 'healthy' }], alerts: [], gates: [], workers: [] })
    const store = useRadarStore()
    await store.refresh()
    expect(store.models[0].model_route).toBe('qwen')
    expect(store.error).toBeNull()
  })

  it('loads run projections into section state', async () => {
    setActivePinia(createPinia())
    vi.mocked(radarAdminAPI.runs).mockResolvedValueOnce([{ id: 'run-1', plan_id: 'plan-1', status: 'pending' }])
    const store = useRadarStore()

    await store.loadSection('runs')

    expect(store.runs).toEqual([{ id: 'run-1', plan_id: 'plan-1', status: 'pending' }])
  })

  it('retains dataset provenance in the current tenant projection', async () => {
    setActivePinia(createPinia())
    vi.mocked(radarAdminAPI.datasets).mockResolvedValueOnce([
      { id: 'dataset-1', source_type: 'synthetic', created_by: 101, tenant_id: 7 }
    ])
    const store = useRadarStore()

    await store.loadSection('datasets')

    expect(store.datasets).toEqual([
      { id: 'dataset-1', source_type: 'synthetic', created_by: 101, tenant_id: 7 }
    ])
  })
})
