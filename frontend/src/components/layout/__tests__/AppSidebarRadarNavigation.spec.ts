import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

const dir = dirname(fileURLToPath(import.meta.url))
const sidebarSource = readFileSync(resolve(dir, '../AppSidebar.vue'), 'utf8')

const radarChildren = [
  ['/admin/radar', 'overview'],
  ['/admin/radar/models', 'models'],
  ['/admin/radar/runs', 'runs'],
  ['/admin/radar/alerts', 'alerts'],
  ['/admin/radar/gates', 'gates'],
  ['/admin/radar/workers', 'workers'],
  ['/admin/radar/datasets', 'datasets'],
] as const

describe('AppSidebar radar navigation', () => {
  it('assigns distinct icons to quality radar and model health', () => {
    expect(sidebarSource).toContain('const RadarIcon =')
    expect(sidebarSource).toContain('const ActivityIcon =')
    expect(sidebarSource).toMatch(/path: '\/model-health', label: t\('modelHealth\.title'\), icon: ActivityIcon/)
    expect(sidebarSource).toMatch(/path: '\/admin\/radar-group',[\s\S]*?icon: RadarIcon/)
    expect(sidebarSource).toContain("path: '/monitor', label: t('nav.channelStatus'), icon: SignalIcon")
  })

  it('localizes the model health entry through the shared model health title', () => {
    expect(sidebarSource).toContain("path: '/model-health', label: t('modelHealth.title')")
    expect(sidebarSource).not.toContain("path: '/model-health', label: 'Model Health'")
  })

  it('renders Radar as a localized expandable group with all management pages', () => {
    expect(sidebarSource).toContain("path: '/admin/radar-group'")
    expect(sidebarSource).toContain("label: t('nav.qualityRadar')")
    expect(sidebarSource).toContain('expandOnly: true')

    for (const [path, labelKey] of radarChildren) {
      expect(sidebarSource).toContain(`path: '${path}', label: t('admin.radar.pages.${labelKey}')`)
    }

    expect(sidebarSource).not.toContain("label: 'Quality Radar'")
  })

  it('keeps child selection exact and lets an active group be collapsed', () => {
    expect(sidebarSource).toContain("route.path === child.path")
    expect(sidebarSource).toContain('groupExpansionOverrides')
    expect(sidebarSource).toContain('groupExpansionOverrides.value.get(item.path)')
  })
})
