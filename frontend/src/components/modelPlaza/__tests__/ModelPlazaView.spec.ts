import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ModelPlazaView from '@/views/ModelPlazaView.vue'

const state = vi.hoisted(() => {
  vi.stubGlobal('__INTLIFY_JIT_COMPILATION__', true)
  return {
    settings: { enabled: true, requireAuth: false, loaded: true, backendMode: false },
    auth: { isAuthenticated: false, isAdmin: false }
  }
})
vi.mock('vue-router', () => ({ useRoute: () => ({ query: {} }) }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<div><slot /></div>' } }))
vi.mock('../PlazaNavBar.vue', () => ({ default: { template: '<nav />' } }))
vi.mock('@/api/modelPlaza', () => ({ getModelPlaza: async () => ({ description: '', groups: [] }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => state.auth }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({
  publicSettingsLoaded: state.settings.loaded,
  cachedPublicSettings: { model_plaza_enabled: state.settings.enabled, model_plaza_require_auth: state.settings.requireAuth },
  backendModeEnabled: state.settings.backendMode,
  fetchPublicSettings: async () => undefined
}) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

describe('model plaza reference feature gates', () => {
  it.each([
    { loaded: false, enabled: true, requireAuth: false, backendMode: false, user: 'anonymous', visible: false },
    { loaded: true, enabled: false, requireAuth: false, backendMode: false, user: 'admin', visible: false },
    { loaded: true, enabled: true, requireAuth: true, backendMode: false, user: 'anonymous', visible: false },
    { loaded: true, enabled: true, requireAuth: false, backendMode: true, user: 'anonymous', visible: false },
    { loaded: true, enabled: true, requireAuth: false, backendMode: true, user: 'user', visible: false },
    { loaded: true, enabled: true, requireAuth: false, backendMode: true, user: 'admin', visible: true },
    { loaded: true, enabled: true, requireAuth: true, backendMode: false, user: 'user', visible: true },
    { loaded: true, enabled: true, requireAuth: false, backendMode: false, user: 'anonymous', visible: true }
  ])('respects settings and access for $user: $visible ($loaded/$enabled/$requireAuth/$backendMode)', async (scenario) => {
    Object.assign(state.settings, scenario)
    Object.assign(state.auth, { isAuthenticated: scenario.user !== 'anonymous', isAdmin: scenario.user === 'admin' })
    const wrapper = mount(ModelPlazaView)
    await flushPromises()
    expect(wrapper.find('article').exists()).toBe(scenario.visible)
    wrapper.unmount()
  })
})
