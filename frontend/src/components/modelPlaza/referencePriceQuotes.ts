import type { ModelPlazaGroup, PlazaModel } from '@/api/modelPlaza'
import type { ReferenceModel, ReferenceTier } from '@/data/modelReferencePrices'
import type { UserPricingInterval, UserSupportedModelPricing } from '@/api/channels'

export const referencePriceColumns = ['input', 'output', 'cacheRead', 'cacheWrite', 'cacheWrite1h', 'explicitCacheRead'] as const
type PriceColumn = typeof referencePriceColumns[number]
const overrideFields = {
  input: 'input_price', output: 'output_price', cacheRead: 'cache_read_price',
  cacheWrite: 'cache_write_price', cacheWrite1h: 'cache_write_1h_price', explicitCacheRead: 'cache_read_price'
} as const
const valid = (value: number): boolean => Number.isFinite(value) && value >= 0

/** Quotes are display calculations, never instructions to the billing engine.
 * Explicit group prices already contain model adjustments: use them once,
 * without multiplying the official base by the premium a second time.
 */
export function calculateGroupTiers(reference: ReferenceModel, group: ModelPlazaGroup, model: PlazaModel, effort: string, timeRate = 1): ReferenceTier[] | null {
  const rate = group.user_rate_multiplier ?? group.rate_multiplier
  const override = model.group_pricing_override
  const efforts = override?.reasoning_effort_multipliers ?? model.pricing?.reasoning_effort_multipliers
  const effortRate = effort === 'default' ? 1 : (efforts?.[effort] ?? 1)
  if (reference.status === 'unverified' || !valid(rate) || !valid(effortRate) || !valid(timeRate)) return null
  // Explicit group prices are resolved on the server through the billing engine.
  // Their context tiers replace the official tiers; never apply an uplift twice.
  if (override && (reference.currency !== 'USD' || (override.billing_mode && override.billing_mode !== 'token'))) return null
  const tiers = override ? overrideTiers(reference, override, model.group_cache_write_1h_configured === true) :
    !group.long_context_pricing_enabled && reference.note !== 'deepseekHours' ? reference.tiers.slice(0, 1) : reference.tiers
  if (!tiers) return null
  const result: ReferenceTier[] = []
  for (const tier of tiers) {
    const values = {} as Record<PriceColumn, number | null>
    for (const column of referencePriceColumns) {
      const base = tier[column]
      if (base !== null && !valid(base)) return null
      const quoted = base === null ? null : base * rate * effortRate * timeRate
      if (quoted !== null && !valid(quoted)) return null
      values[column] = quoted
    }
    result.push({ context: tier.context, ...values })
  }
  return result
}

function overrideTiers(reference: ReferenceModel, pricing: UserSupportedModelPricing, oneHourConfigured: boolean): ReferenceTier[] | null {
  const intervals = pricing.intervals
  // Raw/unresolved configurations must not masquerade as the resolved schedule.
  let previous = 0
  for (const interval of intervals) {
    if (!Number.isSafeInteger(interval.min_tokens) || interval.min_tokens !== previous ||
      (interval.max_tokens !== null && (!Number.isSafeInteger(interval.max_tokens) || interval.max_tokens <= previous)) ||
      [interval.input_multiplier, interval.output_multiplier, interval.cache_write_multiplier, interval.cache_read_multiplier].some(value => value != null)) return null
    previous = interval.max_tokens ?? Infinity
  }
  if (intervals.length && previous !== Infinity) return null
  const base = reference.tiers[0]
  if (!base) return null
  const rows = intervals.length ? intervals : [null]
  const result: ReferenceTier[] = []
  for (const interval of rows) {
    const values = {} as Record<PriceColumn, number | null>
    for (const column of referencePriceColumns) {
      if ((column === 'explicitCacheRead' && base.explicitCacheRead === null) ||
        (column === 'cacheWrite1h' && base.cacheWrite1h === null && !oneHourConfigured)) {
        values[column] = null
        continue
      }
      const field = overrideFields[column]
      // A lone cache-write override applies to both TTLs in the billing engine.
      const configured = interval?.[field] ??
        (column === 'cacheWrite1h' ? interval?.cache_write_price : null) ?? pricing[field] ??
        (column === 'cacheWrite1h' ? pricing.cache_write_price : null)
      if (configured != null && !valid(configured)) return null
      values[column] = configured != null ? configured * 1e6 : base[column]
    }
    const context = interval ? intervalLabel(interval) : 'all'
    result.push({ context, ...values })
  }
  return result
}

function intervalLabel(interval: UserPricingInterval): string {
  if (interval.tier_label) return interval.tier_label
  const lower = interval.min_tokens > 0 ? `>${interval.min_tokens.toLocaleString('en-US')}` : ''
  const upper = interval.max_tokens === null ? '' : `≤${interval.max_tokens.toLocaleString('en-US')}`
  return [lower, upper].filter(Boolean).join(', ') || 'all'
}

export function formatReferencePrice(value: number | null, currency: ReferenceModel['currency']): string | null {
  if (value === null || !valid(value)) return null
  const digits = value > 0 ? Math.min(20, Math.max(6, 2 - Math.floor(Math.log10(value)))) : 6
  return new Intl.NumberFormat('en-US', { style: 'currency', currency, minimumFractionDigits: 2, maximumFractionDigits: digits }).format(value)
}
