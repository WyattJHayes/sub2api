import path from 'node:path'
import { expect, test } from '@playwright/test'
import { installLoopbackOriginGuard, requireLoopbackOrigin } from './support/environment'

const ADMIN_STORAGE_STATE = path.resolve('test-results/.auth/admin.json')
const USER_STORAGE_STATE = path.resolve('test-results/.auth/user.json')
const BASE_ORIGIN = requireLoopbackOrigin(process.env.RADAR_E2E_BASE_URL ?? '')

const ADMIN_ROUTES = [
  { route: '/admin/radar', api: '/api/v1/admin/radar/overview', body: ':scope > div.space-y-5', content: '模型健康', heading: false },
  { route: '/admin/radar/models', api: '/api/v1/admin/radar/models', body: ':scope > section', content: '跟踪模型', heading: true },
  { route: '/admin/radar/runs', api: '/api/v1/admin/radar/runs', body: ':scope > section', content: '评测运行', heading: true },
  { route: '/admin/radar/datasets', api: '/api/v1/admin/radar/datasets', body: ':scope > section', content: '评测数据集', heading: true },
  { route: '/admin/radar/alerts', api: '/api/v1/admin/radar/alerts', body: ':scope > section', content: '告警生命周期', heading: false },
  { route: '/admin/radar/gates', api: '/api/v1/admin/radar/gates', body: ':scope > section', content: '发布门禁', heading: false },
  { route: '/admin/radar/workers', api: '/api/v1/admin/radar/workers', body: ':scope > section', content: '执行器健康', heading: false }
] as const

test.beforeEach(async ({ context }) => {
  await installLoopbackOriginGuard(context, BASE_ORIGIN)
})

test.describe('administrator Radar routes', () => {
  test.use({ storageState: ADMIN_STORAGE_STATE })

  for (const { route, api, body, content, heading } of ADMIN_ROUTES) {
    test(`@smoke ${route} renders localized content after its Radar API succeeds`, async ({ page }) => {
      const radarResponse = page.waitForResponse((response) => {
        const url = new URL(response.url())
        return url.origin === BASE_ORIGIN
          && url.pathname === api
          && response.request().method() === 'GET'
      })

      await page.goto(route)
      const response = await radarResponse
      expect(response.ok()).toBeTruthy()
      const radarShell = page.locator('section.space-y-5').filter({
        has: page.getByRole('heading', { name: '质量雷达', exact: true })
      }).first()
      const routeBody = radarShell.locator(body)
      await expect(routeBody).toBeVisible()
      if (heading) {
        await expect(routeBody.getByRole('heading', { name: content, exact: true })).toBeVisible()
      } else {
        await expect(routeBody.getByText(content, { exact: true }).first()).toBeVisible()
      }
    })
  }
})

test.describe('ordinary-user Radar route guard', () => {
  test.use({ storageState: USER_STORAGE_STATE })

  test('@smoke denies the admin Radar route and keeps model health accessible', async ({ page }) => {
    const backendDenials: number[] = []
    page.on('response', (response) => {
      const url = new URL(response.url())
      if (url.origin === BASE_ORIGIN
        && url.pathname.startsWith('/api/v1/admin/radar')
        && (response.status() === 401 || response.status() === 403)) {
        backendDenials.push(response.status())
      }
    })

    await page.goto('/admin/radar')
    await expect.poll(() => {
      const redirected = !new URL(page.url()).pathname.startsWith('/admin/radar')
      return redirected || backendDenials.length > 0
    }).toBe(true)

    const healthResponse = page.waitForResponse((response) => {
      const url = new URL(response.url())
      return url.origin === BASE_ORIGIN
        && url.pathname === '/api/v1/radar/health'
        && response.request().method() === 'GET'
    })
    await page.goto('/model-health')
    expect((await healthResponse).ok()).toBeTruthy()
    await expect(page.getByRole('main').getByRole('heading', { name: '模型健康', exact: true })).toBeVisible()
  })
})
