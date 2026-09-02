import { chmodSync, lstatSync, mkdirSync, statSync } from 'node:fs'
import path from 'node:path'
import type { BrowserContext } from '@playwright/test'

const LOOPBACK_HOSTS = new Set(['127.0.0.1', 'localhost', '[::1]'])

export function requireLoopbackOrigin(value: string): string {
  let url: URL
  try {
    url = new URL(value)
  } catch {
    throw new Error('RADAR_E2E_BASE_URL must be a loopback HTTP origin')
  }

  const port = url.port === '' ? 80 : Number(url.port)
  const valid = url.protocol === 'http:'
    && LOOPBACK_HOSTS.has(url.hostname.toLowerCase())
    && Number.isInteger(port)
    && port >= 1
    && port <= 65535
    && url.username === ''
    && url.password === ''
    && (url.pathname === '' || url.pathname === '/')
    && url.search === ''
    && url.hash === ''
  if (!valid) {
    throw new Error('RADAR_E2E_BASE_URL must be a loopback HTTP origin')
  }
  return url.origin
}

export function requireSameOriginURL(value: string, baseOrigin: string): string {
  const allowedOrigin = requireLoopbackOrigin(baseOrigin)
  let url: URL
  try {
    url = new URL(value, `${allowedOrigin}/`)
  } catch {
    throw new Error('request URL must use the configured loopback origin')
  }
  if (url.origin !== allowedOrigin || url.username !== '' || url.password !== '') {
    throw new Error('request URL must use the configured loopback origin')
  }
  return url.href
}

export function ensurePrivateDirectory(directory: string): string {
  mkdirSync(directory, { recursive: true, mode: 0o700 })
  const linkMetadata = lstatSync(directory)
  if (linkMetadata.isSymbolicLink() || !linkMetadata.isDirectory()) {
    throw new Error('artifact path must be a private directory')
  }
  chmodSync(directory, 0o700)
  if ((statSync(directory).mode & 0o777) !== 0o700) {
    throw new Error('artifact directory must use mode 0700')
  }
  return directory
}

export default async function globalSetup(): Promise<void> {
  ensurePrivateDirectory(path.resolve('test-results/artifacts'))
}

export function requireMatchingIdentity(fixtureIdentity: string, configuredIdentity: string): void {
  const normalize = (value: string) => value.trim().toLowerCase()
  if (normalize(fixtureIdentity) !== normalize(configuredIdentity)) {
    throw new Error('fixture identity does not match configured identity')
  }
}

export function requireUnitIntervalConfidence(value: unknown): number {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0 || value > 1) {
    throw new Error('source attribution confidence must be a finite number from 0 to 1')
  }
  return value
}

export async function installLoopbackOriginGuard(
  context: BrowserContext,
  baseOrigin: string
): Promise<void> {
  const allowedOrigin = requireLoopbackOrigin(baseOrigin)
  await context.route('**/*', async (route) => {
    const requestURL = route.request().url()
    let protocol: string
    try {
      protocol = new URL(requestURL).protocol
    } catch {
      await route.abort('blockedbyclient')
      return
    }
    if (protocol !== 'http:' && protocol !== 'https:') {
      await route.continue()
      return
    }
    try {
      requireSameOriginURL(requestURL, allowedOrigin)
      await route.continue()
    } catch {
      await route.abort('blockedbyclient')
    }
  })
}
