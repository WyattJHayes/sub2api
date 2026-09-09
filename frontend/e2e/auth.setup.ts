import { statSync } from 'node:fs'
import { chmod, mkdir } from 'node:fs/promises'
import path from 'node:path'
import { expect, test as setup, type Page } from '@playwright/test'
import {
  installLoopbackOriginGuard,
  requireLoopbackOrigin,
  requireSameOriginURL
} from './support/environment'

const AUTH_DIRECTORY = path.resolve('test-results/.auth')
export const ADMIN_STORAGE_STATE = path.join(AUTH_DIRECTORY, 'admin.json')
export const USER_STORAGE_STATE = path.join(AUTH_DIRECTORY, 'user.json')

const REQUIRED_ENVIRONMENT = [
  'RADAR_E2E_BASE_URL',
  'RADAR_E2E_ADMIN_EMAIL',
  'RADAR_E2E_ADMIN_PASSWORD',
  'RADAR_E2E_USER_EMAIL',
  'RADAR_E2E_USER_PASSWORD',
  'RADAR_E2E_FIXTURE_MANIFEST'
] as const

type RequiredEnvironmentName = typeof REQUIRED_ENVIRONMENT[number]

function requireEnvironment(name: RequiredEnvironmentName): string {
  const value = process.env[name]
  if (!value) throw new Error(`${name} is required`)
  return value
}

const environment = Object.fromEntries(
  REQUIRED_ENVIRONMENT.map((name) => [name, requireEnvironment(name)])
) as Record<RequiredEnvironmentName, string>
const BASE_ORIGIN = requireLoopbackOrigin(environment.RADAR_E2E_BASE_URL)

setup('@artifact-contract artifact directory uses mode 0700 after runner cleanup', async () => {
  expect(statSync(path.resolve('test-results/artifacts')).mode & 0o777).toBe(0o700)
})

function asRecord(value: unknown): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error('authentication response must be an object')
  }
  return value as Record<string, unknown>
}

async function login(
  page: Page,
  email: string,
  password: string,
  expectedRole: 'admin' | 'user',
  statePath: string
) {
  await mkdir(AUTH_DIRECTORY, { recursive: true, mode: 0o700 })
  await chmod(AUTH_DIRECTORY, 0o700)

  await installLoopbackOriginGuard(page.context(), BASE_ORIGIN)
  await page.addInitScript(() => localStorage.setItem('sub2api_locale', 'zh'))
  await page.goto('/login')
  await page.locator('#email').fill(email)
  await page.locator('#password').fill(password)
  await page.locator('button[type="submit"]').click()
  await expect(page).toHaveURL(/\/dashboard/)

  const token = await page.evaluate(() => localStorage.getItem('auth_token'))
  if (!token) throw new Error('login did not create an authenticated session')
  expect(await page.evaluate(() => localStorage.getItem('sub2api_locale'))).toBe('zh')

  const requestURL = requireSameOriginURL('/api/v1/auth/me', BASE_ORIGIN)
  const response = await page.request.get(requestURL, {
    headers: { Authorization: `Bearer ${token}` },
    maxRedirects: 0
  })
  expect(response.status()).toBeGreaterThanOrEqual(200)
  expect(response.status()).toBeLessThan(300)
  const envelope = asRecord(await response.json())
  const currentUser = asRecord('data' in envelope ? envelope.data : envelope)
  expect(currentUser.role).toBe(expectedRole)

  await page.context().storageState({ path: statePath })
  await chmod(statePath, 0o600)
}

setup('authenticate administrator', async ({ page }) => {
  await login(
    page,
    environment.RADAR_E2E_ADMIN_EMAIL,
    environment.RADAR_E2E_ADMIN_PASSWORD,
    'admin',
    ADMIN_STORAGE_STATE
  )
})

setup('authenticate ordinary user', async ({ page }) => {
  await login(
    page,
    environment.RADAR_E2E_USER_EMAIL,
    environment.RADAR_E2E_USER_PASSWORD,
    'user',
    USER_STORAGE_STATE
  )
})
