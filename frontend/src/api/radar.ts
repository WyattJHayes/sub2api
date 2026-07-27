import { apiClient } from './client'
import type { RadarModelHealth } from './admin/radar'

export interface RadarPublicHealth {
  model_alias: string
  health_state: 'healthy' | 'degraded' | 'insufficient_evidence'
  public_domain_scores?: RadarModelHealth[]
  sample_count?: number
  freshness?: string
  p99_ms?: number
  error_rate?: number
}

export async function getModelHealth(params?: { model?: string }): Promise<RadarPublicHealth[]> {
  const { data } = await apiClient.get<RadarPublicHealth[]>('/radar/health', { params })
  return data
}
