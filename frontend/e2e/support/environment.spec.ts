import { mkdirSync, mkdtempSync, rmSync, statSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  ensurePrivateDirectory,
  requireLoopbackOrigin,
  requireMatchingIdentity,
  requireSameOriginURL,
  requireUnitIntervalConfidence
} from './environment'

describe('requireLoopbackOrigin', () => {
  it.each([
    ['http://127.0.0.1:18080', 'http://127.0.0.1:18080'],
    ['http://localhost:18080/', 'http://localhost:18080'],
    ['http://[::1]:18080', 'http://[::1]:18080']
  ])('accepts loopback origin %s', (input, expected) => {
    expect(requireLoopbackOrigin(input)).toBe(expected)
  })

  it.each([
    '',
    'https://sub2api.weihub.cloud',
    'http://192.255.134.229',
    'https://127.0.0.1:18080',
    'http://0.0.0.0:18080',
    'http://127.0.0.1:0',
    'http://user:secret@127.0.0.1:18080',
    'http://127.0.0.1:18080/path',
    'http://127.0.0.1:18080/?query=1',
    'http://127.0.0.1:18080/#fragment'
  ])('rejects value outside the loopback-origin boundary: %s', (input) => {
    expect(() => requireLoopbackOrigin(input)).toThrow(/loopback HTTP origin/)
  })
})

describe('requireSameOriginURL', () => {
  it.each([
    ['/dashboard', 'http://127.0.0.1:18080/dashboard'],
    ['api/v1/auth/me', 'http://127.0.0.1:18080/api/v1/auth/me'],
    ['http://127.0.0.1:18080/model-health', 'http://127.0.0.1:18080/model-health']
  ])('accepts same-origin URL %s', (input, expected) => {
    expect(requireSameOriginURL(input, 'http://127.0.0.1:18080')).toBe(expected)
  })

  it.each([
    'https://example.com/resource.js',
    'https://127.0.0.1:18080/dashboard',
    'http://127.0.0.1:18081/dashboard',
    'https://sub2api.weihub.cloud',
    'http://192.255.134.229'
  ])('rejects URL outside the configured origin: %s', (input) => {
    expect(() => requireSameOriginURL(input, 'http://127.0.0.1:18080'))
      .toThrow(/configured loopback origin/)
  })
})

describe('ensurePrivateDirectory', () => {
  it('creates and tightens the artifact directory to mode 0700', () => {
    const temporaryRoot = mkdtempSync(path.join(tmpdir(), 'radar-artifacts-'))
    const artifactDirectory = path.join(temporaryRoot, 'artifacts')
    try {
      mkdirSync(artifactDirectory, { mode: 0o755 })
      expect(ensurePrivateDirectory(artifactDirectory)).toBe(artifactDirectory)
      expect(statSync(artifactDirectory).mode & 0o777).toBe(0o700)
    } finally {
      rmSync(temporaryRoot, { recursive: true, force: true })
    }
  })
})

describe('requireMatchingIdentity', () => {
  it('accepts identities that match after email normalization', () => {
    expect(() => requireMatchingIdentity(' User@Example.Invalid ', 'user@example.invalid'))
      .not.toThrow()
  })

  it('rejects a mismatch without exposing either identity', () => {
    let message = ''
    try {
      requireMatchingIdentity('first@example.invalid', 'second@example.invalid')
    } catch (error) {
      message = error instanceof Error ? error.message : String(error)
    }
    expect(message).toBe('fixture identity does not match configured identity')
    expect(message).not.toContain('first@example.invalid')
    expect(message).not.toContain('second@example.invalid')
  })
})

describe('requireUnitIntervalConfidence', () => {
  it.each([0, 0.5, 1])('accepts finite confidence %s', (value) => {
    expect(requireUnitIntervalConfidence(value)).toBe(value)
  })

  it.each([-0.01, 1.01, Number.NaN, Number.POSITIVE_INFINITY, '0.5', null])(
    'rejects invalid confidence %s',
    (value) => {
      expect(() => requireUnitIntervalConfidence(value)).toThrow(/finite number from 0 to 1/)
    }
  )
})
