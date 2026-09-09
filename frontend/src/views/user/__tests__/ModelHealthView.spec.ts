import { config, flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import ModelHealthView from '../ModelHealthView.vue'
import zhCommon from '@/i18n/locales/zh/common'

const { getModelHealth } = vi.hoisted(() => ({
  getModelHealth: vi.fn()
}))

const labels: Record<string, string> = {
  'common.refresh': '刷新',
  'common.loading': '加载中...',
  'common.notAvailable': '不可用',
  'modelHealth.title': '模型健康',
  'modelHealth.description': '查看可用模型的公开质量与可靠性信号。',
  'modelHealth.updated': '最后更新',
  'modelHealth.p99': 'P99 延迟',
  'modelHealth.errorRate': '错误率',
  'modelHealth.empty': '暂无公开模型健康数据',
  'modelHealth.loadFailed': '加载模型健康数据失败，请稍后重试。',
  'modelHealth.status.healthy': '健康',
  'modelHealth.status.degraded': '降级',
  'modelHealth.status.insufficient_evidence': '证据不足',
  'modelHealth.quality.highRisk': '高风险',
  'modelHealth.quality.suspected': '疑似异常',
  'modelHealth.quality.observe': '需要观察',
  'modelHealth.quality.normal': '未见显著异常',
  'modelHealth.quality.insufficient': '覆盖不足',
  'modelHealth.coverage.sufficient': '覆盖充分',
  'modelHealth.coverage.insufficient': '覆盖不足',
  'modelHealth.coverage.none': zhCommon.modelHealth.coverage.none,
  'modelHealth.coverage.detail': '覆盖维度'
}

vi.mock('@/api/radar', () => ({
  getModelHealth
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => labels[key] ?? key,
    te: (key: string) => key in labels
  })
}))

vi.mock('@/components/layout/AppLayout.vue', () => ({
  default: { template: '<main data-testid="app-layout"><slot /></main>' }
}))

config.global.stubs.RouterLink = { template: '<a><slot /></a>' }

describe('ModelHealthView', () => {
  beforeEach(() => {
    getModelHealth.mockReset()
    getModelHealth.mockResolvedValue([
      {
        model_alias: 'gpt-5.6-sol',
        health_state: 'healthy',
        freshness: '2026-08-10T02:00:00Z',
        p99_ms: 850,
        error_rate: 0.01
      }
    ])
  })

  it('renders public model health inside the standard app layout', async () => {
    const wrapper = mount(ModelHealthView)
    await flushPromises()

    expect(wrapper.find('[data-testid="app-layout"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('模型健康')
    expect(wrapper.text()).toContain('最后更新')
    expect(wrapper.get('time').attributes('datetime')).toBe('2026-08-10T02:00:00Z')
    expect(wrapper.text()).not.toContain('样本数')
    expect(wrapper.text()).toContain('P99 延迟 850 ms')
    expect(wrapper.text()).toContain('错误率 1.00%')
  })

  it('shows an API failure separately from an empty health inventory', async () => {
    getModelHealth.mockRejectedValueOnce(new Error('endpoint missing'))

    const wrapper = mount(ModelHealthView)
    await flushPromises()

    expect(wrapper.text()).toContain('加载模型健康数据失败，请稍后重试。')
    expect(wrapper.text()).not.toContain('暂无公开模型健康数据')
  })

  it('shows a quality conclusion and links to the report without rendering an invalid timestamp', async () => {
    getModelHealth.mockResolvedValueOnce([{
      model_alias: 'gpt-4-1',
      health_state: 'degraded',
      overall_conclusion: 'high_risk',
      adulteration_risk: 'high_risk',
      degradation_risk: 'observe',
      freshness: 'not-a-date'
    }])

    const wrapper = mount(ModelHealthView, {
      global: { stubs: { RouterLink: { template: '<a><slot /></a>' } } }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('高风险')
    expect(wrapper.find('[data-test="model-quality-report-gpt-4-1"]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('Invalid Date')
  })

  it('renders coverage independently from the health conclusion', async () => {
    getModelHealth.mockResolvedValueOnce([{
      model_alias: 'gpt-5.6-sol',
      health_state: 'healthy',
      overall_conclusion: 'no_significant_anomaly',
      coverage: 2 / 3,
      covered_dimensions: 2,
      total_dimensions: 3,
      minimum_samples: 3
    }])

    const wrapper = mount(ModelHealthView)
    await flushPromises()

    expect(wrapper.text()).toContain('未见显著异常')
    expect(wrapper.text()).toContain('覆盖不足')
    expect(wrapper.text()).toContain('覆盖维度 2/3 (67%)')
  })

  it('does not infer zero samples from missing dimension statistics', async () => {
    getModelHealth.mockResolvedValueOnce([{
      model_alias: 'global',
      health_state: 'insufficient_evidence',
      overall_conclusion: 'insufficient_coverage',
      coverage: 0,
      covered_dimensions: 0,
      total_dimensions: 0,
      minimum_samples: 3
    }])

    const wrapper = mount(ModelHealthView)
    await flushPromises()

    expect(wrapper.text()).toContain('暂无覆盖数据')
    expect(wrapper.text()).not.toContain('暂无样本')
    expect(wrapper.text()).not.toContain('覆盖维度 0/0')
  })

  it('does not infer zero samples when an older API omits coverage fields', async () => {
    const wrapper = mount(ModelHealthView)
    await flushPromises()

    expect(wrapper.text()).toContain('暂无覆盖数据')
    expect(wrapper.text()).not.toContain('暂无样本')
  })
})
