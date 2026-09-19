import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import ModelQualityReportView from '../ModelQualityReportView.vue'

const { getModelQualityReport } = vi.hoisted(() => ({ getModelQualityReport: vi.fn() }))

vi.mock('@/api/radar', () => ({ getModelQualityReport }))
vi.mock('vue-router', () => ({ useRoute: () => ({ params: { alias: 'gpt-4-1' } }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => ({
  'modelHealth.report.title': '模型质量报告',
  'modelHealth.report.coverageInsufficient': '检测覆盖不足',
  'modelHealth.report.stale': '报告已过期',
  'modelHealth.report.generatedAt': '生成时间',
  'modelHealth.report.freshUntil': '有效至',
  'modelHealth.report.notFound': '当前检测覆盖不足，尚无可用质量报告。',
  'modelHealth.report.loadFailed': '加载质量报告失败，请稍后重试。'
})[key] ?? key }) }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))

describe('ModelQualityReportView', () => {
  it('renders the public report and no private probe fields', async () => {
    getModelQualityReport.mockResolvedValueOnce({
      model_alias: 'gpt-4-1', overall_conclusion: 'observe', adulteration_risk: 'observe', degradation_risk: 'observe',
      generated_at: '2026-08-11T00:00:00Z', fresh_until: '2026-08-12T00:00:00Z', dimension_results: [],
      source_attribution: { state: 'insufficient_evidence', evidence_code: 'source_insufficient_evidence' }, evidence: [],
      route_trace_id: 'private-route-trace', prompt: 'private-prompt'
    })
    const wrapper = mount(ModelQualityReportView, {
      global: {
        mocks: { $route: { params: { alias: 'gpt-4-1' } } },
        stubs: { QualitySummary: true, QualityDetectionMatrix: true, QualityEvidencePanels: true }
      }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('模型质量报告')
    expect(wrapper.text()).toContain('gpt-4-1')
    expect(wrapper.text()).not.toContain('private-route-trace')
    expect(wrapper.text()).not.toContain('private-prompt')
  })

  it('marks insufficient coverage and stale reports without crashing on invalid dates', async () => {
    getModelQualityReport.mockResolvedValueOnce({
      model_alias: 'gpt-4-1', overall_conclusion: 'insufficient_coverage', adulteration_risk: 'insufficient_coverage', degradation_risk: 'insufficient_coverage',
      generated_at: 'invalid-date', fresh_until: '2000-01-01T00:00:00Z', dimension_results: [],
      source_attribution: { state: 'insufficient_evidence', evidence_code: 'source_insufficient_evidence' }, evidence: []
    })
    const wrapper = mount(ModelQualityReportView, {
      global: { stubs: { QualitySummary: true, QualityDetectionMatrix: true, QualityEvidencePanels: true } }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('检测覆盖不足')
    expect(wrapper.text()).toContain('报告已过期')
    expect(wrapper.text()).toContain('生成时间')
    expect(wrapper.text()).toContain('有效至')
  })

  it('does not mark reports stale when freshness is malformed', async () => {
    getModelQualityReport.mockResolvedValueOnce({
      model_alias: 'gpt-4-1', overall_conclusion: 'observe', adulteration_risk: 'observe', degradation_risk: 'observe',
      generated_at: '2026-08-11T00:00:00Z', fresh_until: 'invalid-freshness', dimension_results: [],
      source_attribution: { state: 'insufficient_evidence', evidence_code: 'source_insufficient_evidence' }, evidence: []
    })
    const wrapper = mount(ModelQualityReportView, {
      global: { stubs: { QualitySummary: true, QualityDetectionMatrix: true, QualityEvidencePanels: true } }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('有效至')
    expect(wrapper.text()).not.toContain('报告已过期')
  })

  it('shows the coverage-insufficient state for the normalized 404 error', async () => {
    getModelQualityReport.mockRejectedValueOnce({ status: 404, code: 404, message: 'Quality report not found' })
    const wrapper = mount(ModelQualityReportView)
    await flushPromises()

    expect(wrapper.text()).toContain('当前检测覆盖不足，尚无可用质量报告。')
    expect(wrapper.text()).not.toContain('加载质量报告失败，请稍后重试。')
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })

  it('shows the loading failure state for a non-404 response', async () => {
    getModelQualityReport.mockRejectedValueOnce(new Error('network unavailable'))
    const wrapper = mount(ModelQualityReportView)
    await flushPromises()

    expect(wrapper.text()).toContain('加载质量报告失败，请稍后重试。')
  })
})
