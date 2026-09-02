import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import RadarAlertsView from '../RadarAlertsView.vue'
import RadarDatasetsView from '../RadarDatasetsView.vue'
import RadarGatesView from '../RadarGatesView.vue'
import RadarOverviewView from '../RadarOverviewView.vue'
import RadarRunsView from '../RadarRunsView.vue'
import RadarWorkersView from '../RadarWorkersView.vue'

const radarStore = {
  overview: null as any,
  models: [] as any[],
  alerts: [] as any[],
  gates: [] as any[],
  workers: [] as any[],
  datasets: [] as any[],
  runs: [] as any[],
  loadSection: vi.fn()
}

vi.mock('vue-i18n', async () => {
  const { default: zhLocale } = await import('@/i18n/locales/zh')
  const resolve = (key: string) => key.split('.').reduce<any>((value, part) => value?.[part], zhLocale)

  return {
    useI18n: () => ({
      t: (key: string, params?: Record<string, string>) => {
        const value = resolve(key) ?? key
        return typeof value === 'string' && params
          ? value.replace(/\{(\w+)\}/g, (_, name) => params[name] ?? `{${name}}`)
          : value
      },
      te: (key: string) => resolve(key) !== undefined
    })
  }
})

vi.mock('@/stores/radar', () => ({ useRadarStore: () => radarStore }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))
vi.mock('@/api/admin/radar', () => ({ default: {} }))

const BaseDialogStub = {
  props: ['show', 'title'],
  template: '<div v-if="show" role="dialog"><h2>{{ title }}</h2><slot /><slot name="footer" /></div>'
}

function mountView(component: any) {
  return mount(component, {
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Icon: { template: '<span />' }
      }
    }
  })
}

describe('Radar localized views', () => {
  beforeEach(() => {
    radarStore.overview = null
    radarStore.models = []
    radarStore.alerts = []
    radarStore.gates = []
    radarStore.workers = []
    radarStore.datasets = []
    radarStore.runs = []
    radarStore.loadSection.mockReset()
    radarStore.loadSection.mockResolvedValue(undefined)
  })

  it('renders the overview, alert lifecycle, gate and worker labels in Chinese', async () => {
    radarStore.overview = { summary: { models: 2, open_alerts: 1, blocked_gates: 1, healthy_workers: 1 } }
    radarStore.alerts = [{ id: 'alert-1', model_route: 'candidate', capability_domain: 'reasoning', cause: 'service_quality', severity: 'P1', status: 'open' }]
    radarStore.gates = [{ id: 'gate-1', model_route: 'candidate', status: 'blocked' }]
    radarStore.workers = [{ id: 'worker-1', name: 'runner-1', worker_kind: 'runner', status: 'active' }]

    const overview = mountView(RadarOverviewView)
    const alerts = mountView(RadarAlertsView)
    const gates = mountView(RadarGatesView)
    const workers = mountView(RadarWorkersView)
    await flushPromises()

    expect(overview.text()).toContain('模型健康')
    expect(overview.text()).toContain('待处理告警')
    expect(overview.text()).toContain('已阻止门禁')
    expect(overview.text()).toContain('健康执行器')
    expect(alerts.text()).toContain('告警生命周期')
    expect(alerts.text()).toContain('能力域')
    expect(alerts.text()).toContain('推理')
    expect(alerts.text()).toContain('服务质量')
    expect(alerts.text()).toContain('待处理')
    expect(gates.text()).toContain('发布门禁')
    expect(gates.text()).toContain('已阻止')
    expect(workers.text()).toContain('执行器健康')
    expect(workers.text()).toContain('运行执行器')
    expect(workers.text()).toContain('心跳')
    expect(workers.text()).toContain('未知')
    expect(workers.text()).toContain('活跃')
  })

  it('renders dataset and run dialogs, controls and status values in Chinese', async () => {
    radarStore.datasets = [
      {
        id: 'dataset-1',
        name: 'reasoning-smoke',
        version: '2026-07-27',
        status: 'draft',
        source_type: 'synthetic',
        created_by: 101,
        tenant_id: 7
      },
      { id: 'dataset-2', name: 'legacy-smoke', status: 'draft' }
    ]
    radarStore.runs = [{ id: 'run-1', plan_id: 'plan-1', trigger_source: 'manual', status: 'pending', reserved_cost: '0' }]

    const datasets = mountView(RadarDatasetsView)
    const runs = mountView(RadarRunsView)
    await flushPromises()

    expect(datasets.text()).toContain('评测数据集')
    expect(datasets.text()).toContain('数据集')
    expect(datasets.text()).toContain('来源')
    expect(datasets.text()).toContain('创建者')
    expect(datasets.text()).toContain('租户 ID')
    expect(datasets.text()).toContain('合成')
    expect(datasets.text()).toContain('101')
    expect(datasets.text()).toContain('7')
    expect(datasets.text()).toContain('-')
    expect(datasets.text()).toContain('发布')
    expect(datasets.text()).toContain('草稿')
    await datasets.get('[data-test="new-dataset"]').trigger('click')
    expect(datasets.text()).toContain('新建评测数据集')
    expect(datasets.text()).toContain('数据集标识')
    expect(datasets.text()).toContain('合成')
    expect(datasets.text()).toContain('推理')
    expect(datasets.text()).toContain('创建数据集')

    expect(runs.text()).toContain('评测运行')
    expect(runs.text()).toContain('保留成本')
    expect(runs.text()).toContain('手动')
    expect(runs.text()).toContain('等待中')
    await runs.get('[data-test="new-plan"]').trigger('click')
    expect(runs.text()).toContain('新建评测计划')
    expect(runs.text()).toContain('已发布数据集')
    expect(runs.text()).toContain('启用雷达评测')
    expect(runs.text()).toContain('创建计划')
    await runs.get('[data-test="start-run"]').trigger('click')
    expect(runs.text()).toContain('开始评测运行')
    expect(runs.text()).toContain('候选发布引用')
  })
})
