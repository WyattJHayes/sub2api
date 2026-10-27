/** Display-only catalog, reviewed 2026-10-17. Never import into billing.
 * Published currency and units; no exchange-rate conversion.
 * Sources and regions are retained for audit, never rendered as source notices.
 * Unknown prices remain null, not zero or an outdated third-party snapshot.
 */
export interface ReferenceTier {
  context: string
  input: number | null
  output: number | null
  cacheRead: number | null
  cacheWrite: number | null
  cacheWrite1h: number | null
  explicitCacheRead: number | null
}

/** Display-only rows. Image output estimates are alternatives to token totals. */
export type ReferenceMediaPrice = {
  kind: 'tokens'
  modality: 'text' | 'image' | 'audio'
  input: number | null
  cacheRead: number | null
  output: number | null
  outputUnsupported?: boolean
} | {
  kind: 'imageEstimate'
  quality: 'low' | 'medium' | 'high'
  size: '1024×1024' | '1024×1536' | '1536×1024'
  output: number
}

export interface ReferenceModel {
  id: string
  provider: string
  currency: 'USD' | 'CNY'
  status: 'official' | 'reference' | 'unverified'
  region?: 'beijing' | 'singapore'
  /** Exact published service model, retained for quote/mapping review only. */
  quotedModel?: string
  source: string
  tiers: ReferenceTier[]
  note?: 'wholeRequest' | 'promptLengthTier' | 'deepseekHours'
  mediaPrices?: readonly ReferenceMediaPrice[]
  mediaNote?: 'imageOutputEstimates' | 'imageTokenUsage'
  cacheReadResponsesOnly?: boolean
  deprecated?: boolean
  pendingReason?: 'modelMapping' | 'priceVerification'
  /** Internal audit date; never rendered as a source notice. */
  reviewedAt?: string
}

function tier(context: string, input: number | null, output: number | null, cacheRead: number | null, cacheWrite: number | null = null, cacheWrite1h: number | null = null, explicitCacheRead: number | null = null): ReferenceTier {
  return { context, input, output, cacheRead, cacheWrite, cacheWrite1h, explicitCacheRead }
}
const openai = 'https://developers.openai.com/api/docs/pricing'
const claude = 'https://platform.claude.com/docs/en/about-claude/pricing'
const kimi = 'https://platform.kimi.ai/docs/pricing/chat.md'
const zai = 'https://docs.z.ai/guides/overview/pricing'
const deepseek = 'https://api-docs.deepseek.com/quick_start/pricing/'
const xai = 'https://docs.x.ai/developers/pricing'
const byteplus = 'https://ai.byteplus.com/ark/region:ap-southeast-1/docs/model-pricing'
const ali = 'https://www.alibabacloud.com/help/en/model-studio/'
function model(id: string, provider: string, source: string, tiers: ReferenceTier[], options: Partial<Pick<ReferenceModel, 'status' | 'region' | 'note' | 'quotedModel'>> = {}): ReferenceModel {
  return { id, provider, currency: 'USD', status: 'official', source, tiers, ...options }
}
const unknown = (id: string, provider: string, source: string): ReferenceModel => ({
  ...model(id, provider, source, [tier('all', null, null, null)], { status: 'unverified' }),
  pendingReason: 'priceVerification', reviewedAt: '2026-10-17'
})
function mediaModel(id: string, mediaPrices: ReferenceMediaPrice[], options: Pick<ReferenceModel, 'mediaNote' | 'cacheReadResponsesOnly' | 'deprecated'> = {}): ReferenceModel {
  return { id, provider: 'OpenAI', currency: 'USD', status: 'official',
    source: `https://developers.openai.com/api/docs/models/${id}`,
    tiers: [], mediaPrices, reviewedAt: '2026-10-17', ...options }
}
function imageEstimates(prices: readonly number[]): ReferenceMediaPrice[] {
  const sizes = ['1024×1024', '1024×1536', '1536×1024'] as const
  return (['low', 'medium', 'high'] as const).flatMap((quality, qualityIndex) =>
    sizes.map((size, sizeIndex) => ({ kind: 'imageEstimate', quality, size, output: prices[qualityIndex * 3 + sizeIndex]! })))
}
const dsFlash = [tier('peak', 0.3, 1.2, 0.006), tier('offPeak', 0.15, 0.6, 0.003)]
const dsPro = [tier('peak', 1.32, 3.96, 0.044), tier('offPeak', 0.66, 1.98, 0.022)]

const baseModels: readonly ReferenceModel[] = [
  model('gpt-6-astra', 'OpenAI', openai, [tier('≤272K', 10, 50, 1, 12.5), tier('>272K', 20, 75, 2, 25)], { note: 'wholeRequest' }),
  model('gpt-6-sol', 'OpenAI', openai, [tier('≤272K', 2, 10, 0.2, 2.5), tier('>272K', 4, 15, 0.4, 5)], { note: 'wholeRequest' }),
  model('gpt-5.6-sol', 'OpenAI', openai, [tier('≤272K', 4, 20, 0.4, 5), tier('>272K', 8, 30, 0.8, 10)], { note: 'wholeRequest' }),
  model('gpt-5.6-terra', 'OpenAI', openai, [tier('≤272K', 2, 12, 0.2, 2.5), tier('>272K', 4, 18, 0.4, 5)], { note: 'wholeRequest' }),
  model('gpt-5.3-codex', 'OpenAI', openai, [tier('all', 1.75, 14, 0.175)]),
  model('claude-fable-5-1', 'Claude Code', claude, [tier('all', 10, 50, 0.25, 12.5, 20)]),
  model('claude-fable-5', 'Claude Code', claude, [tier('all', 10, 50, 1, 12.5, 20)]),
  model('claude-opus-5-5', 'Claude Code', claude, [tier('all', 4, 20, 0.2, 5, 8)]),
  model('claude-opus-5', 'Claude Code', claude, [tier('all', 5, 25, 0.5, 6.25, 10)]),
  model('claude-opus-4-8', 'Claude Code', claude, [tier('all', 5, 25, 0.5, 6.25, 10)]),
  model('claude-sonnet-4-6', 'Claude Code', claude, [tier('all', 3, 15, 0.3, 3.75, 6)]),
  model('claude-haiku-4-5', 'Claude Code', claude, [tier('all', 1, 5, 0.1, 1.25, 2)]),
  model('GLM-5.1', 'GLM', zai, [tier('all', 1.4, 4.4, 0.26)]),
  model('GLM-5.2', 'GLM', zai, [tier('all', 1.4, 4.4, 0.26)]),
  model('GLM-5.2-fast-preview', 'GLM', ali + 'glm-5-2-fast', [tier('all', 2.2, 7.702, 0.55)], { status: 'reference', region: 'beijing' }),
  model('Kimi-K3', 'Kimi', kimi, [tier('all', 3, 15, 0.3, 3, 6)]),
  // Original K2.5 retired; hosted-service reference, not an original-provider price.
  model('Kimi-K2.5', 'Kimi', ali + 'kimi-k2-5', [tier('all', 0.574, 3.011, 0.115, 0.718, null, 0.057)], { status: 'reference', region: 'beijing' }),
  model('Kimi-K2.6', 'Kimi', kimi, [tier('all', 0.95, 4, 0.16)]),
  model('Kimi-K2.7-code', 'Kimi', kimi, [tier('all', 0.95, 4, 0.19)]),
  model('Qwen3.7-max', 'Qwen', ali + 'qwen3-7-max', [tier('all', 1.65, 4.951, 0.33, 2.063, null, 0.165)], { region: 'beijing' }),
  model('Qwen3.8-max', 'Qwen', ali + 'qwen3-8-max', [tier('all', 1.65, 4.951, 0.206, 2.063, null, 0.137)], { region: 'beijing' }),
  model('Qwen3.8-max-0902', 'Qwen', ali + 'qwen3-8-max', [tier('all', 1.65, 4.951, 0.206, 2.063, null, 0.137)], { region: 'beijing' }),
  model('Qwen3.7-plus', 'Qwen', ali + 'qwen3-7-plus', [tier('≤256K', 0.276, 1.101, 0.056, 0.344, null, 0.028), tier('>256K', 0.826, 3.301, 0.166, 1.032, null, 0.083)], { region: 'beijing' }),
  model('Qwen3.6-flash', 'Qwen', ali + 'qwen3-6-flash', [tier('≤256K', 0.165, 0.99, null, 0.206, null, 0.017), tier('>256K', 0.66, 3.961, null, 0.825, null, 0.066)], { region: 'beijing' }),
  model('Qwen3.7-flash', 'Qwen', ali + 'qwen3-7-flash', [tier('≤32K', 0.028, 0.11, 0.006, 0.034, null, 0.003), tier('>32K, ≤256K', 0.083, 0.33, 0.017, 0.103, null, 0.008), tier('>256K', 0.165, 0.66, 0.033, 0.206, null, 0.017)], { region: 'beijing' }),
  model('Qwen3.8-flash', 'Qwen', ali + 'qwen3-8-flash', [tier('all', 0.113, 0.382, 0.014, 0.177, null, 0.014)], { region: 'beijing' }),
  model('DeepSeek-V4-Pro', 'DeepSeek', deepseek, dsPro, { note: 'deepseekHours' }),
  model('DeepSeek-V4-Pro-0813', 'DeepSeek', deepseek, dsPro, { note: 'deepseekHours' }),
  // The official legacy alias is now served and billed as V4.1-Flash.
  model('DeepSeek-V4-Flash', 'DeepSeek', deepseek, dsFlash, { note: 'deepseekHours' }),
  // Retired on the original API; this exact dated release remains hosted.
  // Cache storage ($/M tokens/hour) is not a cache-write token price.
  model('DeepSeek-V4-Flash-0731', 'DeepSeek', byteplus, [tier('all', 0.44, 1.32, 0.014)], { status: 'reference', region: 'singapore', quotedModel: 'deepseek-v4-flash-ga-260731' }),
  model('DeepSeek-V4.1-Flash', 'DeepSeek', deepseek, dsFlash, { note: 'deepseekHours' }),
  unknown('Doubao-Seed-2-1-Pro', 'Doubao', 'https://seed.bytedance.com/en/seed2_1'),
  // The Seed2.1 English API link selects this international Turbo service.
  model('Doubao-Seed-2-1-Turbo', 'Doubao', byteplus, [tier('≤256K', 0.5, 2.5, 0.1)], { status: 'reference', region: 'singapore', quotedModel: 'dola-seed-2-1-turbo' })
]

// Verified exact IDs are public independently of group visibility. Unresolved
// entries are only added when a visible group advertises their exact ID.
const supplementalModels: readonly ReferenceModel[] = [
  // Exact aliases only. gpt-5.6 is documented by OpenAI; gpt-6 maps to
  // Astra in backend/internal/service/openai_model_alias.go and constants.go.
  { ...model('gpt-5.6', 'OpenAI', 'https://developers.openai.com/api/docs/models/gpt-5.6', [tier('≤272K', 4, 20, 0.4, 5), tier('>272K', 8, 30, 0.8, 10)], { note: 'wholeRequest', quotedModel: 'gpt-5.6-sol' }), reviewedAt: '2026-10-17' },
  { ...model('gpt-6', 'OpenAI', 'https://developers.openai.com/api/docs/models/gpt-6-astra', [tier('≤272K', 10, 50, 1, 12.5), tier('>272K', 20, 75, 2, 25)], { note: 'wholeRequest', quotedModel: 'gpt-6-astra' }), reviewedAt: '2026-10-17' },
  // No verified exact public model mapping. Do not infer automatic routing
  // behavior, a free price, or an API rate from subscription credit tables.
  { ...unknown('codex-auto-review', 'OpenAI', openai), pendingReason: 'modelMapping', reviewedAt: '2026-10-17' },
  { ...unknown('gpt-reserve', 'OpenAI', openai), pendingReason: 'modelMapping', reviewedAt: '2026-10-17' },
  { ...unknown('gpt-5.3-codex-spark', 'OpenAI', openai), pendingReason: 'priceVerification', reviewedAt: '2026-10-17' },
  mediaModel('gpt-4o-audio-preview', [
    { kind: 'tokens', modality: 'text', input: 2.5, cacheRead: null, output: 10 },
    { kind: 'tokens', modality: 'audio', input: 40, cacheRead: null, output: 80 }
  ], { deprecated: true }),
  mediaModel('gpt-4o-realtime-preview', [
    { kind: 'tokens', modality: 'text', input: 5, cacheRead: 2.5, output: 20 },
    { kind: 'tokens', modality: 'audio', input: 40, cacheRead: 2.5, output: 80 }
  ], { deprecated: true }),
  mediaModel('gpt-image-1', [
    { kind: 'tokens', modality: 'text', input: 5, cacheRead: 1.25, output: null, outputUnsupported: true },
    { kind: 'tokens', modality: 'image', input: 10, cacheRead: 2.5, output: 40 },
    ...imageEstimates([0.011, 0.016, 0.016, 0.042, 0.063, 0.063, 0.167, 0.25, 0.25])
  ], { mediaNote: 'imageOutputEstimates', deprecated: true }),
  mediaModel('gpt-image-1.5', [
    { kind: 'tokens', modality: 'text', input: 5, cacheRead: 1.25, output: 10 },
    { kind: 'tokens', modality: 'image', input: 8, cacheRead: 2, output: 32 },
    ...imageEstimates([0.009, 0.013, 0.013, 0.034, 0.05, 0.05, 0.133, 0.2, 0.2])
  ], { mediaNote: 'imageOutputEstimates', deprecated: true }),
  ...['gpt-image-2', 'gpt-image-2.5-flare', 'gpt-image-2.5-sunburst'].map(id => mediaModel(id, [
    { kind: 'tokens', modality: 'text', input: 5, cacheRead: 1.25, output: null, outputUnsupported: true },
    { kind: 'tokens', modality: 'image', input: 8, cacheRead: 2, output: 30 }
  ], { mediaNote: 'imageTokenUsage', cacheReadResponsesOnly: true })),
  // Exact snapshots listed by the official model pages; no date-stripping fallback.
  model('gpt-5.2-2025-12-11', 'OpenAI', 'https://developers.openai.com/api/docs/models/gpt-5.2', [tier('all', 1.75, 14, 0.175)]),
  model('gpt-5.2-chat-latest', 'OpenAI', 'https://developers.openai.com/api/docs/models/gpt-5.2-chat-latest', [tier('all', 1.75, 14, 0.175)]),
  model('gpt-5.2-pro', 'OpenAI', 'https://developers.openai.com/api/docs/models/gpt-5.2-pro', [tier('all', 21, 168, null)]),
  model('gpt-5.2-pro-2025-12-11', 'OpenAI', 'https://developers.openai.com/api/docs/models/gpt-5.2-pro', [tier('all', 21, 168, null)]),
  model('gpt-5.4-2026-03-05', 'OpenAI', 'https://developers.openai.com/api/docs/models/gpt-5.4', [tier('≤272K', 2.5, 15, 0.25), tier('>272K', 5, 22.5, 0.5)], { note: 'wholeRequest' }),
  model('claude-sonnet-5-5', 'Claude Code', claude, [tier('all', 2, 10, 0.1, 2.5, 4)]),
  model('claude-sonnet-5', 'Claude Code', claude, [tier('all', 2, 10, 0.2, 2.5, 4)]),
  model('claude-sonnet-4-5', 'Claude Code', claude, [tier('all', 3, 15, 0.3, 3.75, 6)]),
  model('claude-opus-4-5', 'Claude Code', claude, [tier('all', 5, 25, 0.5, 6.25, 10)]),
  model('claude-opus-4-6', 'Claude Code', claude, [tier('all', 5, 25, 0.5, 6.25, 10)]),
  model('claude-opus-4-7', 'Claude Code', claude, [tier('all', 5, 25, 0.5, 6.25, 10)]),
  model('claude-haiku-5-5', 'Claude Code', claude, [tier('≤100K', 0.1, 0.5, 0.01, 0.125, 0.2), tier('>100K', 0.5, 2.5, 0.05, 0.625, 1)], { note: 'promptLengthTier' }),
  model('GLM-5.3', 'GLM', zai, [tier('all', 1.4, 4.4, 0.26)]),
  model('GLM-5.3-Flash', 'GLM', zai, [tier('all', 0.15, 0.5, 0.03)]),
  { ...model('GLM-5.3-FlashX', 'GLM', zai, [tier('all', 0.37, 1.25, 0.075)]), reviewedAt: '2026-10-27' },
  { ...model('GLM-5.3-FlashX-Legal', 'GLM', zai, [tier('all', 0.37, 1.25, 0.075)]), reviewedAt: '2026-10-27' },
  model('grok-4.5', 'Grok', xai, [tier('<200K', 2, 6, 0.3), tier('≥200K', 4, 12, 0.6)], { note: 'promptLengthTier' }),
  model('grok-4.6', 'Grok', xai, [tier('<200K', 2, 6, 0.5), tier('≥200K', 4, 12, 1)], { note: 'promptLengthTier' }),
  model('grok-4.7', 'Grok', xai, [tier('<200K', 2, 6, 0.5), tier('≥200K', 4, 12, 1)], { note: 'promptLengthTier' }),
  model('gpt-6.1-sol', 'OpenAI', openai, [tier('≤272K', 2, 10, 0.1, 2.5), tier('>272K', 4, 15, 0.2, 5)], { note: 'wholeRequest' }),
  // Standard processing; cache writes use the published 30-minute retention rate.
  { ...model('gpt-6.1-terra', 'OpenAI', 'https://developers.openai.com/api/docs/models/gpt-6.1-terra', [tier('≤272K', 1, 5, 0.05, 1.25), tier('>272K', 2, 7.5, 0.1, 2.5)], { note: 'wholeRequest' }), reviewedAt: '2026-10-27' },
  model('gpt-6-luna', 'OpenAI', openai, [tier('≤272K', 0.1, 0.5, 0.01, 0.125), tier('>272K', 0.2, 0.75, 0.02, 0.25)], { note: 'wholeRequest' }),
  model('gpt-5.6-luna', 'OpenAI', openai, [tier('≤272K', 0.2, 1.2, 0.02, 0.25), tier('>272K', 0.4, 1.8, 0.04, 0.5)], { note: 'wholeRequest' }),
  model('gpt-5.5', 'OpenAI', openai, [tier('≤272K', 5, 30, 0.5), tier('>272K', 10, 45, 1)], { note: 'wholeRequest' }),
  model('gpt-5.4', 'OpenAI', openai, [tier('≤272K', 2.5, 15, 0.25), tier('>272K', 5, 22.5, 0.5)], { note: 'wholeRequest' }),
  model('gpt-5.4-mini', 'OpenAI', openai, [tier('all', 0.75, 4.5, 0.075)]),
  model('gpt-5.2', 'OpenAI', openai, [tier('all', 1.75, 14, 0.175)])
]

export const referenceModels: readonly ReferenceModel[] = [
  ...baseModels,
  ...supplementalModels.filter(model => model.status !== 'unverified')
]

export function findReferenceModel(id: string): ReferenceModel | undefined {
  const key = referenceModelKey(id)
  return [...baseModels, ...supplementalModels].find(model => referenceModelKey(model.id) === key)
}

/** Only established spelling equivalences; no fuzzy family/date matching. */
export function referenceModelKey(id: string): string {
  const key = id.trim().toLowerCase()
  return key.startsWith('claude-') ? key.replace(/\./g, '-') : key
}
