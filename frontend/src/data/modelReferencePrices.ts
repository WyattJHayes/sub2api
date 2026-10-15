/** Public external reference snapshot. All prices are USD per million tokens.
 * Display only: never import into billing or apply platform/group multipliers.
 * Update prices, context boundaries and capturedOn together after checking source.
 */
export const referencePriceSource = {
  name: 'MY JARVIS',
  url: 'https://web.aijws.com/models',
  capturedOn: '2026-10-15'
} as const

interface ReferenceTier {
  context: string
  input: number
  output: number
  cacheRead: number | null
  cacheWrite: number | null
}

interface ReferenceModel {
  id: string
  provider: string
  tiers: ReferenceTier[]
  note?: 'wholeRequest'
}

function tier(context: string, input: number, output: number, cacheRead: number | null, cacheWrite: number | null = null): ReferenceTier {
  return { context, input, output, cacheRead, cacheWrite }
}

export const referenceModels: readonly ReferenceModel[] = [
  { id: 'gpt-6-astra', provider: 'OpenAI', note: 'wholeRequest', tiers: [tier('≤272K', 10, 50, 1), tier('>272K', 20, 75, 2)] },
  { id: 'gpt-6-sol', provider: 'OpenAI', note: 'wholeRequest', tiers: [tier('≤272K', 2, 10, 0.2), tier('>272K', 4, 15, 0.4)] },
  { id: 'gpt-5.6-sol', provider: 'OpenAI', note: 'wholeRequest', tiers: [tier('≤272K', 4, 20, 0.4), tier('>272K', 8, 30, 0.8)] },
  { id: 'gpt-5.6-terra', provider: 'OpenAI', note: 'wholeRequest', tiers: [tier('≤272K', 2, 12, 0.2), tier('>272K', 4, 18, 0.4)] },
  { id: 'gpt-5.3-codex', provider: 'OpenAI', tiers: [tier('all', 1.75, 14, 0.175)] },
  { id: 'claude-fable-5-1', provider: 'Claude Code', tiers: [tier('all', 12, 60, 0.3)] },
  { id: 'claude-fable-5', provider: 'Claude Code', tiers: [tier('all', 12, 60, 1.2)] },
  { id: 'claude-opus-5-5', provider: 'Claude Code', tiers: [tier('all', 4, 20, 0.2)] },
  { id: 'claude-opus-5', provider: 'Claude Code', tiers: [tier('all', 5, 25, 0.5)] },
  { id: 'claude-opus-4-8', provider: 'Claude Code', tiers: [tier('all', 5, 25, 0.5)] },
  { id: 'claude-sonnet-4-6', provider: 'Claude Code', tiers: [tier('all', 3, 15, 0.3)] },
  { id: 'claude-haiku-4-5', provider: 'Claude Code', tiers: [tier('all', 1, 5, 0.1)] },
  { id: 'GLM-5.1', provider: 'GLM', tiers: [tier('<32K', 6, 24, 1.3), tier('≥32K', 8, 28, 2)] },
  { id: 'GLM-5.2', provider: 'GLM', tiers: [tier('all', 8, 28, 2)] },
  { id: 'GLM-5.2-fast-preview', provider: 'GLM', tiers: [tier('all', 16, 56, 4)] },
  { id: 'Kimi-K3', provider: 'Kimi', tiers: [tier('all', 20, 100, 2)] },
  { id: 'Kimi-K2.5', provider: 'Kimi', tiers: [tier('all', 4, 21, 0.8, 5)] },
  { id: 'Kimi-K2.6', provider: 'Kimi', tiers: [tier('all', 6.5, 27, 1.1)] },
  { id: 'Kimi-K2.7-code', provider: 'Kimi', tiers: [tier('all', 6.5, 27, 1.3)] },
  { id: 'Qwen3.7-max', provider: 'Qwen', tiers: [tier('all', 12, 36, 2.4, 15)] },
  { id: 'Qwen3.8-max', provider: 'Qwen', tiers: [tier('all', 12, 36, 1.5, 15)] },
  { id: 'Qwen3.8-max-0902', provider: 'Qwen', tiers: [tier('all', 12, 36, 1.5, 15)] },
  { id: 'Qwen3.7-plus', provider: 'Qwen', tiers: [tier('≤256K', 2, 8, 0.4, 2.5), tier('>256K', 6, 24, 1.2, 7.5)] },
  { id: 'Qwen3.6-flash', provider: 'Qwen', tiers: [tier('≤256K', 1.2, 7.2, 0.24, 1.5), tier('>256K', 4.8, 28.8, 0.96, 6)] },
  { id: 'Qwen3.7-flash', provider: 'Qwen', tiers: [tier('≤32K', 0.2, 0.8, 0.04, 0.25), tier('>32K, ≤256K', 0.6, 2.4, 0.12, 0.75), tier('>256K', 1.2, 4.8, 0.24, 1.5)] },
  { id: 'Qwen3.8-flash', provider: 'Qwen', tiers: [tier('all', 0.8, 2.7, 0.1, 1.25)] },
  { id: 'DeepSeek-V4-Pro', provider: 'DeepSeek', tiers: [tier('all', 9, 27, 0.3)] },
  { id: 'DeepSeek-V4-Pro-0813', provider: 'DeepSeek', tiers: [tier('all', 9, 27, 0.9)] },
  { id: 'DeepSeek-V4-Flash', provider: 'DeepSeek', tiers: [tier('all', 2, 8, 0.04)] },
  { id: 'DeepSeek-V4-Flash-0731', provider: 'DeepSeek', tiers: [tier('all', 3, 9, 0.3)] },
  { id: 'DeepSeek-V4.1-Flash', provider: 'DeepSeek', tiers: [tier('all', 2, 8, 0.04)] },
  { id: 'Doubao-Seed-2-1-Pro', provider: 'Doubao', tiers: [tier('all', 6, 30, 1.2)] },
  { id: 'Doubao-Seed-2-1-Turbo', provider: 'Doubao', tiers: [tier('all', 3, 15, 0.6)] }
]
