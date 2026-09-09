import { defineConfig, devices } from '@playwright/test'
import { requireLoopbackOrigin } from './e2e/support/environment'

const baseURL = requireLoopbackOrigin(process.env.RADAR_E2E_BASE_URL ?? '')

export default defineConfig({
  testDir: './e2e',
  outputDir: './test-results/artifacts',
  globalSetup: './e2e/support/environment',
  retries: 1,
  reporter: [
    ['list'],
    ['json', { outputFile: 'test-results/playwright.json' }],
    ['junit', { outputFile: 'test-results/playwright.xml' }]
  ],
  use: { baseURL, trace: 'on-first-retry', screenshot: 'only-on-failure', serviceWorkers: 'block' },
  projects: [
    { name: 'auth-setup', testMatch: /auth\.setup\.ts/, use: { trace: 'off', screenshot: 'off', serviceWorkers: 'block' } },
    { name: 'chromium-desktop', testIgnore: [/auth\.setup\.ts/, /support\/environment\.spec\.ts/], use: { ...devices['Desktop Chrome'], trace: 'on-first-retry', screenshot: 'only-on-failure', serviceWorkers: 'block' }, dependencies: ['auth-setup'] },
    { name: 'chromium-mobile', testIgnore: [/auth\.setup\.ts/, /support\/environment\.spec\.ts/], use: { ...devices['Pixel 7'], trace: 'on-first-retry', screenshot: 'only-on-failure', serviceWorkers: 'block' }, dependencies: ['auth-setup'] }
  ]
})
