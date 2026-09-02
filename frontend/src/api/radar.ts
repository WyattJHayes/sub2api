import { apiClient } from './client'

export type QualityConclusion = 'no_significant_anomaly' | 'observe' | 'suspected' | 'high_risk' | 'insufficient_coverage'
export type QualityDimension =
  | 'knowledge_freshness'
  | 'model_fingerprint'
  | 'reasoning_stability'
  | 'structure_compliance'
  | 'parameter_fidelity'
  | 'instruction_hierarchy'
  | 'protocol_schema'
  | 'stream_completeness'
export type QualitySourceState = 'confirmed' | 'inferred' | 'insufficient_evidence'
export type QualityEvidenceCode =
  | 'within_policy_bounds'
  | 'coverage_insufficient'
  | 'fingerprint_matched'
  | 'fingerprint_mismatch'
  | 'reasoning_variance'
  | 'structure_violation'
  | 'parameter_deviation'
  | 'instruction_violation'
  | 'protocol_violation'
  | 'stream_incomplete'
  | 'source_confirmed'
  | 'source_inferred'
  | 'source_insufficient_evidence'

export interface ModelQualityDimensionResult {
  key: QualityDimension
  score: number
  status: QualityConclusion
  sample_count: number
  confidence: number
  stable_baseline_delta_pp?: number
  reference_baseline_delta_pp?: number
  checked_at: string
  evidence_code: QualityEvidenceCode
}

export interface ModelQualitySourceAttribution {
  state: QualitySourceState
  display_name?: string
  confidence?: number
  coverage?: number
  alternate_candidates?: Array<{ display_name: string; confidence: number }>
  evidence_code: QualityEvidenceCode
}

export interface ModelQualityEvidence {
  dimension_key?: QualityDimension
  code: QualityEvidenceCode
}

export interface ModelQualityReport {
  model_alias: string
  overall_conclusion: QualityConclusion
  adulteration_risk: QualityConclusion
  degradation_risk: QualityConclusion
  generated_at: string
  fresh_until: string
  dimension_results: ModelQualityDimensionResult[]
  source_attribution: ModelQualitySourceAttribution
  evidence: ModelQualityEvidence[]
}

export interface RadarPublicHealth {
  model_alias: string
  health_state: 'healthy' | 'degraded' | 'insufficient_evidence'
  overall_conclusion?: QualityConclusion
  adulteration_risk?: QualityConclusion
  degradation_risk?: QualityConclusion
  checked_at?: string
  freshness?: string
  p99_ms?: number
  error_rate?: number
}

export async function getModelHealth(params?: { model?: string }): Promise<RadarPublicHealth[]> {
  const { data } = await apiClient.get<RadarPublicHealth[]>('/radar/health', { params })
  return data
}

export async function getModelQualityReport(modelAlias: string): Promise<ModelQualityReport> {
  const { data } = await apiClient.get<ModelQualityReport>(`/radar/models/${encodeURIComponent(modelAlias)}/quality-report`)
  return data
}
