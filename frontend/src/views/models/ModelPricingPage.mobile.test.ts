// @vitest-environment happy-dom
//
// The model pricing page's two viewport states (the shared 768px breakpoint,
// inclusive): desktop renders the labeled grid rows, mobile swaps them for
// stacked cards whose fields carry their own labels — browse, filter,
// expand, and single-row edit all stay reachable. The bulk selection layer
// is desktop-only: mobile must not show a checkbox or the bulk toolbar.
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

// A controllable MediaQueryList: the initial `matches` decides which layout
// the page renders, and change events flip it live.
function stubMatchMedia(matches: boolean) {
  const listeners = new Set<(e: { matches: boolean }) => void>()
  const mql = {
    matches,
    media: '(max-width: 768px)',
    onchange: null,
    addEventListener: (_type: string, cb: (e: { matches: boolean }) => void) => listeners.add(cb),
    removeEventListener: (_type: string, cb: (e: { matches: boolean }) => void) => listeners.delete(cb),
    addListener: (cb: (e: { matches: boolean }) => void) => listeners.add(cb),
    removeListener: (cb: (e: { matches: boolean }) => void) => listeners.delete(cb),
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
  modelFixture(7, 'mobile-model', [
    candidateFixture({ id: 11, provider_name: 'alpha', provider_model_name: 'up-a' }),
    candidateFixture({ id: 12, provider_name: 'beta', provider_model_name: 'up-b', input_price: 2, output_price: 4 }),
  ]),
]

async function mountPage(matches: boolean, models: Model[] = MODELS()) {
  stubMatchMedia(matches)
  listModelsMock.mockResolvedValue({ list: models })
  listProvidersMock.mockResolvedValue({ list: [] })
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

async function expandAll() {
  const button = wrapper!.findAll('button').find((b) => (b.text() ?? '').includes(zhCN.modelPricing.expandAll))
  expect(button, 'the expand-all control rendered').toBeTruthy()
  await button!.trigger('click')
  await vi.waitFor(() => expect(wrapper!.findAll('.pricing-row').length).toBe(2))
}

beforeEach(() => {
  listProvidersMock.mockResolvedValue({ list: [] })
  // The shared edit modal pulls the edited provider's upstream catalogue when
  // it opens; the mobile suite only needs a settled answer.
  catalogueMock.mockResolvedValue({ models: [], outcome: null, detail: '' })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('ModelPricingPage viewports', () => {
  it('desktop renders the labeled grid and no per-field labels', async () => {
    await mountPage(false)
    await expandAll()
    expect(wrapper!.find('.pricing-row-labels').exists(), 'the desktop column labels row rendered').toBe(true)
    expect(wrapper!.findAll('.pricing-row__field-label').length, 'no per-field labels on desktop').toBe(0)
  })

  it('mobile renders stacked labeled cards, still expandable and editable, with no selection UI', async () => {
    await mountPage(true)
    // Collapsed by default on mobile too; the header expands it.
    expect(wrapper!.findAll('.pricing-row').length).toBe(0)
    await expandAll()

    expect(wrapper!.find('.pricing-row-labels').exists(), 'no desktop labels row on mobile').toBe(false)
    const fieldLabels = wrapper!.findAll('.pricing-row__field-label')
    expect(fieldLabels.length, 'mobile fields carry their own labels').toBeGreaterThan(0)
    expect(fieldLabels.map((l) => l.text())).toContain(zhCN.modelPricing.colPrice)

    // Filtering stays available on mobile.
    expect(wrapper!.find('.filter-panel').exists(), 'the filter panel rendered').toBe(true)

    // Single-row edit stays available on mobile.
    const edit = wrapper!
      .findAll('.pricing-row')
      .find((r) => r.find('.pricing-row__upstream-name').text() === 'up-a')!
      .findAll('button')
      .find((b) => (b.text() ?? '').includes(zhCN.modelPricing.editMapping))
    expect(edit, 'the row edit entry rendered').toBeTruthy()
    await edit!.trigger('click')
    await vi.waitFor(() => expect(wrapper!.findComponent(CandidateEditModal).exists()).toBe(true))

    // No selection UI on mobile: naive-ui's checkbox renders as a
    // div.n-checkbox (role=checkbox), no native input inside.
    expect(document.body.querySelectorAll('.n-checkbox').length, 'no checkboxes on mobile').toBe(0)
  })

  it('desktop carries the bulk selection layer: checkboxes render there', async () => {
    await mountPage(false)
    await expandAll()
    expect(document.body.querySelectorAll('.n-checkbox').length, 'desktop renders the selection checkboxes').toBeGreaterThan(0)
  })
})
