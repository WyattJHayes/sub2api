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
  it.each([
    { name: 'gpt-5.2-2025-12-11', platform: 'openai', rows: [['全部', '$1.75', '$14.00', '$0.175', '未提供']] },
    { name: 'gpt-5.2-chat-latest', platform: 'openai', rows: [['全部', '$1.75', '$14.00', '$0.175', '未提供']] },
    { name: 'gpt-5.2-pro', platform: 'openai', rows: [['全部', '$21.00', '$168.00', '未提供', '未提供']] },
    { name: 'gpt-5.2-pro-2025-12-11', platform: 'openai', rows: [['全部', '$21.00', '$168.00', '未提供', '未提供']] },
    { name: 'gpt-5.4-2026-03-05', platform: 'openai', rows: [
      ['≤272K', '$2.50', '$15.00', '$0.25', '未提供'], ['>272K', '$5.00', '$22.50', '$0.50', '未提供']
    ] },
    { name: 'claude-sonnet-5-5', platform: 'anthropic', rows: [['全部', '$2.00', '$10.00', '$0.10', '$2.50', '$4.00']] },
    { name: 'claude-sonnet-5', platform: 'anthropic', rows: [['全部', '$2.00', '$10.00', '$0.20', '$2.50', '$4.00']] },
    { name: 'claude-sonnet-4-5', platform: 'anthropic', rows: [['全部', '$3.00', '$15.00', '$0.30', '$3.75', '$6.00']] },
    { name: 'claude-opus-4-5', platform: 'anthropic', rows: [['全部', '$5.00', '$25.00', '$0.50', '$6.25', '$10.00']] },
    { name: 'claude-opus-4-6', platform: 'anthropic', rows: [['全部', '$5.00', '$25.00', '$0.50', '$6.25', '$10.00']] },
    { name: 'claude-opus-4-7', platform: 'anthropic', rows: [['全部', '$5.00', '$25.00', '$0.50', '$6.25', '$10.00']] },
    { name: 'claude-haiku-5-5', platform: 'anthropic', rows: [
      ['≤100K', '$0.10', '$0.50', '$0.01', '$0.125', '$0.20'],
      ['>100K', '$0.50', '$2.50', '$0.05', '$0.625', '$1.00']
    ] },
    { name: 'glm-5.3', platform: 'openai', rows: [['全部', '$1.40', '$4.40', '$0.26', '未提供']] },
    { name: 'glm-5.3-flash', platform: 'openai', rows: [['全部', '$0.15', '$0.50', '$0.03', '未提供']] },
    { name: 'grok-4.5', platform: 'grok', rows: [
      ['<200K', '$2.00', '$6.00', '$0.30', '未提供'], ['≥200K', '$4.00', '$12.00', '$0.60', '未提供']
    ] },
    { name: 'grok-4.6', platform: 'grok', rows: [
      ['<200K', '$2.00', '$6.00', '$0.50', '未提供'], ['≥200K', '$4.00', '$12.00', '$1.00', '未提供']
    ] },
    { name: 'grok-4.7', platform: 'grok', rows: [
      ['<200K', '$2.00', '$6.00', '$0.50', '未提供'], ['≥200K', '$4.00', '$12.00', '$1.00', '未提供']
    ] }
  ])('renders verified public prices for $name without using group or billing fallback prices', async ({ name, platform, rows }) => {
    const visible = group(0.08, 0.01)
    visible.models[0] = {
      ...visible.models[0]!, name, platform,
      official_pricing: { input_price: 999e-6, output_price: 999e-6, cache_read_price: 999e-6, cache_write_price: 999e-6 }
    }
    const wrapper = render([visible])
    await wrapper.get('input[type="search"]').setValue(name)
    const cards = wrapper.findAll('article').filter(card => card.get('h3').text().toLowerCase() === name)
    expect(cards).toHaveLength(1)
    const card = cards[0]!
    expect(card.findAll('table')).toHaveLength(1)
    expect(card.findAll('tbody tr').map(row => [row.get('th').text(), ...row.findAll('td').map(cell => cell.text())])).toEqual(rows)
    expect(card.text()).not.toContain('待核实')
    expect(card.text()).not.toContain('$999.00')
    expect(wrapper.find('[data-group-quote]').exists()).toBe(false)
    wrapper.unmount()
  })

  it.each(['zh', 'en'])('explains Haiku and Grok prompt-length tiers without OpenAI-specific thresholds in %s', async (locale) => {
    const visible = group()
    visible.models[0] = { ...visible.models[0]!, name: 'claude-haiku-5-5' }
    const wrapper = render([visible], locale)
    await wrapper.get('input[type="search"]').setValue('claude-haiku-5-5')
    const card = wrapper.get('article')
    expect(card.text()).toContain(locale === 'zh' ? '整次请求' : 'entire request')
    expect(card.text()).not.toContain('272K')
    expect(card.text()).not.toContain('modelPlaza.reference.')
    wrapper.unmount()
  })

  it.each([['anthropic', 'openai'], ['openai', 'anthropic']])('keeps every provider filter for an unknown model advertised by %s and %s', async (first, second) => {
    const mixed = group()
    mixed.models = [first, second].map(platform => ({
      ...mixed.models[0]!, name: 'shared-unverified-model', platform
    }))
    const wrapper = render([mixed])
    await wrapper.get('input[type="search"]').setValue('shared-unverified-model')
    expect(wrapper.findAll('article')).toHaveLength(1)
    expect(wrapper.get('article').text()).toContain('Anthropic')
    expect(wrapper.get('article').text()).toContain('OpenAI')
    for (const platform of [first, second]) {
      await wrapper.get('select').setValue(platform)
      expect(wrapper.findAll('article')).toHaveLength(1)
      expect(wrapper.get('article').findAll('table')).toHaveLength(1)
      expect(wrapper.get('article').text()).toContain('待核实')
    }
    wrapper.unmount()
  })

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
