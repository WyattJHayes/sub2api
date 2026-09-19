import path from 'node:path'
import { expect, test } from '@playwright/test'
import { installLoopbackOriginGuard, requireLoopbackOrigin } from './support/environment'

const USER_STORAGE_STATE = path.resolve('test-results/.auth/user.json')
const WATERED_ALIAS = 'radar-quality-watered'
const BASE_ORIGIN = requireLoopbackOrigin(process.env.RADAR_E2E_BASE_URL ?? '')

test.use({ storageState: USER_STORAGE_STATE })
test.beforeEach(async ({ context }) => {
  await installLoopbackOriginGuard(context, BASE_ORIGIN)
})

test('@smoke embedded static server preserves the model-quality deep link', async ({ page }) => {
  const reportResponse = page.waitForResponse((response) => {
    const url = new URL(response.url())
    return url.origin === BASE_ORIGIN
      && url.pathname === `/api/v1/radar/models/${WATERED_ALIAS}/quality-report`
      && response.request().method() === 'GET'
  })

  const documentResponse = await page.goto(`/model-health/${WATERED_ALIAS}`)
  expect(documentResponse).not.toBeNull()
  if (!documentResponse) throw new Error('deep-link navigation did not return a document response')
  const documentURL = new URL(documentResponse.url())
  expect(documentURL.origin).toBe(BASE_ORIGIN)
  expect(documentURL.pathname).toBe(`/model-health/${WATERED_ALIAS}`)
  expect(documentResponse.ok()).toBeTruthy()
  expect(documentResponse.headers()['content-type']).toContain('text/html')

  const response = await reportResponse
  expect(response.status()).toBe(200)
  expect(new URL(response.url()).origin).toBe(BASE_ORIGIN)
  await expect(page.getByRole('main').getByRole('heading', { name: '模型质量报告', exact: true })).toBeVisible()
  await expect(page.getByText(WATERED_ALIAS, { exact: true }).first()).toBeVisible()
})
