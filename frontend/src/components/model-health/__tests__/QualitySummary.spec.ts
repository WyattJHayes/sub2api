import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import QualitySummary from '../QualitySummary.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => ({
      'modelHealth.report.declaredModel': '检测模型',
      'modelHealth.report.overall': '综合结论',
      'modelHealth.report.generatedAt': '生成时间',
      'modelHealth.report.freshUntil': '有效至',
      'modelHealth.report.source': '来源识别',
      'modelHealth.report.confirmedSource': '已确认来源',
      'modelHealth.report.inferredSource': '行为推断',
      'modelHealth.report.sourceUnavailable': '暂无法判断来源',
      'modelHealth.report.alternateCandidates': '其他候选来源',
      'modelHealth.quality.adulteration': '掺水风险',
      'modelHealth.quality.degradation': '降智风险',
      'modelHealth.quality.observe': '需要观察'
    })[key] ?? key
  })
}))

describe('QualitySummary', () => {
  it('shows inferred source alternatives alongside attribution confidence', () => {
    const wrapper = mount(QualitySummary, {
      props: {
        report: {
          model_alias: 'public-alias', overall_conclusion: 'observe', adulteration_risk: 'observe', degradation_risk: 'observe',
          generated_at: '2026-08-11T00:00:00Z', fresh_until: '2026-08-12T00:00:00Z', dimension_results: [], evidence: [],
          source_attribution: {
            state: 'inferred', display_name: 'Candidate A', confidence: 0.82, evidence_code: 'source_inferred',
            alternate_candidates: [{ display_name: 'Candidate B', confidence: 0.61 }]
          }
        }
      }
    })

    expect(wrapper.text()).toContain('行为推断')
    expect(wrapper.text()).toContain('Candidate A')
    expect(wrapper.text()).toContain('82%')
    expect(wrapper.text()).toContain('其他候选来源')
    expect(wrapper.text()).toContain('Candidate B')
    expect(wrapper.text()).toContain('61%')
  })

  it('shows a confirmed source name', () => {
    const wrapper = mount(QualitySummary, {
      props: {
        report: {
          model_alias: 'public-alias', overall_conclusion: 'observe', adulteration_risk: 'observe', degradation_risk: 'observe',
          generated_at: '2026-08-11T00:00:00Z', fresh_until: '2026-08-12T00:00:00Z', dimension_results: [], evidence: [],
          source_attribution: { state: 'confirmed', display_name: 'GPT-4.1', evidence_code: 'source_confirmed' }
        }
      }
    })

    expect(wrapper.text()).toContain('已确认来源')
    expect(wrapper.text()).toContain('GPT-4.1')
  })

  it('does not show source names when evidence is insufficient', () => {
    const wrapper = mount(QualitySummary, {
      props: {
        report: {
          model_alias: 'public-alias', overall_conclusion: 'observe', adulteration_risk: 'observe', degradation_risk: 'observe',
          generated_at: '2026-08-11T00:00:00Z', fresh_until: '2026-08-12T00:00:00Z', dimension_results: [], evidence: [],
          source_attribution: {
            state: 'insufficient_evidence', display_name: 'private-source-name',
            alternate_candidates: [{ display_name: 'private-candidate-name', confidence: 0.9 }], evidence_code: 'source_insufficient_evidence'
          }
        }
      }
    })

    expect(wrapper.text()).toContain('暂无法判断来源')
    expect(wrapper.text()).not.toContain('private-source-name')
    expect(wrapper.text()).not.toContain('private-candidate-name')
  })
})
