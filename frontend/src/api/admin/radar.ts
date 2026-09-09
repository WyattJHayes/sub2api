import { apiClient } from '../client'
import type { QualityDimension } from '../radar'

export type RadarDecisionStatus = 'recorded' | 'passed' | 'blocked' | 'review_required' | 'insufficient_evidence' | 'waived'
export type RadarAlertStatus = 'open' | 'acknowledged' | 'resolved'

export interface RadarOverview {
  freshness?: string
  calibration?: { enforcement_starts_at?: string; observation_days?: number; days_observed?: number }
  models?: RadarModelHealth[]
  alerts?: RadarAlert[]
  gates?: RadarGate[]
  workers?: RadarWorker[]
  summary?: { models?: number; open_alerts?: number; blocked_gates?: number; healthy_workers?: number }
}
export interface RadarModelHealth {
  model_alias?: string
  model_route: string
  capability_domain?: string
  health_state?: 'healthy' | 'degraded' | 'blocked' | 'insufficient_evidence'
  baseline_score?: number
  candidate_score?: number
  delta_pp?: number
  ci_low_pp?: number
  ci_high_pp?: number
  sample_count?: number
  freshness?: string
  p99_ms?: number
  error_rate?: number
}
export interface RadarTrackedModel {
  model_alias: string
  created_by: number
  created_at: string
}
export interface RadarAlert {
  id: string
  model_route: string
  capability_domain: string
  cause: string
  severity?: 'P0' | 'P1' | 'P2'
  status: RadarAlertStatus
  attribution_confidence?: number
  first_seen_at?: string
}
export interface RadarGate {
  id: string
  model_route?: string
  run_id?: string
  status: RadarDecisionStatus
  rule_id?: string
  created_at?: string
}
export interface RadarWorker {
  id: string
  name: string
  worker_kind: 'runner' | 'grader' | 'statistics'
  status: 'active' | 'disabled' | 'stale'
  last_heartbeat_at?: string
  capabilities?: string[]
}
export interface RadarDataset {
  id: string
  name?: string
  dataset_key?: string
  version?: string
  status?: string
  cases?: number
  case_count?: number
  manifest_sha256?: string
  source_type?: string
  created_by?: number
  tenant_id?: number
  created_at?: string
}
export interface RadarRun {
  id: string
  plan_id: string
  trigger_source?: string
  status: string
  budget_limit?: string
  reserved_cost?: string
  created_at?: string
  started_at?: string
  finished_at?: string
}
export interface CreateRadarCasePayload {
  case_key: string
  capability_domain: string
  priority: string
  weight: string
  sample_count: number
  prompt_spec: unknown
  expected_spec: unknown
  execution_spec: Record<string, unknown>
  grader_id: string
  grader_version: string
  confidentiality: string
  estimated_cost?: string
  quality_dimension?: QualityDimension
  quality_probe_spec?: {
    schema_version: 'quality-v1'
    quality_dimension: QualityDimension
    event_class: 'request_shape' | 'response_shape' | 'stream_integrity' | 'parameter_echo' | 'fingerprint'
    minimum_samples: number
    source_candidate?: { display_name: string; confidence: number }
  }
}
export interface CreateRadarDatasetPayload {
  dataset_key: string
  version: string
  source_type: string
  cases: CreateRadarCasePayload[]
}
export interface RadarPlan {
  id: string
  name: string
  dataset_version_id: string
  gateway_api_key_id: number
  trigger_type: string
  model_matrix: Array<Record<string, unknown>>
  max_run_cost: string
  daily_cost_limit: string
  max_concurrency: number
  enabled?: boolean
  created_at?: string
}
export interface CreateRadarPlanPayload {
  name: string
  dataset_version_id: string
  gateway_api_key_id: number
  trigger_type: string
  model_matrix: Array<Record<string, unknown>>
  max_run_cost: string
  daily_cost_limit: string
  max_concurrency: number
}
export interface StartRadarRunPayload {
  plan_id: string
  trigger_source: string
  baseline_ref?: Record<string, unknown>
  candidate_ref?: Record<string, unknown>
}

async function get<T>(path: string, params?: Record<string, unknown>): Promise<T> {
  const { data } = await apiClient.get<T>(path, { params })
  return data
}

export const radarAdminAPI = {
  overview: () => get<RadarOverview>('/admin/radar/overview'),
  models: (params?: Record<string, unknown>) => get<RadarModelHealth[]>('/admin/radar/models', params),
  registerModel: (payload: { model_alias: string }) => apiClient.post<RadarTrackedModel>('/admin/radar/models', payload).then((response) => response.data),
	untrackModel: (modelAlias: string) =>
		apiClient.delete<{ model_alias: string; status: 'untracked' }>(`/admin/radar/models/${encodeURIComponent(modelAlias)}`).then((response) => response.data),
  runs: (params?: Record<string, unknown>) => get<RadarRun[]>('/admin/radar/runs', params),
  alerts: (params?: Record<string, unknown>) => get<RadarAlert[]>('/admin/radar/alerts', params),
  gates: (params?: Record<string, unknown>) => get<RadarGate[]>('/admin/radar/gates', params),
  workers: () => get<RadarWorker[]>('/admin/radar/workers'),
  datasets: () => get<RadarDataset[]>('/admin/radar/datasets'),
  createDataset: (payload: CreateRadarDatasetPayload) => apiClient.post<RadarDataset>('/admin/radar/datasets', payload).then((response) => response.data),
  publishDataset: (id: string) => apiClient.post<RadarDataset>(`/admin/radar/datasets/${id}/publish`).then((response) => response.data),
  createPlan: (payload: CreateRadarPlanPayload) => apiClient.post<RadarPlan>('/admin/radar/plans', payload).then((response) => response.data),
  enableEvaluationKey: (id: number) => apiClient.post(`/admin/radar/evaluation-keys/${id}/enable`).then((response) => response.data),
  startRun: (payload: StartRadarRunPayload) => apiClient.post<RadarRun>('/admin/radar/runs', payload).then((response) => response.data),
  evaluateGate: (payload: Record<string, unknown>) => apiClient.post<RadarGate>('/admin/radar/gates/evaluate', payload).then((response) => response.data)
}

export default radarAdminAPI
