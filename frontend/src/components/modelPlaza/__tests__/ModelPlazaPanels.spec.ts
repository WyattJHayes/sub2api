import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh'
import en from '@/i18n/locales/en'
import { i18n } from '@/i18n'
import ModelPlazaPanels from '../ModelPlazaPanels.vue'
import type { ModelPlazaResponse } from '@/api/modelPlaza'

// Match vite.config.ts: runtime i18n uses the CSP-safe JIT compiler.
vi.hoisted(() => { vi.stubGlobal('__INTLIFY_JIT_COMPILATION__', true) })
i18n.global.setLocaleMessage('en', en)

vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isAuthenticated: false }) }))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ cachedPublicSettings: null, showSuccess: vi.fn(), showError: vi.fn() })
}))

function render(locale = 'zh', referenceEnabled = true, error = false) {
  return mount(ModelPlazaPanels, {
    props: { response: { description: '', groups: [] }, loading: false, error, referenceEnabled },
    global: { plugins: [createI18n({ legacy: false, locale, messages: { zh, en } })] }
  })
}

describe('model plaza reference prices', () => {
  it('preserves sanitized administrator Markdown without displaying platform prices', async () => {
    const wrapper = render()
    await wrapper.setProps({ response: {
      description: '**计费说明**：汇率以结算页面为准。<img src="x" onerror="alert(1)"><script>alert(1)</script>',
      groups: []
    } })
    const notice = wrapper.get('[data-plaza-description]')
    expect(notice.get('strong').text()).toBe('计费说明')
    expect(notice.text()).toContain('汇率以结算页面为准')
    expect(notice.find('script').exists()).toBe(false)
    expect(notice.find('img').attributes('onerror')).toBeUndefined()
    expect(wrapper.findAll('article')).toHaveLength(33)
    expect(wrapper.find('[data-price-tab="actual"]').exists()).toBe(false)
    await wrapper.setProps({ referenceEnabled: false, error: true })
    expect(wrapper.find('[data-plaza-description]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('shows only website prices without configured groups or a platform-price tab', () => {
    const wrapper = render()
    expect(wrapper.findAll('article')).toHaveLength(33)
    expect(wrapper.text()).toContain('美元 / 百万 tokens')
    expect(wrapper.find('a[href="https://web.aijws.com/models"]').exists()).toBe(false)
    expect(wrapper.find('[data-price-tab="actual"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('平台实际价格')
  })

  it.each(['zh', 'en'])('shows the full price catalog without source information in %s', async (locale) => {
    const wrapper = render(locale)
    expect(wrapper.findAll('article')).toHaveLength(33)
    expect(wrapper.get('h2').text()).toBe(locale === 'zh' ? '模型价格' : 'Model prices')
    expect(wrapper.find('a[href*="aijws.com"]').exists()).toBe(false)
    expect(wrapper.text()).not.toMatch(/MY JARVIS|价格来源|采集日期|原站|外站|Captured:|Source:|Source note|Source rule|external/i)
    await wrapper.get('input[type="search"]').setValue('claude-fable-5-1')
    expect(wrapper.findAll('article')).toHaveLength(1)
    expect(wrapper.get('article').text()).toContain('$12.00')
    expect(wrapper.get('article').text()).toContain('$60.00')
    expect(wrapper.get('article').text()).toContain('$0.30')
    wrapper.unmount()
  })

  it('searches case-insensitively and intersects provider and model filters', async () => {
    const wrapper = render()
    await wrapper.get('input[type="search"]').setValue('  QWEN3.7-FLASH  ')
    expect(wrapper.findAll('article')).toHaveLength(1)
    const card = wrapper.get('article')
    expect(card.text()).toContain('>32K, ≤256K')
    expect(card.text()).toContain('$0.04')
    await wrapper.get('select').setValue('OpenAI')
    expect(wrapper.findAll('article')).toHaveLength(0)
    expect(wrapper.text()).toContain('没有匹配的模型')
  })

  it.each([
    { userRate: undefined },
    { userRate: 0.5 }
  ])('keeps website prices independent of group and user rates ($userRate)', async (tc) => {
    const response: ModelPlazaResponse = {
      description: '',
      groups: [{
        id: 10, name: 'Claude Max', description: '', platform: 'anthropic',
        subscription_type: 'standard', rate_multiplier: 1.2, user_rate_multiplier: tc.userRate,
        peak_rate_enabled: false, peak_start: '', peak_end: '', peak_rate_multiplier: 1,
        is_exclusive: false, image_rate_independent: false, image_rate_multiplier: 1,
        video_rate_independent: false, video_rate_multiplier: 1,
        long_context_pricing_enabled: false,
        models: [{
          name: 'claude-fable-5', platform: 'anthropic', official_pricing: null,
          pricing: {
            billing_mode: 'token', input_price: 12e-6, output_price: 60e-6,
            cache_read_price: 1.2e-6, cache_write_price: 15e-6, cache_write_1h_price: 24e-6,
            image_input_price: null, image_output_price: null, per_request_price: null,
            intervals: [], reasoning_effort_multipliers: { max: 3 }
          }
        }]
      }]
    }
    const wrapper = render()
    await wrapper.setProps({ response })
    expect(wrapper.find('[data-price-tab="actual"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Claude Max')
    await wrapper.get('input[type="search"]').setValue('claude-fable-5-1')
    const row = wrapper.get('tbody tr')
    expect(wrapper.get('article').text()).toContain('claude-fable-5-1')
    expect(row.text()).toContain('$12.00')
    expect(row.text()).toContain('$60.00')
    expect(row.text()).toContain('$0.30')
    expect(wrapper.find('[data-reasoning-effort="max"]').exists()).toBe(false)
    expect(wrapper.findAll('tbody tr')).toHaveLength(1)
    wrapper.unmount()
  })

  it('preserves small cache prices, missing values and whole-request context tiers', async () => {
    const wrapper = render()
    await wrapper.get('input[type="search"]').setValue('gpt-5.3-codex')
    expect(wrapper.get('article').text()).toContain('$0.175')
    expect(wrapper.get('article').text()).toContain('未提供')
    expect(wrapper.get('article').text()).not.toContain('$0.00')
    await wrapper.get('input[type="search"]').setValue('gpt-6-astra')
    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[1]!.text()).toContain('>272K')
    expect(rows[1]!.text()).toContain('$20.00')
    expect(rows[1]!.text()).toContain('$75.00')
    expect(rows[1]!.text()).toContain('$2.00')
    expect(wrapper.text()).toContain('整次请求')
    await wrapper.get('input[type="search"]').setValue('GLM-5.1')
    expect(wrapper.findAll('tbody tr')[0]!.text()).toContain('<32K')
    expect(wrapper.findAll('tbody tr')[1]!.text()).toContain('≥32K')
  })

  it('does not show reference data before the feature gate is confirmed, and preserves API errors', async () => {
    const wrapper = render('zh', false, true)
    expect(wrapper.findAll('article')).toHaveLength(0)
    expect(wrapper.find('[data-price-tab="reference"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('加载模型广场失败')
    await wrapper.setProps({ referenceEnabled: true })
    expect(wrapper.findAll('article')).toHaveLength(33)
    expect(wrapper.text()).not.toContain('加载模型广场失败')
    expect(wrapper.find('[data-price-tab="actual"]').exists()).toBe(false)
  })

  it('renders English labels and unavailable prices without raw translation keys', () => {
    const wrapper = render('en')
    expect(wrapper.text()).toContain('USD / million tokens')
    expect(wrapper.text()).toContain('Not provided')
    expect(wrapper.text()).not.toContain('modelPlaza.reference.')
  })

  it('copies the selected model ID and reports clipboard failure honestly', async () => {
    const clipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard')
    const secure = Object.getOwnPropertyDescriptor(window, 'isSecureContext')
    const execCommand = Object.getOwnPropertyDescriptor(document, 'execCommand')
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } })
    Object.defineProperty(window, 'isSecureContext', { configurable: true, value: true })
    Object.defineProperty(document, 'execCommand', { configurable: true, value: () => false })
    const wrapper = render()
    try {
      await wrapper.get('input[type="search"]').setValue('gpt-5.3-codex')
      await wrapper.get('article button').trigger('click')
      expect(writeText).toHaveBeenCalledWith('gpt-5.3-codex')
      expect(wrapper.get('[role="status"]').text()).toContain('已复制 gpt-5.3-codex')
      writeText.mockRejectedValueOnce(new Error('Clipboard denied'))
      await wrapper.get('article button').trigger('click')
      await vi.waitFor(() => expect(wrapper.get('[role="status"]').text()).toContain('复制失败'))
    } finally {
      wrapper.unmount()
      if (clipboard) Object.defineProperty(navigator, 'clipboard', clipboard)
      else Reflect.deleteProperty(navigator, 'clipboard')
      if (secure) Object.defineProperty(window, 'isSecureContext', secure)
      else Reflect.deleteProperty(window, 'isSecureContext')
      if (execCommand) Object.defineProperty(document, 'execCommand', execCommand)
      else Reflect.deleteProperty(document, 'execCommand')
    }
  })
})
