import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import RadarModelsView from '../RadarModelsView.vue'

const { radarStore, registerModel, untrackModel } = vi.hoisted(() => ({
  radarStore: {
    models: [],
    loadSection: vi.fn()
  },
  registerModel: vi.fn(),
  untrackModel: vi.fn()
}))

const labels: Record<string, string> = {
  'admin.radar.models.title': '跟踪模型',
  'admin.radar.models.description': '添加模型后会先显示证据不足，直到评测产生聚合结果。',
  'admin.radar.models.alias': '模型别名',
  'admin.radar.models.aliasPlaceholder': '例如 gpt-5.6-sol',
  'admin.radar.actions.addModel': '添加模型',
  'admin.radar.actions.addingModel': '添加中...',
  'admin.radar.actions.untrack': '解除跟踪',
  'admin.radar.actions.untrackHint': '解除跟踪，历史质量报告仍会保留',
  'admin.radar.actions.untrackConfirm': '解除跟踪该模型？历史质量报告会保留。',
  'admin.radar.messages.modelAliasRequired': '请输入模型别名',
  'admin.radar.messages.modelAdded': '模型已加入雷达跟踪',
  'admin.radar.messages.modelAddFailed': '添加模型失败',
  'admin.radar.messages.modelUntracked': '模型已解除跟踪',
  'admin.radar.messages.modelUntrackFailed': '解除模型跟踪失败'
}

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => labels[key] ?? key })
}))

vi.mock('@/stores/radar', () => ({
  useRadarStore: () => radarStore
}))

vi.mock('@/api/admin/radar', () => ({
  default: { registerModel, untrackModel }
}))

vi.mock('../components/RadarTable.vue', () => ({
  default: {
    props: ['rows'],
    template: '<div data-testid="radar-table"><div v-for="row in rows" :key="row.model_route" :data-model="row.model_route"><slot name="actions" :row="row" /></div>{{ rows.length }}</div>'
  }
}))

describe('RadarModelsView', () => {
  beforeEach(() => {
    radarStore.models = []
    radarStore.loadSection.mockReset()
    radarStore.loadSection.mockResolvedValue(undefined)
    registerModel.mockReset()
    registerModel.mockResolvedValue({ model_alias: 'gpt-5.6-sol' })
    untrackModel.mockReset()
    untrackModel.mockResolvedValue({ model_alias: 'gpt-5.6-sol', status: 'untracked' })
  })

  it('registers a model alias and refreshes the visible projection', async () => {
    const wrapper = mount(RadarModelsView)
    await flushPromises()

    await wrapper.get('input[name="model-alias"]').setValue('gpt-5.6-sol')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(registerModel).toHaveBeenCalledWith({ model_alias: 'gpt-5.6-sol' })
    expect(radarStore.loadSection).toHaveBeenLastCalledWith('models')
    expect(wrapper.text()).toContain('模型已加入雷达跟踪')
  })

  it('untracks a model after confirmation and refreshes the list', async () => {
    radarStore.models = [{ model_alias: 'gpt-5.6-sol', model_route: 'gpt-5.6-sol' }]
    vi.stubGlobal('confirm', vi.fn(() => true))
    const wrapper = mount(RadarModelsView)
    await flushPromises()

    const action = wrapper.get('[data-test="untrack-model-gpt-5.6-sol"]')
    expect(action.attributes('aria-label')).toBe('解除跟踪')
    expect(action.attributes('title')).toBe('解除跟踪，历史质量报告仍会保留')

    await action.trigger('click')
    await flushPromises()

    expect(window.confirm).toHaveBeenCalledWith('解除跟踪该模型？历史质量报告会保留。')
    expect(untrackModel).toHaveBeenCalledWith('gpt-5.6-sol')
    expect(radarStore.loadSection).toHaveBeenLastCalledWith('models')
    expect(wrapper.text()).toContain('模型已解除跟踪')
  })

  it('shows localized failure feedback without reloading after untracking is rejected', async () => {
    radarStore.models = [{ model_alias: 'gpt-5.6-sol', model_route: 'gpt-5.6-sol' }]
    radarStore.loadSection.mockClear()
    untrackModel.mockRejectedValueOnce(new Error('delete failed'))
    vi.stubGlobal('confirm', vi.fn(() => true))
    const wrapper = mount(RadarModelsView)
    await flushPromises()
    radarStore.loadSection.mockClear()

    await wrapper.get('[data-test="untrack-model-gpt-5.6-sol"]').trigger('click')
    await flushPromises()

    expect(window.confirm).toHaveBeenCalledWith('解除跟踪该模型？历史质量报告会保留。')
    expect(untrackModel).toHaveBeenCalledWith('gpt-5.6-sol')
    expect(radarStore.loadSection).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('解除模型跟踪失败')
  })

  it('uses the tracked alias instead of the visible route when untracking', async () => {
    radarStore.models = [{ model_alias: 'gpt-5.6-sol', model_route: 'global' }]
    vi.stubGlobal('confirm', vi.fn(() => true))
    const wrapper = mount(RadarModelsView)
    await flushPromises()

    await wrapper.get('[data-test="untrack-model-global"]').trigger('click')
    await flushPromises()

    expect(untrackModel).toHaveBeenCalledWith('gpt-5.6-sol')
  })

  it('does not offer untracking for a visible route without a tracked alias', async () => {
    radarStore.models = [{ model_route: 'global' }]
    const wrapper = mount(RadarModelsView)
    await flushPromises()

    expect(wrapper.find('[data-test="untrack-model-global"]').exists()).toBe(false)
  })

  it('hides aggregate global rows from the tracked-model management list', async () => {
    radarStore.models = [
      { model_route: 'global' },
      { model_alias: 'gpt-5.6-sol', model_route: 'gpt-5.6-sol' }
    ]
    const wrapper = mount(RadarModelsView)
    await flushPromises()

    expect(wrapper.find('[data-model="global"]').exists()).toBe(false)
    expect(wrapper.find('[data-model="gpt-5.6-sol"]').exists()).toBe(true)
  })
})
