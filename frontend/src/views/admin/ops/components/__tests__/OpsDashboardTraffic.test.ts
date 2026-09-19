import { describe, expect, it } from 'vitest'
import zhLocale from '@/i18n/locales/zh'
import enLocale from '@/i18n/locales/en'
import type { OpsSystemMetricsSnapshot } from '@/api/admin/ops'
import {
  getResourceSeverity,
  normalizeTrafficBreakdown,
  type OpsTrafficBreakdown
} from '../../utils/opsDashboardMetrics'

describe('Ops dashboard traffic and resource presentation', () => {
  it('normalizes all traffic classes without allowing invalid counts', () => {
    const input = {
      production: 12,
      metadata: -4,
      synthetic: Number.NaN,
      unknown: 3.8
    }

    expect(normalizeTrafficBreakdown(input)).toEqual<OpsTrafficBreakdown>({
      production: 12,
      metadata: 0,
      synthetic: 0,
      unknown: 3
    })
  })

  it('prioritizes explicit critical resource warnings over lower signals', () => {
    const available = 512
    const swapUsed = 0
    const diskUsed = 71
    const snapshot: OpsSystemMetricsSnapshot = {
      id: 1,
      created_at: '2026-08-27T00:00:00Z',
      window_minutes: 1,
      mem_available_mb: available,
      swap_used_mb: swapUsed,
      swap_total_mb: 1024,
      disk_used_percent: diskUsed,
      oom_kill_count: 0,
      resource_warning: 'critical:oom_kill'
    }

    expect(getResourceSeverity(snapshot)).toBe('critical')
  })

  it('maps low memory, swap use, and disk pressure to visible warning levels', () => {
    expect(getResourceSeverity({ mem_available_mb: 64 })).toBe('critical')
    expect(getResourceSeverity({ swap_used_mb: 1, swap_total_mb: 1024 })).toBe('warning')
    expect(getResourceSeverity({ disk_used_percent: 91 })).toBe('critical')
    expect(getResourceSeverity({})).toBe('unknown')
  })

  it.each([
    ['zh', zhLocale, '生产流量'],
    ['en', enLocale, 'Production']
  ])('defines localized traffic labels for %s', (_name, locale, expected) => {
    expect(locale.admin.ops.traffic.production).toBe(expected)
    expect(locale.admin.ops.traffic.metadata).toBeTruthy()
    expect(locale.admin.ops.traffic.synthetic).toBeTruthy()
    expect(locale.admin.ops.traffic.unknown).toBeTruthy()
  })
})
