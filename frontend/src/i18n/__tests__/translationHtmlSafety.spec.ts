import { describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import en from '../locales/en'
import zh from '../locales/zh'

vi.hoisted(() => { vi.stubGlobal('__INTLIFY_JIT_COMPILATION__', true) })

describe('translation HTML safety', () => {
  it('renders Chinese model states and interpolation without dynamic code generation', () => {
    const dynamicCode = vi.spyOn(globalThis, 'Function').mockImplementation(() => {
      throw new Error('CSP forbids dynamic code generation')
    })
    try {
      const i18n = createI18n({
        legacy: false,
        locale: 'zh',
        messages: { zh: { ...zh, cspExample: '模型：{model}' }, en }
      })
      expect(i18n.global.t('usage.modelConsistent')).toBe('模型一致')
      expect(i18n.global.t('usage.modelUnknown')).toBe('模型未知')
      expect(i18n.global.t('cspExample', { model: 'gpt-6.1-terra' })).toBe('模型：gpt-6.1-terra')
      i18n.global.locale.value = 'en'
      expect(i18n.global.t('usage.modelConsistent')).toBe('Model consistent')
      expect(dynamicCode).not.toHaveBeenCalled()
    } finally {
      dynamicCode.mockRestore()
    }
  })

  it('neutralizes iframe srcdoc instead of preserving an active nested HTML document', () => {
    const i18n = createI18n({
      legacy: false,
      locale: 'en',
      escapeParameter: true,
      warnHtmlMessage: false,
      messages: { en: { example: '<iframe srcdoc="<b>untrusted</b>"></iframe>' } }
    })
    const translated = i18n.global.t('example')
    const container = document.createElement('div')
    container.innerHTML = translated
    expect(container.querySelector('iframe')?.hasAttribute('srcdoc')).toBe(false)
  })
})
