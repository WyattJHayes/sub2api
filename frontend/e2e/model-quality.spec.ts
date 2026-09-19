import { readFileSync, statSync } from 'node:fs'
import path from 'node:path'
import { expect, test } from '@playwright/test'
import {
  installLoopbackOriginGuard,
  requireLoopbackOrigin,
  requireMatchingIdentity,
  requireSameOriginURL,
  requireUnitIntervalConfidence
} from './support/environment'

const USER_STORAGE_STATE = path.resolve('test-results/.auth/user.json')
const BASE_ORIGIN = requireLoopbackOrigin(process.env.RADAR_E2E_BASE_URL ?? '')
const SCENARIO_NAMES = ['healthy', 'watered', 'degraded', 'insufficient'] as const
const DIMENSION_LABELS = [
  '知识时效核验',
  '型号指纹匹配',
  '逻辑求解稳定性',
  '结构约束遵循',
  '调用参数保真',
  '指令层级遵循',
  '协议字段规范',
  '流式响应完整性'
] as const

const CONCLUSION_LABELS = {
  no_significant_anomaly: '未见显著异常',
  observe: '需要观察',
  suspected: '疑似异常',
  high_risk: '高风险',
  insufficient_coverage: '检测覆盖不足'
} as const

type Conclusion = keyof typeof CONCLUSION_LABELS
type ScenarioName = typeof SCENARIO_NAMES[number]
type SourceState = 'inferred' | 'insufficient_evidence'

interface ExpectedConclusion {
  overall_conclusion: Conclusion
  adulteration_risk: Conclusion
  degradation_risk: Conclusion
  source_state: SourceState
}

interface FixtureScenario {
  model_alias: string
  run_id: string
  expected: ExpectedConclusion
}

interface FixtureManifest {
  schema_version: string
  run_identifier: string
  fixture_user_email: string
  setup_administrator_email: string
  route_snapshot_path: string
  scenarios: Record<ScenarioName, FixtureScenario>
}

const EXPECTED_SCENARIOS: Record<ScenarioName, FixtureScenario> = {
  healthy: {
    model_alias: 'radar-quality-healthy',
    run_id: '',
    expected: {
      overall_conclusion: 'no_significant_anomaly',
      adulteration_risk: 'no_significant_anomaly',
      degradation_risk: 'no_significant_anomaly',
      source_state: 'inferred'
    }
  },
  watered: {
    model_alias: 'radar-quality-watered',
    run_id: '',
    expected: {
      overall_conclusion: 'high_risk',
      adulteration_risk: 'high_risk',
      degradation_risk: 'no_significant_anomaly',
      source_state: 'inferred'
    }
  },
  degraded: {
    model_alias: 'radar-quality-degraded',
    run_id: '',
    expected: {
      overall_conclusion: 'high_risk',
      adulteration_risk: 'no_significant_anomaly',
      degradation_risk: 'high_risk',
      source_state: 'inferred'
    }
  },
  insufficient: {
    model_alias: 'radar-quality-insufficient',
    run_id: '',
    expected: {
      overall_conclusion: 'insufficient_coverage',
      adulteration_risk: 'insufficient_coverage',
      degradation_risk: 'insufficient_coverage',
      source_state: 'insufficient_evidence'
    }
  }
}

const forbiddenKeys = new Set([
  'prompt', 'completion', 'credentials', 'route_trace_id', 'account_ref',
  'channel_ref', 'artifact', 'probe_spec_hash', 'observation_hash'
])
const forbiddenKeyFamilies = [
  'credential', 'password', 'token', 'apikey', 'prompt', 'completion', 'trace',
  'account', 'channel', 'rawartifact', 'probespechash', 'observation'
] as const

function asRecord(value: unknown, errorMessage: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(errorMessage)
  }
  return value as Record<string, unknown>
}

function normalizedKey(key: string): string {
  return key.toLowerCase().replace(/[^a-z0-9]+/g, '')
}

const normalizedForbiddenKeys = new Set([...forbiddenKeys].map(normalizedKey))

function containsForbiddenKey(value: unknown): boolean {
  if (Array.isArray(value)) return value.some(containsForbiddenKey)
  if (typeof value !== 'object' || value === null) return false

  return Object.entries(value).some(([key, nested]) => {
    const normalized = normalizedKey(key)
    return normalizedForbiddenKeys.has(normalized)
      || forbiddenKeyFamilies.some((family) => normalized.includes(family))
      || containsForbiddenKey(nested)
  })
}

function hasExactKeys(value: Record<string, unknown>, keys: readonly string[]): boolean {
  const actual = Object.keys(value).sort()
  const expected = [...keys].sort()
  return actual.length === expected.length && actual.every((key, index) => key === expected[index])
}

function loadFixtureManifest(): FixtureManifest {
  const manifestPath = process.env.RADAR_E2E_FIXTURE_MANIFEST
  if (!manifestPath) throw new Error('RADAR_E2E_FIXTURE_MANIFEST is required')

  let document: unknown
  try {
    const metadata = statSync(manifestPath)
    if (!metadata.isFile() || (metadata.mode & 0o777) !== 0o600) {
      throw new Error('fixture manifest must be a mode 0600 regular file')
    }
    document = JSON.parse(readFileSync(manifestPath, 'utf8'))
  } catch (error) {
    if (error instanceof Error && error.message === 'fixture manifest must be a mode 0600 regular file') throw error
    throw new Error('fixture manifest must be readable valid JSON')
  }

  const manifest = asRecord(document, 'fixture manifest must be a JSON object')
  if (containsForbiddenKey(manifest)) throw new Error('fixture manifest contains a forbidden evidence key')
  if (!hasExactKeys(manifest, [
    'schema_version', 'run_identifier', 'fixture_user_email',
    'setup_administrator_email', 'route_snapshot_path', 'scenarios'
  ])) throw new Error('fixture manifest must use the exact top-level schema')
  if (manifest.schema_version !== 'radar-local-quality-fixture-v1') {
    throw new Error('fixture manifest has an unsupported schema version')
  }
  for (const field of ['run_identifier', 'fixture_user_email', 'setup_administrator_email'] as const) {
    if (typeof manifest[field] !== 'string' || manifest[field].length === 0) {
      throw new Error('fixture manifest is missing a required identity field')
    }
  }
  if (typeof manifest.route_snapshot_path !== 'string'
    || !/^\/api\/v1\/admin\/groups\/[1-9][0-9]*\/composite-routes$/.test(manifest.route_snapshot_path)) {
    throw new Error('fixture manifest route snapshot path is invalid')
  }
  const configuredUserIdentity = process.env.RADAR_E2E_USER_EMAIL
  const configuredAdministratorIdentity = process.env.RADAR_E2E_ADMIN_EMAIL
  if (!configuredUserIdentity || !configuredAdministratorIdentity) {
    throw new Error('configured fixture identities are required')
  }
  requireMatchingIdentity(String(manifest.fixture_user_email), configuredUserIdentity)
  requireMatchingIdentity(String(manifest.setup_administrator_email), configuredAdministratorIdentity)

  const scenarios = asRecord(manifest.scenarios, 'fixture manifest scenarios must be an object')
  if (!hasExactKeys(scenarios, SCENARIO_NAMES)) {
    throw new Error('fixture manifest must contain exactly four scenarios')
  }
  for (const name of SCENARIO_NAMES) {
    const scenario = asRecord(scenarios[name], 'fixture manifest scenario is malformed')
    if (!hasExactKeys(scenario, ['model_alias', 'run_id', 'expected'])) {
      throw new Error('fixture manifest scenario is malformed')
    }
    const expected = asRecord(scenario.expected, 'fixture manifest expected conclusion is malformed')
    const contract = EXPECTED_SCENARIOS[name]
    if (scenario.model_alias !== contract.model_alias || typeof scenario.run_id !== 'string' || scenario.run_id.length === 0) {
      throw new Error('fixture manifest scenario identity is invalid')
    }
    if (!hasExactKeys(expected, ['overall_conclusion', 'adulteration_risk', 'degradation_risk', 'source_state'])
      || Object.entries(contract.expected).some(([key, value]) => expected[key] !== value)) {
      throw new Error('fixture manifest scenario conclusion is invalid')
    }
  }

  return manifest as unknown as FixtureManifest
}

function collectCandidateNames(source: Record<string, unknown>): string[] {
  const names = new Set<string>()
  if (typeof source.display_name === 'string' && source.display_name) names.add(source.display_name)
  if (Array.isArray(source.alternate_candidates)) {
    for (const candidate of source.alternate_candidates) {
      const record = asRecord(candidate, 'quality report candidate is malformed')
      if (typeof record.display_name === 'string' && record.display_name) names.add(record.display_name)
    }
  }
  return [...names]
}

const manifest = loadFixtureManifest()

test.use({ storageState: USER_STORAGE_STATE })
test.beforeEach(async ({ context }) => {
  await installLoopbackOriginGuard(context, BASE_ORIGIN)
})

test('@smoke ordinary user sees the four fixture quality reports without sensitive evidence', async ({ page }) => {
  const knownCandidateNames = new Set<string>()
  const main = page.getByRole('main')

  await page.goto('/model-health')
  await expect(main.getByRole('heading', { name: '模型健康', exact: true })).toBeVisible()
  for (const name of SCENARIO_NAMES) {
    const alias = manifest.scenarios[name].model_alias
    await expect(page.locator(`[data-test="model-quality-report-${alias}"]`)).toBeVisible()
  }

  for (const name of SCENARIO_NAMES) {
    const scenario = manifest.scenarios[name]
    await page.goto(`/model-health/${scenario.model_alias}`)
    await expect(main.getByRole('heading', { name: '模型质量报告', exact: true })).toBeVisible()

    const token = await page.evaluate(() => localStorage.getItem('auth_token'))
    if (!token) throw new Error('ordinary-user storage state is missing authentication')
    const requestURL = requireSameOriginURL(
      `/api/v1/radar/models/${encodeURIComponent(scenario.model_alias)}/quality-report`,
      BASE_ORIGIN
    )
    const response = await page.request.get(requestURL, {
      headers: { Authorization: `Bearer ${token}` },
      maxRedirects: 0
    })
    expect(response.status()).toBeGreaterThanOrEqual(200)
    expect(response.status()).toBeLessThan(300)

    const envelope = asRecord(await response.json(), 'quality report response must be an object')
    if (containsForbiddenKey(envelope)) throw new Error('public quality report contains a forbidden evidence key')
    const report = asRecord('data' in envelope ? envelope.data : envelope, 'quality report payload must be an object')
    expect(report.model_alias).toBe(scenario.model_alias)
    expect(report.overall_conclusion).toBe(scenario.expected.overall_conclusion)
    expect(report.adulteration_risk).toBe(scenario.expected.adulteration_risk)
    expect(report.degradation_risk).toBe(scenario.expected.degradation_risk)

    const source = asRecord(report.source_attribution, 'quality report source attribution must be an object')
    expect(source.state).toBe(scenario.expected.source_state)
    const qualitySummary = main.locator('section.grid').filter({
      has: page.getByText('检测模型', { exact: true })
    })
    await expect(qualitySummary).toHaveCount(1)
    const sourceAttributionRegion = qualitySummary
      .getByText('来源识别', { exact: true })
      .locator('..')

    await expect(page.getByText('综合结论', { exact: true }).locator('..'))
      .toContainText(CONCLUSION_LABELS[scenario.expected.overall_conclusion])
    const riskSummary = page.getByText('掺水风险', { exact: true }).locator('..')
    await expect(riskSummary).toContainText(CONCLUSION_LABELS[scenario.expected.adulteration_risk])
    await expect(riskSummary).toContainText(`降智风险 ${CONCLUSION_LABELS[scenario.expected.degradation_risk]}`)

    if (name === 'insufficient') {
      await expect(page.getByText('检测覆盖不足', { exact: true }).first()).toBeVisible()
      await expect(sourceAttributionRegion.getByText('暂无法判断来源', { exact: true })).toBeVisible()
      expect(collectCandidateNames(source)).toEqual([])
      for (const candidateName of knownCandidateNames) {
        await expect(page.getByText(candidateName, { exact: false })).toHaveCount(0)
      }
      continue
    }

    const sourceConfidence = requireUnitIntervalConfidence(source.confidence)
    for (const candidateName of collectCandidateNames(source)) knownCandidateNames.add(candidateName)
    for (const label of DIMENSION_LABELS) {
      await expect(page.getByText(label, { exact: true })).toBeVisible()
    }
    await expect(sourceAttributionRegion.getByText(/^行为推断/)).toBeVisible()
    await expect(sourceAttributionRegion.getByText(`${Math.round(sourceConfidence * 100)}%`, { exact: true })).toBeVisible()
    await expect(page.getByText('来源识别仅基于受控探针的允许证据编码，不展示调用链原始数据。', { exact: true })).toBeVisible()
  }
})
