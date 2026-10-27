import { createHash } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { pathToFileURL } from 'node:url'

const hosts = new Set(['developers.openai.com', 'platform.claude.com', 'platform.kimi.ai', 'docs.z.ai', 'docs.x.ai', 'api-docs.deepseek.com', 'ai.byteplus.com', 'www.alibabacloud.com'])
const maxBytes = 2 * 1024 * 1024

function officialUrl(value) {
  const url = new URL(value)
  if (url.protocol !== 'https:' || !hosts.has(url.hostname) || url.port || url.username || url.password || url.search || url.hash) throw new Error('URL is not an approved public pricing page')
  return url
}

export function documentDigest(document, format = 'text') {
  // Page changes require human review; a changed hash is not proof of a price change.
  if (format === 'html-article') {
    const article = document.match(/<article\b[^>]*class=["'][^"']*\bmarkdown-body\b[^"']*["'][^>]*>([\s\S]*?)<\/article>/i)?.[1]
    if (!article) throw new Error('Pricing article missing; manual verification required')
    document = article.replace(/<(script|style)\b[^>]*>[\s\S]*?<\/\1>/gi, '').replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ')
  } else if (format === 'xai-text-api') {
    // xAI randomizes unrelated media table rows. Monitor the complete text
    // pricing section, including rates, context thresholds and regional notes.
    const heading = /^### Text API Pricing[ \t]*\r?\n/m.exec(document)
    if (!heading) throw new Error('Pricing section missing; manual verification required')
    const content = document.slice(heading.index + heading[0].length)
    const nextHeading = content.search(/^#{1,3} /m)
    document = heading[0] + (nextHeading < 0 ? content : content.slice(0, nextHeading))
  } else if (format !== 'text') throw new Error('Invalid source format')
  return createHash('sha256').update(document.replace(/\r\n/g, '\n').trim()).digest('hex')
}

async function publicDocument(source, fetcher) {
  let url = officialUrl(source.url)
  for (let redirects = 0; redirects <= 3; redirects++) {
    const response = await fetcher(url, { redirect: 'manual', signal: AbortSignal.timeout(20000) })
    if ([301, 302, 303, 307, 308].includes(response.status)) {
      const location = response.headers.get('location')
      await response.body?.cancel()
      if (!location) throw new Error('Redirect has no location')
      url = officialUrl(new URL(location, url).href)
      continue
    }
    if (!response.ok) {
      await response.body?.cancel()
      throw new Error(`Documentation returned HTTP ${response.status}`)
    }
    if (!response.body) throw new Error('Empty document')
    const reader = response.body.getReader()
    const chunks = []
    let bytes = 0
    try {
      while (true) {
        const { done, value } = await reader.read()
        if (done) break
        bytes += value.byteLength
        if (bytes > maxBytes) throw new Error('Document exceeds the 2 MiB limit')
        chunks.push(value)
      }
    } finally {
      await reader.cancel()
    }
    const document = new TextDecoder('utf-8', { fatal: true }).decode(Buffer.concat(chunks))
    if (document.length < 200 || !document.toLowerCase().includes(source.marker.toLowerCase())) throw new Error('Pricing marker missing; document needs manual verification')
    return document
  }
  throw new Error('Too many redirects')
}

function validateAcknowledgment(value, now) {
  const date = text => typeof text === 'string' && /^\d{4}-\d{2}-\d{2}$/.test(text) &&
    Number.isFinite(Date.parse(text)) && new Date(text).toISOString().slice(0, 10) === text
  if (!value || value.reason !== 'pricing-table-unavailable' || !date(value.reviewedAt) || !date(value.expiresAt) ||
    !Array.isArray(value.models) || !value.models.length || value.models.length > 20 ||
    value.models.some(model => typeof model !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(model)) ||
    Date.parse(value.expiresAt) <= Date.parse(value.reviewedAt) ||
    Date.parse(value.expiresAt) - Date.parse(value.reviewedAt) > 7 * 86400000 ||
    !Number.isFinite(now.getTime()) || now < new Date(value.reviewedAt)) throw new Error('Invalid source pricing acknowledgment')
  if (now >= new Date(value.expiresAt)) throw new Error('Pricing verification acknowledgment expired')
}

export async function auditSource(source, fetcher = fetch, now = new Date()) {
  try {
    if (!/^[a-z0-9-]+$/.test(source.id) || typeof source.marker !== 'string' || !source.marker.trim()) throw new Error('Invalid source definition')
    if (source.unverifiedPricing !== undefined) validateAcknowledgment(source.unverifiedPricing, now)
    const digest = documentDigest(await publicDocument(source, fetcher), source.format)
    if (digest === source.sha256 && source.unverifiedPricing) {
      return { id: source.id, status: 'warning', sha256: digest, warning: 'Official numeric prices remain unverified',
        models: source.unverifiedPricing.models, expiresAt: source.unverifiedPricing.expiresAt }
    }
    return { id: source.id, status: digest === source.sha256 ? 'unchanged' : 'changed', sha256: digest }
  } catch (error) {
    // Never print document bodies, headers or arbitrary exception messages.
    return { id: source.id, status: 'error', error: error instanceof Error && /^(URL is|Documentation returned|Empty document|Document exceeds|Pricing marker|Pricing section|Pricing verification|Too many redirects|Redirect has|Invalid source)/.test(error.message) ? error.message : 'Could not read the public documentation' }
  }
}

async function main() {
  const capture = process.argv.includes('--capture')
  const manifest = JSON.parse(await readFile(new URL('./sources.json', import.meta.url), 'utf8'))
  if (!Array.isArray(manifest.sources) || !manifest.sources.length || manifest.sources.length > 64 ||
    new Set(manifest.sources.map(source => source.id)).size !== manifest.sources.length ||
    (!capture && manifest.sources.some(source => !/^[a-f0-9]{64}$/.test(source.sha256)))) throw new Error('Invalid manifest')
  const results = await Promise.all(manifest.sources.map(source => auditSource(source)))
  for (const result of results.filter(result => result.status === 'warning')) {
    process.stderr.write(JSON.stringify(result) + '\n')
    if (process.env.GITHUB_ACTIONS === 'true') process.stderr.write(`::warning title=Public prices remain unverified::${result.id}: ${result.models.join(', ')}; acknowledgment expires ${result.expiresAt}\n`)
  }
  if (capture) {
    for (const result of results) if (result.status === 'error') process.stderr.write(JSON.stringify(result) + '\n')
    const sources = manifest.sources.map((source, index) => ({ ...source, sha256: results[index].sha256 ?? source.sha256 }))
    process.stdout.write(JSON.stringify({ reviewed: new Date().toISOString().slice(0, 10), sources }, null, 2) + '\n')
  } else {
    process.stdout.write(JSON.stringify({ reviewed: manifest.reviewed, results }, null, 2) + '\n')
  }
  // Capture only proposes a baseline on stdout; it never writes accepted prices.
  if (results.some(result => result.status === 'error' || (!capture && !['unchanged', 'warning'].includes(result.status)))) process.exitCode = 1
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(() => { process.stderr.write('Invalid pricing audit manifest\n'); process.exitCode = 1 })
}
