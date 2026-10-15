import { describe, expect, it } from 'vitest'
import { calculateGroupTiers, formatReferencePrice } from '../referencePriceQuotes'
import { referenceModels } from '@/data/modelReferencePrices'
import type { ModelPlazaGroup, PlazaModel } from '@/api/modelPlaza'

const astra = referenceModels.find(m => m.id === 'gpt-6-astra')!
const plain = { name: 'gpt-6-astra', platform: 'openai', pricing: null, official_pricing: null } satisfies PlazaModel
const group = { rate_multiplier: 0.4, long_context_pricing_enabled: true } as ModelPlazaGroup

describe('reference price calculations', () => {
  it('applies each official long-context tier once and retains missing prices', () => {
    const tiers = calculateGroupTiers(astra, group, plain, 'default')!
    expect(tiers[0]).toMatchObject({ input: 4, output: 20, cacheRead: 0.4, cacheWrite: 5, cacheWrite1h: null })
    expect(tiers[1]).toMatchObject({ input: 8, output: 30, cacheRead: 0.8, cacheWrite: 10 })
    expect(calculateGroupTiers(astra, { ...group, long_context_pricing_enabled: false }, plain, 'default')).toHaveLength(1)
  })
  it('keeps DeepSeek time rates when long-context pricing is disabled', () => {
    const ds = referenceModels.find(m => m.id === 'DeepSeek-V4-Pro')!
    const tiers = calculateGroupTiers(ds, { ...group, long_context_pricing_enabled: false }, plain, 'default')!
    expect(tiers).toHaveLength(2)
    expect(tiers[0]!.input).toBeCloseTo(0.528)
    expect(tiers[1]!.input).toBeCloseTo(0.264)
  })
  it.each([NaN, Infinity, -1])('rejects invalid rates rather than showing free or full-price quotes: %s', rate => {
    expect(calculateGroupTiers(astra, { ...group, rate_multiplier: rate }, plain, 'default')).toBeNull()
  })
  it('does not invent a price for an unknown exact model', () => {
    const unknown = referenceModels.find(m => m.id === 'Doubao-Seed-2-1-Pro')!
    expect(calculateGroupTiers(unknown, group, plain, 'default')).toBeNull()
  })
  it('preserves official currency and small prices without converting CNY to dollars', () => {
    expect(formatReferencePrice(0.003, 'USD')).toBe('$0.003')
    expect(formatReferencePrice(6, 'CNY')).toBe('CN¥6.00')
    expect(formatReferencePrice(null, 'USD')).toBeNull()
    expect(formatReferencePrice(0.0000003, 'USD')).toBe('$0.0000003')
    const cny = { ...astra, currency: 'CNY' as const }
    expect(calculateGroupTiers(cny, group, plain, 'default')![0]!.input).toBe(4)
    const override = { ...plain, group_pricing_override: { billing_mode: 'token', input_price: 1e-6, intervals: [] } } as PlazaModel
    expect(calculateGroupTiers(cny, group, override, 'default')).toBeNull()
  })
  it('rejects a negative configured price even when the group is free', () => {
    const single = { ...astra, tiers: astra.tiers.slice(0, 1) }
    const override = { ...plain, group_pricing_override: { billing_mode: 'token', input_price: -1e-6, intervals: [] } } as PlazaModel
    expect(calculateGroupTiers(single, { ...group, rate_multiplier: 0 }, override, 'default')).toBeNull()
  })

  it('uses resolved group context tiers without applying the official long-context uplift again', () => {
    const override = { ...plain, group_pricing_override: {
      billing_mode: 'token', input_price: 12e-6, output_price: 60e-6,
      intervals: [
        { min_tokens: 0, max_tokens: 100000, tier_label: '≤100K', input_price: 12e-6, output_price: 60e-6 },
        { min_tokens: 100000, max_tokens: null, tier_label: '>100K', input_price: 18e-6, output_price: 90e-6 }
      ]
    } } as PlazaModel
    const tiers = calculateGroupTiers(astra, group, override, 'default')!
    expect(tiers).toHaveLength(2)
    expect(tiers[0]).toMatchObject({ context: '≤100K', output: 24 })
    expect(tiers[0]!.input).toBeCloseTo(4.8)
    expect(tiers[1]).toMatchObject({ context: '>100K', output: 36 })
    expect(tiers[1]!.input).toBeCloseTo(7.2)
  })

  it('rejects unresolved interval multipliers and gaps instead of guessing their rates', () => {
    const override = { ...plain, group_pricing_override: {
      billing_mode: 'token', input_price: 12e-6,
      intervals: [{ min_tokens: 100000, max_tokens: null, input_multiplier: 2 }]
    } } as PlazaModel
    expect(calculateGroupTiers(astra, group, override, 'default')).toBeNull()
  })

  it('uses a lone configured cache-write price for both TTL tiers, matching billing', () => {
    const fable = referenceModels.find(m => m.id === 'claude-fable-5-1')!
    const override = { ...plain, group_pricing_override: {
      billing_mode: 'token', cache_write_price: 15e-6, intervals: []
    } } as PlazaModel
    const tier = calculateGroupTiers(fable, group, override, 'default')![0]!
    expect(tier.cacheWrite).toBe(6)
    expect(tier.cacheWrite1h).toBe(6)
  })

  it('does not invent explicit-cache or TTL modes absent from the tariff and configuration', () => {
    const fable = referenceModels.find(m => m.id === 'claude-fable-5-1')!
    const override = { ...plain, group_pricing_override: {
      billing_mode: 'token', cache_read_price: 0.3e-6, cache_write_price: 4e-6, intervals: []
    } } as PlazaModel
    expect(calculateGroupTiers(fable, group, override, 'default')![0]!.explicitCacheRead).toBeNull()
    const codex = referenceModels.find(m => m.id === 'gpt-5.3-codex')!
    expect(calculateGroupTiers(codex, group, override, 'default')![0]!.cacheWrite1h).toBeNull()
    const resolved = { ...override, group_cache_write_1h_configured: false, group_pricing_override: {
      ...override.group_pricing_override!, cache_write_1h_price: 4e-6
    } } as PlazaModel
    expect(calculateGroupTiers(codex, group, resolved, 'default')![0]!.cacheWrite1h).toBeNull()
    expect(calculateGroupTiers(codex, group, { ...resolved, group_cache_write_1h_configured: true }, 'default')![0]!.cacheWrite1h).toBeCloseTo(1.6)
  })

  it('applies a selected time rate and zero rates without hiding unknown prices', () => {
    expect(calculateGroupTiers(astra, group, plain, 'default', 0.5)![0]!.input).toBe(2)
    expect(calculateGroupTiers(astra, group, plain, 'default', 0)![0]).toMatchObject({ input: 0, cacheWrite1h: null })
    expect(calculateGroupTiers(astra, group, plain, 'default', -1)).toBeNull()
  })
})
