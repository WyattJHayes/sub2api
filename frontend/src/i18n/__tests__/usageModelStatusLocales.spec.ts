import { describe, expect, it } from 'vitest'

import en from '../locales/en'
import zh from '../locales/zh'

describe('usage upstream model status locale keys', () => {
  it('keeps the Chinese traffic-light labels explicit', () => {
    expect(zh.usage.modelConsistent).toBe('模型一致')
    expect(zh.usage.modelUnknown).toBe('模型未知')
    expect(zh.usage.modelMismatch).toBe('模型不一致')
    expect(zh.usage.upstreamResponseUnknown).toBe('未知')
  })

  it('keeps the English status labels aligned with the same keys', () => {
    expect(en.usage.modelConsistent).toBe('Model consistent')
    expect(en.usage.modelUnknown).toBe('Model unknown')
    expect(en.usage.modelMismatch).toBe('Model mismatch')
    expect(en.usage.upstreamResponseUnknown).toBe('Unknown')
  })
})
