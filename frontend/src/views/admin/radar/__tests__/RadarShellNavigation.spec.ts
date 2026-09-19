import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

import enRadar from '@/i18n/locales/en/admin/radar'
import zhRadar from '@/i18n/locales/zh/admin/radar'
import enCommon from '@/i18n/locales/en/common'
import zhCommon from '@/i18n/locales/zh/common'

const dir = dirname(fileURLToPath(import.meta.url))
const shellSource = readFileSync(resolve(dir, '../RadarShell.vue'), 'utf8')
const routerSource = readFileSync(resolve(dir, '../../../../router/index.ts'), 'utf8')

describe('Radar shell navigation', () => {
  it('keeps the control panel shell and uses localized route tabs', () => {
    expect(shellSource).toContain('<AppLayout>')
    expect(shellSource).toContain("import AppLayout from '@/components/layout/AppLayout.vue'")
    expect(shellSource).toContain("t('admin.radar.title')")
    expect(shellSource).toContain("t('admin.radar.pages.overview')")
    expect(shellSource).toContain("t('admin.radar.pages.datasets')")
    expect(shellSource).toContain('activeSection === tab.section')
    expect(shellSource).not.toContain("section: 'alerts'")
    expect(shellSource).not.toContain("section: 'gates'")
    expect(shellSource).not.toContain("section: 'workers'")
    expect(shellSource).not.toContain("raw === 'alerts'")
    expect(shellSource).not.toContain("raw === 'gates'")
    expect(shellSource).not.toContain("raw === 'workers'")
  })

  it('uses localized document titles for every Radar route', () => {
    expect(routerSource).toContain("titleKey: 'admin.radar.title'")
    for (const section of ['overview', 'models', 'runs', 'alerts', 'gates', 'workers', 'datasets']) {
      expect(routerSource).toContain(`titleKey: 'admin.radar.pages.${section}'`)
    }
  })

  it('provides complete Chinese navigation, table, domain and status labels', () => {
    expect(zhCommon.nav.qualityRadar).toBe('质量雷达')
    expect(enCommon.nav.qualityRadar).toBe('Quality Radar')
    expect(zhRadar.radar.pages).toEqual({
      overview: '概览',
      models: '模型',
      runs: '评测运行',
      alerts: '告警',
      gates: '发布门禁',
      workers: '执行器',
      datasets: '数据集',
    })
    expect(zhRadar.radar.table).toMatchObject({
      model: '模型',
      domain: '能力域',
      health: '健康状态',
      status: '状态',
    })
    expect(zhRadar.radar.domains).toMatchObject({
      aggregate: '综合',
      coding: '编程',
      reasoning: '推理',
      long_context: '长上下文',
      tool_call: '工具调用',
    })
    expect(zhRadar.radar.status).toMatchObject({
      healthy: '健康',
      degraded: '降级',
      blocked: '已阻止',
      insufficient_evidence: '证据不足',
    })
    expect(zhRadar.radar.pages).not.toEqual(enRadar.radar.pages)
  })
})
