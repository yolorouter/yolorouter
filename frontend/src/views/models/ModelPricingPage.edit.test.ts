// @vitest-environment happy-dom
//
// The model pricing page's single-row edit: a row's edit entry opens the
// shared CandidateEditModal with THAT row's model id (the PATCH URL's owner)
// and the row itself as the edited candidate — the same component, the same
// API, the same PATCH write path the mapping tables use. A saved modal
// refetches the models list so the page re-renders from stored prices.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent } from 'vue'

import { listModels, type Model, type ModelCandidate } from '../../api/models'
import { listModelsForProvider, listProviders } from '../../api/providers'

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return { ...actual, listModels: vi.fn() }
})

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return { ...actual, listModelsForProvider: vi.fn(), listProviders: vi.fn() }
})

const listModelsMock = vi.mocked(listModels)
const listProvidersMock = vi.mocked(listProviders)
const catalogueMock = vi.mocked(listModelsForProvider)

// Must be imported after the vi.mock factories are registered.
import ModelPricingPage from './ModelPricingPage.vue'
import CandidateEditModal from '../../components/models/CandidateEditModal.vue'
import zhCN from '../../locales/zh-CN'

const Host = defineComponent({
  components: { ModelPricingPage },
  template: '<NConfigProvider><NMessageProvider><ModelPricingPage /></NMessageProvider></NConfigProvider>',
})

let wrapper: VueWrapper | null = null

function stubMatchMedia() {
  const mql = {
    matches: false,
    media: '(max-width: 768px)',
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => true,
  }
  vi.stubGlobal('matchMedia', vi.fn(() => mql))
}

function candidateFixture(overrides: Partial<ModelCandidate> = {}): ModelCandidate {
  return {
    id: 1,
    provider_id: 10,
    provider_name: 'alpha',
    provider_model_name: 'alpha-up',
    input_price: 1.5,
    output_price: 2.5,
    cache_write_price: null,
    cache_read_price: null,
    max_output: 4096,
    billing_mode: 'token',
    image_pricing_tiers: null,
    video_pricing_tiers: null,
    audio_unit_price: null,
    supports_streaming: null,
    supports_function_calling: null,
    management_status: 1,
    sort_order: 1,
    verification_status: 1,
    routable: true,
    blocked_by: '',
    last_test_result: 0,
    last_test_duration_ms: 120,
    last_tested_at: '2026-09-01T00:00:00Z',
    prices_declared: true,
    probe_owed: false,
    binding_count: 0,
    ...overrides,
  }
}

function modelFixture(id: number, name: string, candidates: ModelCandidate[]): Model {
  return {
    id,
    name,
    management_status: 1,
    running_status: 'available',
    scheduling_mode: 'failover',
    supports_image_input: null,
    output_modalities: ['text'],
    context_length: null,
    input_modalities: null,
    supports_tools: null,
    supports_reasoning: null,
    temperature: null,
    candidates,
    created_at: '2026-09-01T00:00:00Z',
  }
}

const MODELS = () => [
  modelFixture(7, 'edit-model', [
    candidateFixture({ id: 11, provider_name: 'alpha', provider_model_name: 'up-a' }),
    candidateFixture({ id: 12, provider_name: 'beta', provider_model_name: 'up-b', input_price: 2, output_price: 4 }),
  ]),
]

async function mountPage(models: Model[] = MODELS()) {
  listModelsMock.mockResolvedValue({ list: models })
  listProvidersMock.mockResolvedValue({ list: [] })
  // The shared edit modal pulls the edited provider's upstream catalogue when
  // it opens; nothing here needs its rows, only a settled answer.
  catalogueMock.mockResolvedValue({ models: [], outcome: null, detail: '' })
  const i18n = createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { 'zh-CN': zhCN } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/providers/:id', component: { template: '<div />' } },
      { path: '/models/:id', component: { template: '<div />' } },
    ],
  })
  await router.push('/')
  await router.isReady()
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  await vi.waitFor(() => expect(wrapper!.find('.pricing-group').exists()).toBe(true))
}

// The mapping row rendering the given upstream name.
function rowByUpstream(name: string) {
  const row = wrapper!.findAll('.pricing-row').find((r) => r.find('.pricing-row__upstream-name').text() === name)
  expect(row, `row for ${name} rendered`).toBeTruthy()
  return row!
}

beforeEach(() => {
  stubMatchMedia()
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('ModelPricingPage single-row edit', () => {
  it('a row edit opens the shared modal with that row model id and candidate', async () => {
    await mountPage()
    await wrapper!.findAll('button').find((b) => (b.text() ?? '').includes(zhCN.modelPricing.expandAll))!.trigger('click')
    await vi.waitFor(() => expect(wrapper!.findAll('.pricing-row').length).toBe(2))

    const edit = rowByUpstream('up-b').findAll('button').find((b) => (b.text() ?? '').includes(zhCN.modelPricing.editMapping))
    expect(edit, 'the row edit entry rendered').toBeTruthy()
    await edit!.trigger('click')

    const modal = wrapper!.findComponent(CandidateEditModal)
    expect(modal.exists(), 'the shared candidate edit modal opened').toBe(true)
    expect(modal.props('modelId')).toBe(7)
    expect(modal.props('modelName')).toBe('edit-model')
    const editing = modal.props('editingCandidate') as ModelCandidate | null
    expect(editing?.id).toBe(12)
    expect(editing?.provider_model_name).toBe('up-b')
  })

  it('a saved modal refetches the models list', async () => {
    await mountPage()
    expect(listModelsMock).toHaveBeenCalledTimes(1)
    await wrapper!.findAll('button').find((b) => (b.text() ?? '').includes(zhCN.modelPricing.expandAll))!.trigger('click')
    await vi.waitFor(() => expect(wrapper!.findAll('.pricing-row').length).toBe(2))

    const edit = rowByUpstream('up-a').findAll('button').find((b) => (b.text() ?? '').includes(zhCN.modelPricing.editMapping))
    await edit!.trigger('click')
    const modal = wrapper!.findComponent(CandidateEditModal)
    expect(modal.exists(), 'the shared candidate edit modal opened').toBe(true)

    modal.vm.$emit('saved')
    await vi.waitFor(() => expect(listModelsMock).toHaveBeenCalledTimes(2))
  })
})
