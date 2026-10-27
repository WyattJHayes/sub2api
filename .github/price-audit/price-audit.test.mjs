import { test } from 'node:test'
import assert from 'node:assert/strict'
import { auditSource, documentDigest } from './price-audit.mjs'

const document = '# Published pricing\n' + 'model-a input USD 1 output USD 4\n'.repeat(10)
const source = { id: 'provider', url: 'https://platform.kimi.ai/docs/pricing/chat.md', marker: 'model-a', sha256: '' }
const response = (body, status = 200) => async () => new Response(body, { status })
const now = new Date('2026-10-27T14:00:00Z')
const acknowledged = {
  ...source, sha256: documentDigest(document),
  unverifiedPricing: {
    reason: 'pricing-table-unavailable', reviewedAt: '2026-10-27', expiresAt: '2026-11-03',
    models: ['Kimi-K3', 'Kimi-K2.6', 'Kimi-K2.7-code']
  }
}

test('keeps an explicit warning for an unchanged, temporarily acknowledged unverified source', async () => {
  const result = await auditSource(acknowledged, response(document), now)
  assert.equal(result.status, 'warning')
  assert.deepEqual(result.models, ['Kimi-K3', 'Kimi-K2.6', 'Kimi-K2.7-code'])
  assert.equal(result.expiresAt, '2026-11-03')
})

test('a known pricing warning never accepts new documents or fetch failures', async () => {
  assert.equal((await auditSource(acknowledged, response(document.replaceAll('USD 4', 'USD 5')), now)).status, 'changed')
  assert.equal((await auditSource(acknowledged, response('Forbidden', 403), now)).status, 'error')
})

test('expires a pricing acknowledgment and rejects invalid or indefinite acknowledgments', async () => {
  assert.equal((await auditSource(acknowledged, response(document), new Date('2026-11-03T00:00:00Z'))).status, 'error')
  for (const invalid of [
    { expiresAt: '2026-12-03' }, { reviewedAt: '2026-02-30' }, { expiresAt: 'invalid' },
    { reason: 'ignore-errors' }, { models: [] }, { models: ['bad\n::warning::injection'] }
  ]) {
    const item = { ...acknowledged, unverifiedPricing: { ...acknowledged.unverifiedPricing, ...invalid } }
    assert.equal((await auditSource(item, response(document), now)).status, 'error')
  }
})

test('audits the official xAI public pricing page and still rejects credential URLs', async () => {
  const xai = { ...source, url: 'https://docs.x.ai/developers/pricing.md', sha256: documentDigest(document) }
  assert.equal((await auditSource(xai, response(document))).status, 'unchanged')
  const neverFetch = async () => { assert.fail('credential URL reached the network') }
  assert.equal((await auditSource({ ...xai, url: xai.url + '?token=secret' }, neverFetch)).status, 'error')
})

test('ignores xAI media table ordering while detecting text prices and missing text sections', async () => {
  const text = '### Text API Pricing\n\n| Model | Input | Output |\n| grok-4.7 | $2 | $6 |\n\n'
  const media = '### Imagine Pricing\n\n| image-a | $0.02 |\n| image-b | $0.04 |\n'
  const body = '# Pricing\n\n' + 'Public rates per million tokens.\n'.repeat(4) + text + media
  const pricing = { ...source, url: 'https://docs.x.ai/developers/pricing.md', marker: 'grok-4.7', format: 'xai-text-api', sha256: documentDigest(text) }
  const reordered = body.replace('| image-a | $0.02 |\n| image-b | $0.04 |', '| image-b | $0.04 |\n| image-a | $0.02 |')
  assert.equal((await auditSource(pricing, response(reordered))).status, 'unchanged')
  assert.equal((await auditSource(pricing, response(body.replace('| grok-4.7 | $2 | $6 |', '| grok-4.7 | $3 | $6 |')))).status, 'changed')
  assert.equal((await auditSource(pricing, response(body.replace('### Text API Pricing', '### Other pricing')))).status, 'error')
})

test('reports a changed price document without accepting a new baseline', async () => {
  const baseline = { ...source, sha256: documentDigest(document) }
  assert.equal((await auditSource(baseline, response(document))).status, 'unchanged')
  const changed = await auditSource(baseline, response(document.replaceAll('USD 4', 'USD 5')))
  assert.equal(changed.status, 'changed')
  assert.equal(baseline.sha256, documentDigest(document))
})

test('fails explicitly for blocked or invalid documentation instead of recording a price change', async () => {
  assert.equal((await auditSource(source, response('Forbidden', 403))).status, 'error')
  assert.equal((await auditSource(source, response('<html>Sign in</html>'))).status, 'error')
  assert.equal((await auditSource(source, async () => { throw new Error('network unavailable') })).status, 'error')
})

test('rejects credential URLs and non-official hosts before making a request', async () => {
  const neverFetch = async () => { assert.fail('unsafe URL reached the network') }
  for (const url of ['https://example.com/pricing', 'https://platform.kimi.ai/docs?token=secret', 'http://platform.kimi.ai/docs']) {
    assert.equal((await auditSource({ ...source, url }, neverFetch)).status, 'error')
  }
})

test('ignores dynamic HTML outside the pricing article but detects a changed price inside it', async () => {
  const html = '<html><script>nonce="first"</script><article class="product-description markdown-body"><p>' + document + '</p></article></html>'
  const baseline = { ...source, url: 'https://www.alibabacloud.com/help/en/model-studio/example', format: 'html-article', sha256: documentDigest(html, 'html-article') }
  assert.equal((await auditSource(baseline, response(html.replace('first', 'second')))).status, 'unchanged')
  assert.equal((await auditSource(baseline, response(html.replaceAll('USD 4', 'USD 5')))).status, 'changed')
  assert.equal((await auditSource(baseline, response(document))).status, 'error')
})
