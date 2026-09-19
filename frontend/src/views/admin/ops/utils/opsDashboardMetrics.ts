import type {
  OpsSystemMetricsSnapshot,
  OpsTrafficBreakdown,
  OpsTrafficClass
} from '@/api/admin/ops'

export type { OpsTrafficBreakdown, OpsTrafficClass }

export type ResourceSeverity = 'ok' | 'warning' | 'critical' | 'unknown'

const TRAFFIC_CLASSES: OpsTrafficClass[] = ['production', 'metadata', 'synthetic', 'unknown']

function finiteNonNegativeInteger(value: unknown): number {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) return 0
  return Math.floor(value)
}

export function normalizeTrafficBreakdown(value?: Partial<Record<OpsTrafficClass, unknown>> | null): OpsTrafficBreakdown {
  return TRAFFIC_CLASSES.reduce((out, trafficClass) => {
    out[trafficClass] = finiteNonNegativeInteger(value?.[trafficClass])
    return out
  }, {} as OpsTrafficBreakdown)
}

function warningLevel(value: string | null | undefined): ResourceSeverity {
  const normalized = String(value || '').toLowerCase()
  if (!normalized) return 'unknown'
  if (normalized.split(',').some((item) => item.trim().startsWith('critical:'))) return 'critical'
  if (normalized.split(',').some((item) => item.trim().startsWith('warning:'))) return 'warning'
  return 'unknown'
}

export function getResourceSeverity(snapshot?: Partial<OpsSystemMetricsSnapshot> | null): ResourceSeverity {
  if (!snapshot) return 'unknown'

  let explicit = warningLevel(snapshot.resource_warning)
  if (explicit === 'critical') return explicit

  const available = snapshot.mem_available_mb
  if (typeof available === 'number' && Number.isFinite(available)) {
    if (available < 128) return 'critical'
    if (available < 256 && explicit !== 'warning') explicit = 'warning'
  }

  const disk = snapshot.disk_used_percent
  if (typeof disk === 'number' && Number.isFinite(disk)) {
    if (disk >= 90) return 'critical'
    if (disk >= 80 && explicit === 'unknown') explicit = 'warning'
  }

  const swapUsed = snapshot.swap_used_mb
  if (typeof swapUsed === 'number' && Number.isFinite(swapUsed) && swapUsed > 0 && explicit === 'unknown') {
    explicit = 'warning'
  }

  if (explicit !== 'unknown') return explicit

  const hasMetric = [available, disk, swapUsed, snapshot.swap_total_mb, snapshot.oom_kill_count].some(
    (value) => typeof value === 'number' && Number.isFinite(value)
  )
  return hasMetric ? 'ok' : 'unknown'
}
