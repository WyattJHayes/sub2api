/** Display-only catalog, reviewed 2026-10-15. Never import into billing.
 * Published currency / 1,000,000 tokens; no exchange-rate conversion.
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
  note?: 'wholeRequest' | 'deepseekHours'
}

function tier(context: string, input: number | null, output: number | null, cacheRead: number | null, cacheWrite: number | null = null, cacheWrite1h: number | null = null, explicitCacheRead: number | null = null): ReferenceTier {
  return { context, input, output, cacheRead, cacheWrite, cacheWrite1h, explicitCacheRead }
}
const openai = 'https://developers.openai.com/api/docs/pricing'
const claude = 'https://platform.claude.com/docs/en/about-claude/pricing'
const kimi = 'https://platform.kimi.ai/docs/pricing/chat.md'
const zai = 'https://docs.z.ai/guides/overview/pricing'
const deepseek = 'https://api-docs.deepseek.com/quick_start/pricing/'
const byteplus = 'https://ai.byteplus.com/ark/region:ap-southeast-1/docs/model-pricing'
const ali = 'https://www.alibabacloud.com/help/en/model-studio/'
function model(id: string, provider: string, source: string, tiers: ReferenceTier[], options: Partial<Pick<ReferenceModel, 'status' | 'region' | 'note' | 'quotedModel'>> = {}): ReferenceModel {
  return { id, provider, currency: 'USD', status: 'official', source, tiers, ...options }
}
const unknown = (id: string, provider: string, source: string) => model(id, provider, source, [tier('all', null, null, null)], { status: 'unverified' })
const dsFlash = [tier('peak', 0.3, 1.2, 0.006), tier('offPeak', 0.15, 0.6, 0.003)]
const dsPro = [tier('peak', 1.32, 3.96, 0.044), tier('offPeak', 0.66, 1.98, 0.022)]

export const referenceModels: readonly ReferenceModel[] = [
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

// Also price these exact IDs when advertised by a visible group. They are not
// added to the public catalog merely because a similarly named model exists.
const supplementalModels: readonly ReferenceModel[] = [
  model('gpt-6.1-sol', 'OpenAI', openai, [tier('≤272K', 2, 10, 0.1, 2.5), tier('>272K', 4, 15, 0.2, 5)], { note: 'wholeRequest' }),
  model('gpt-6-luna', 'OpenAI', openai, [tier('≤272K', 0.1, 0.5, 0.01, 0.125), tier('>272K', 0.2, 0.75, 0.02, 0.25)], { note: 'wholeRequest' }),
  model('gpt-5.6-luna', 'OpenAI', openai, [tier('≤272K', 0.2, 1.2, 0.02, 0.25), tier('>272K', 0.4, 1.8, 0.04, 0.5)], { note: 'wholeRequest' }),
  model('gpt-5.5', 'OpenAI', openai, [tier('≤272K', 5, 30, 0.5), tier('>272K', 10, 45, 1)], { note: 'wholeRequest' }),
  model('gpt-5.4', 'OpenAI', openai, [tier('≤272K', 2.5, 15, 0.25), tier('>272K', 5, 22.5, 0.5)], { note: 'wholeRequest' }),
  model('gpt-5.4-mini', 'OpenAI', openai, [tier('all', 0.75, 4.5, 0.075)]),
  model('gpt-5.2', 'OpenAI', openai, [tier('all', 1.75, 14, 0.175)])
]

export function findReferenceModel(id: string): ReferenceModel | undefined {
  const key = referenceModelKey(id)
  return [...referenceModels, ...supplementalModels].find(model => referenceModelKey(model.id) === key)
}

/** Only established spelling equivalences; no fuzzy family/date matching. */
export function referenceModelKey(id: string): string {
  const key = id.trim().toLowerCase()
  return key.startsWith('claude-') ? key.replace(/\./g, '-') : key
}
