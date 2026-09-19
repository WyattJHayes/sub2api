import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import QualityEvidencePanels from '../QualityEvidencePanels.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => ({
      'modelHealth.report.keyEvidence': '本轮关键证据',
      'modelHealth.report.source': '来源识别',
      'modelHealth.report.sourceBoundary': '来源识别仅基于受控探针的允许证据编码。',
      'modelHealth.report.inferredSource': '行为推断',
      'modelHealth.evidence.fingerprint_mismatch': '型号行为指纹与声明来源存在偏离。'
    })[key] ?? key
  })
}))

describe('QualityEvidencePanels', () => {
  it('keeps public evidence and source attribution in sibling panels with the boundary footer', () => {
    const wrapper = mount(QualityEvidencePanels, {
      props: {
        evidence: [{ dimension_key: 'model_fingerprint', code: 'fingerprint_mismatch' }],
        source: { state: 'inferred', display_name: 'Candidate A', confidence: 0.8, evidence_code: 'source_inferred' }
      }
    })

    const panels = wrapper.findAll('section > div')
    expect(panels).toHaveLength(2)
    expect(panels[0].text()).toContain('本轮关键证据')
    expect(panels[1].text()).toContain('来源识别')
    expect(panels[1].text()).toContain('来源识别仅基于受控探针的允许证据编码。')
  })

  it('hides malformed candidate names when source evidence is insufficient', () => {
    const wrapper = mount(QualityEvidencePanels, {
      props: {
        evidence: [],
        source: {
          state: 'insufficient_evidence',
          display_name: 'private-source-name',
          alternate_candidates: [{ display_name: 'private-candidate-name', confidence: 0.9 }],
          evidence_code: 'source_insufficient_evidence'
        }
      }
    })

    expect(wrapper.text()).not.toContain('private-source-name')
    expect(wrapper.text()).not.toContain('private-candidate-name')
  })
})
