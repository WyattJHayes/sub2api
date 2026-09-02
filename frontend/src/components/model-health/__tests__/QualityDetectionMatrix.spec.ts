import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import QualityDetectionMatrix from '../QualityDetectionMatrix.vue'
import enCommon from '@/i18n/locales/en/common'
import zhCommon from '@/i18n/locales/zh/common'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => ({
      'modelHealth.report.behaviorGroup': '模型行为与能力画像',
      'modelHealth.report.protocolGroup': '调用链路与协议质量',
      'modelHealth.report.samples': '样本',
      'modelHealth.report.confidence': '置信度',
      'modelHealth.dimension.knowledge_freshness': '知识时效核验',
      'modelHealth.evidence.within_policy_bounds': '检测结果处于当前策略边界内。',
      'modelHealth.evidence.fingerprint_mismatch': '型号行为指纹与声明来源存在偏离。'
    })[key] ?? key
  })
}))

const dimensions = [
  'knowledge_freshness', 'model_fingerprint', 'reasoning_stability', 'structure_compliance',
  'parameter_fidelity', 'instruction_hierarchy', 'protocol_schema', 'stream_completeness'
].map((key, index) => ({
  key,
  score: 0.9,
  status: index === 3 ? 'suspected' : 'no_significant_anomaly',
  sample_count: 8,
  confidence: 0.9,
  checked_at: '2026-08-11T00:00:00Z',
  evidence_code: ['within_policy_bounds', 'coverage_insufficient', 'fingerprint_matched', 'fingerprint_mismatch', 'reasoning_variance', 'structure_violation', 'parameter_deviation', 'protocol_violation'][index]
}))

const publicEvidenceCodes = [
  'within_policy_bounds', 'coverage_insufficient', 'fingerprint_matched', 'fingerprint_mismatch',
  'reasoning_variance', 'structure_violation', 'parameter_deviation', 'instruction_violation',
  'protocol_violation', 'stream_incomplete', 'source_confirmed', 'source_inferred', 'source_insufficient_evidence'
]

describe('QualityDetectionMatrix', () => {
  it('renders all eight quality dimensions in two groups', () => {
    const wrapper = mount(QualityDetectionMatrix, { props: { dimensions } })

    expect(wrapper.findAll('[data-test="quality-dimension-row"]')).toHaveLength(8)
    expect(wrapper.text()).toContain('模型行为与能力画像')
    expect(wrapper.text()).toContain('调用链路与协议质量')
  })

  it('shows the allowlisted evidence summary for each dimension', () => {
    const wrapper = mount(QualityDetectionMatrix, { props: { dimensions } })

    expect(wrapper.findAll('[data-test="quality-dimension-evidence"]')).toHaveLength(8)
    expect(wrapper.text()).toContain('检测结果处于当前策略边界内。')
    expect(wrapper.text()).toContain('型号行为指纹与声明来源存在偏离。')
  })

  it('defines every public evidence code in both locales', () => {
    for (const code of publicEvidenceCodes) {
      expect(zhCommon.modelHealth.evidence[code as keyof typeof zhCommon.modelHealth.evidence]).toEqual(expect.any(String))
      expect(enCommon.modelHealth.evidence[code as keyof typeof enCommon.modelHealth.evidence]).toEqual(expect.any(String))
    }
  })
})
