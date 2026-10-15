import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { createPinia } from 'pinia'
import zh from '@/i18n/locales/zh'
import en from '@/i18n/locales/en'
import ModelPlazaPanels from '../ModelPlazaPanels.vue'
import type { ModelPlazaGroup } from '@/api/modelPlaza'

vi.hoisted(() => { vi.stubGlobal('__INTLIFY_JIT_COMPILATION__', true) })

function group(rate = 1.2, userRate?: number): ModelPlazaGroup {
  return {
    id: 4, name: 'Claude Max', description: '', platform: 'anthropic',
    subscription_type: 'standard', rate_multiplier: rate, user_rate_multiplier: userRate,
    peak_rate_enabled: false, peak_start: '', peak_end: '', peak_rate_multiplier: 1,
    is_exclusive: false, image_rate_independent: false, image_rate_multiplier: 1,
    video_rate_independent: false, video_rate_multiplier: 1, long_context_pricing_enabled: true,
    models: [{
      name: 'claude-fable-5-1', platform: 'anthropic', pricing: null, official_pricing: null,
      group_pricing_override: {
        billing_mode: 'token', input_price: 12e-6, output_price: 60e-6,
        cache_read_price: 0.3e-6, cache_write_price: 15e-6, cache_write_1h_price: 24e-6,
        intervals: [], reasoning_effort_multipliers: { max: 3 },
        image_input_price: null, image_output_price: null, per_request_price: null
      }
    }]
  }
}

function render(groups: ModelPlazaGroup[], locale = 'zh') {
  return mount(ModelPlazaPanels, {
    props: { response: { description: '', groups }, loading: false, error: false, referenceEnabled: true },
    global: { plugins: [createPinia(), createI18n({ legacy: false, locale, messages: { zh, en } })] }
  })
}

describe('official prices with configured group adjustments', () => {
  it('recalculates a selected custom period and subscription peak rate, including their overlap', async () => {
    const timed = group()
    timed.subscription_type = 'subscription'
    timed.peak_rate_enabled = true
    timed.peak_start = '18:00'
    timed.peak_end = '22:00'
    timed.peak_rate_multiplier = 2
    timed.models[0]!.time_pricing = {
      timezone: 'Asia/Shanghai', weekdays_only: true,
      periods: [{ start_time: '20:00', end_time: '24:00', multiplier: 0.5 }]
    }
    const wrapper = render([timed])
    await wrapper.get('input[type="search"]').setValue('claude-fable-5-1')
    const quote = wrapper.get('[data-group-quote="4"]')
    expect(quote.text()).not.toContain('显式缓存读取')
    expect(quote.text()).toContain('$14.40')
    expect(quote.text()).toContain('Asia/Shanghai')
    expect(quote.text()).toContain('周一至周五')
    await quote.get('select[data-time-period]').setValue('0')
    expect(quote.text()).toContain('$7.20')
    await quote.get('select[data-peak-period]').setValue('peak')
    expect(quote.text()).toContain('$14.40')
    await wrapper.get('select[data-effort]').setValue('max')
    expect(quote.text()).toContain('$43.20')
    wrapper.unmount()
  })

  it('ignores subscription peak settings for standard groups', async () => {
    const standard = group()
    standard.peak_rate_enabled = true
    standard.peak_rate_multiplier = 10
    const wrapper = render([standard])
    await wrapper.get('input[type="search"]').setValue('claude-fable-5-1')
    expect(wrapper.find('select[data-peak-period]').exists()).toBe(false)
    expect(wrapper.get('[data-group-quote="4"]').text()).toContain('$14.40')
    wrapper.unmount()
  })

  it('keeps Fable premium once, then applies the group rate and selected effort to every price field', async () => {
    const wrapper = render([group()])
    await wrapper.get('input[type="search"]').setValue('claude-fable-5-1')
    const quote = wrapper.get('[data-group-quote="4"]')
    expect(quote.text()).toContain('$14.40')
    expect(quote.text()).toContain('$72.00')
    expect(quote.text()).toContain('$0.36')
    expect(quote.text()).toContain('$18.00')
    expect(quote.text()).toContain('$28.80')
    await wrapper.get('select[data-effort]').setValue('max')
    expect(quote.text()).toContain('$43.20')
    expect(quote.text()).toContain('$216.00')
    expect(quote.text()).toContain('$1.08')
    expect(quote.text()).toContain('$54.00')
    expect(quote.text()).toContain('$86.40')
    wrapper.unmount()
  })

  it.each([{ rate: 0.5, input: '$6.00' }, { rate: 0, input: '$0.00' }])('uses a personal rate as replacement, including zero: $rate', async ({ rate, input }) => {
    const wrapper = render([group(1.2, rate)])
    await wrapper.get('input[type="search"]').setValue('claude-fable-5-1')
    expect(wrapper.get('[data-group-quote="4"]').text()).toContain(input)
    wrapper.unmount()
  })

  it('separately calculates the same model in different groups without inventing a premium on other groups', async () => {
    const plain = group(0.4)
    plain.id = 7
    plain.name = '普通分组'
    delete plain.models[0]!.group_pricing_override
    const wrapper = render([group(), plain])
    await wrapper.get('input[type="search"]').setValue('claude-fable-5-1')
    expect(wrapper.get('[data-group-quote="7"]').text()).toContain('$4.00')
    expect(wrapper.get('[data-group-quote="4"]').text()).toContain('$14.40')
    await wrapper.get('select[data-group-filter]').setValue('7')
    expect(wrapper.find('[data-group-quote="4"]').exists()).toBe(false)
    expect(wrapper.find('[data-group-quote="7"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('does not present stale quotes after the group request fails', async () => {
    const wrapper = render([group()])
    await wrapper.setProps({ error: true })
    expect(wrapper.find('[data-group-quote]').exists()).toBe(false)
    expect(wrapper.get('[role="alert"]').text()).toContain('加载模型广场失败')
    wrapper.unmount()
  })

  it('prices newer discovered models from their exact official tariff, without fuzzy preview matching', async () => {
    const active = group(0.4)
    active.models = [
      { name: 'gpt-6.1-sol', platform: 'openai', pricing: null, official_pricing: null },
      { name: 'gpt-6.1-sol-preview', platform: 'openai', pricing: null, official_pricing: null }
    ]
    const wrapper = render([active])
    await wrapper.get('input[type="search"]').setValue('gpt-6.1-sol')
    const quotes = wrapper.findAll('[data-group-quote="4"]')
    expect(quotes[0]!.text()).toContain('$0.80')
    expect(quotes[0]!.text()).toContain('$0.04')
    expect(quotes[0]!.text()).toContain('$1.60')
    expect(quotes[1]!.text()).toContain('待核实')
    expect(quotes[1]!.text()).not.toContain('$0.80')
    wrapper.unmount()
  })
})
