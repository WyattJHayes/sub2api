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

describe('public model plaza prices', () => {
  it('shows only the public price table without exposing group pricing or calculation controls', async () => {
    const wrapper = render([group()])
    await wrapper.get('input[type="search"]').setValue('claude-fable-5-1')
    const card = wrapper.get('article')
    expect(card.text()).toContain('$10.00')
    expect(card.text()).toContain('$50.00')
    expect(card.text()).toContain('$0.25')
    expect(card.findAll('table')).toHaveLength(1)
    expect(wrapper.find('[data-group-rates]').exists()).toBe(false)
    expect(wrapper.find('[data-group-filter]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Claude Max')
    expect(wrapper.find('[data-group-quote]').exists()).toBe(false)
    expect(wrapper.find('select[data-effort]').exists()).toBe(false)
    expect(wrapper.find('[data-time-period]').exists()).toBe(false)
    expect(wrapper.find('[data-peak-period]').exists()).toBe(false)
    expect(card.text()).not.toContain('$14.40')
    expect(card.text()).not.toContain('$43.20')
    wrapper.unmount()
  })

  it.each([0.5, 0])('keeps the public price independent of the personal multiplier %s', async (userRate) => {
    const wrapper = render([group(1.2, userRate)])
    await wrapper.get('input[type="search"]').setValue('claude-fable-5-1')
    expect(wrapper.get('article').text()).toContain('$10.00')
    expect(wrapper.find('[data-group-rates]').exists()).toBe(false)
    expect(wrapper.get('article').text()).not.toContain('$6.00')
    wrapper.unmount()
  })

  it('shows one public model price when groups advertise the same model on different platforms', async () => {
    const mixed = group()
    mixed.platform = 'composite'
    mixed.name = '混合分组'
    mixed.models.push({ ...mixed.models[0]!, platform: 'openai', group_pricing_override: {
      ...mixed.models[0]!.group_pricing_override!, input_price: 18e-6, output_price: 90e-6,
      reasoning_effort_multipliers: { max: 4 }
    } })
    const wrapper = render([mixed])
    await wrapper.get('input[type="search"]').setValue('claude-fable-5-1')
    expect(wrapper.findAll('article')).toHaveLength(1)
    expect(wrapper.get('article').text()).toContain('$10.00')
    expect(wrapper.get('article').text()).toContain('$50.00')
    expect(wrapper.text()).not.toContain('混合分组')
    expect(wrapper.text()).not.toContain('$21.60')
    expect(wrapper.text()).not.toContain('$86.40')
    wrapper.unmount()
  })

  it('keeps a single public tariff regardless of group, subscription and time pricing settings', async () => {
    const plain = group(0.4)
    plain.id = 7
    plain.name = '普通分组'
    plain.subscription_type = 'subscription'
    plain.peak_rate_enabled = true
    plain.peak_rate_multiplier = 2
    plain.models[0]!.time_pricing = { timezone: 'Asia/Shanghai', weekdays_only: true, periods: [{ start_time: '18:00', end_time: '22:00', multiplier: 0.5 }] }
    const wrapper = render([group(), plain])
    await wrapper.get('input[type="search"]').setValue('claude-fable-5-1')
    expect(wrapper.findAll('article')).toHaveLength(1)
    expect(wrapper.get('article').findAll('table')).toHaveLength(1)
    expect(wrapper.find('[data-group-filter]').exists()).toBe(false)
    expect(wrapper.find('[data-group-rates]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('普通分组')
    expect(wrapper.get('article').text()).toContain('$10.00')
    expect(wrapper.get('article').text()).not.toContain('$4.00')
    wrapper.unmount()
  })
})
