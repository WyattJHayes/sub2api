import { test } from 'node:test'
import assert from 'node:assert/strict'
import { auditSource, documentDigest } from './price-audit.mjs'

const document = '# Published pricing\n' + 'model-a input USD 1 output USD 4\n'.repeat(10)
const source = { id: 'provider', url: 'https://platform.kimi.ai/docs/pricing/chat.md', marker: 'model-a', sha256: '' }
const response = (body, status = 200) => async () => new Response(body, { status })

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
