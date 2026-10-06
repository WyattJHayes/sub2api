import { describe, expect, it } from 'vitest'
import { ssrRenderAttrs } from 'vue/server-renderer'

describe('Vue SSR attribute safety', () => {
  it('rejects carriage returns in attribute names before emitting HTML', () => {
    expect(ssrRenderAttrs({ 'id\ronfocus': 'regression-marker' })).toBe('')
  })

  it('preserves valid attributes and escapes their values', () => {
    expect(ssrRenderAttrs({ id: 'usage', title: '<model>"' })).toBe(
      ' id="usage" title="&lt;model&gt;&quot;"'
    )
  })
})
