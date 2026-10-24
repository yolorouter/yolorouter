// @vitest-environment happy-dom
//
// The model pricing page's three-part client-side filter: keyword over the
// model name / provider name / upstream name, an unpriced-only switch, and a
// provider dropdown whose options are the distinct providers in the fetched
// data. The keyword follows the draft/applied pair (text edits nothing until
// Search); the switch and the dropdown apply immediately, mirroring the
// models list page. A model without mappings never appears; a filter
// everything fails renders the page's own filter empty state.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive, { NSwitch } from 'naive-ui'
import { defineComponent, type ComponentPublicInstance } from 'vue'

import { listModels, type Model, type ModelCandidate } from '../../api/models'

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return { ...actual, listModels: vi.fn() }
})

const listModelsMock = vi.mocked(listModels)

// Must be imported after the vi.mock factories are registered.
import ModelPricingPage from './ModelPricingPage.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import zhCN from '../../locales/zh-CN'

const Host = defineComponent({
  components: { ModelPricingPage },
  template: '<NConfigProvider><NMessageProvider><ModelPricingPage /></NMessageProvider></NConfigProvider>',
})

// The generic FilterSelectField's findComponent handle: a VueWrapper over the
// props this suite reads (same approach as the sibling page tests).
type FilterSelectWrapper = VueWrapper<
  unknown,
  ComponentPublicInstance<{
    label?: string
    placeholder?: string
    options?: Array<{ label: string; value: number }>
  }>
>

// The page's single provider filter select.
function providerSelect(): FilterSelectWrapper {
  const selects = wrapper!.findAllComponents(FilterSelectField) as unknown as FilterSelectWrapper[]
  expect(selects.length, 'the provider filter select rendered').toBe(1)
  return selects[0]
}

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
    provider_id: 1,
    provider_name: 'Provider One',
    provider_model_name: 'up-a',
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

const FIXTURE_MODELS = () => [
  modelFixture(1, 'alpha-model', [
    candidateFixture({ id: 1, provider_id: 1, provider_name: 'Provider One', provider_model_name: 'up-a' }),
    candidateFixture({ id: 2, provider_id: 2, provider_name: 'Provider Two', provider_model_name: 'up-b', prices_declared: false, input_price: 0, output_price: 0 }),
  ]),
  modelFixture(2, 'beta-model', [
    candidateFixture({ id: 3, provider_id: 1, provider_name: 'Provider One', provider_model_name: 'up-c' }),
  ]),
  modelFixture(3, 'gamma-model', []),
]

async function mountPage(models: Model[] = FIXTURE_MODELS()) {
  listModelsMock.mockResolvedValue({ list: models })
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
  await vi.waitFor(() => expect(wrapper!.find('.pricing-group').exists() || wrapper!.find('.empty-state').exists()).toBe(true))
}

function groupNames(): string[] {
  return wrapper!.findAll('.pricing-group__name').map((n) => n.text())
}

function upstreamNames(): string[] {
  return wrapper!.findAll('.pricing-row__upstream-name').map((n) => n.text())
}

function buttonByLabel(label: string) {
  const button = wrapper!.findAll('button').find((b) => (b.text() ?? '').trim() === label)
  expect(button, `button "${label}" rendered`).toBeTruthy()
  return button!
}

// Types into the keyword input and applies via the Search button.
async function search(keyword: string) {
  await wrapper!.find('.filter-item--search input').setValue(keyword)
  await buttonByLabel(zhCN.modelPricing.applyFilter).trigger('click')
  await vi.waitFor(() => expect(wrapper!.findAll('.pricing-group').length).toBeLessThanOrEqual(2))
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

describe('ModelPricingPage filters', () => {
  it('offers the distinct providers of the fetched data as dropdown options and never renders mapping-less models', async () => {
    await mountPage()
    const options = providerSelect().props('options') ?? []
    expect(options.map((o) => o.value)).toEqual([1, 2])
    expect(options.map((o) => o.label)).toEqual(['Provider One', 'Provider Two'])
    expect(wrapper!.text()).not.toContain('gamma-model')
  })

  it('keyword edits stay a draft until Search applies them', async () => {
    await mountPage()
    await wrapper!.find('.filter-item--search input').setValue('beta')
    expect(groupNames(), 'typing alone must not filter').toEqual(['alpha-model', 'beta-model'])
    await buttonByLabel(zhCN.modelPricing.applyFilter).trigger('click')
    await vi.waitFor(() => expect(groupNames()).toEqual(['beta-model']))
  })

  it('the keyword matches the provider name and keeps only that provider rows', async () => {
    await mountPage()
    const expandAll = buttonByLabel(zhCN.modelPricing.expandAll)
    await expandAll.trigger('click')
    await search('Provider Two')
    expect(groupNames()).toEqual(['alpha-model'])
    expect(upstreamNames()).toEqual(['up-b'])
  })

  it('the keyword matches the upstream model name', async () => {
    await mountPage()
    await search('up-c')
    expect(groupNames()).toEqual(['beta-model'])
  })

  it('the unpriced-only switch applies immediately and keeps unpriced rows only', async () => {
    await mountPage()
    await buttonByLabel(zhCN.modelPricing.expandAll).trigger('click')
    const switches = wrapper!.findAllComponents(NSwitch)
    expect(switches.length, 'the unpriced-only switch rendered').toBeGreaterThan(0)
    await switches[0]!.trigger('click')
    await vi.waitFor(() => expect(groupNames()).toEqual(['alpha-model']))
    expect(upstreamNames()).toEqual(['up-b'])
  })

  it('the provider dropdown filters groups down to the chosen provider rows', async () => {
    await mountPage()
    await buttonByLabel(zhCN.modelPricing.expandAll).trigger('click')
    ;(providerSelect() as unknown as VueWrapper).vm.$emit('update:value', 2)
    await vi.waitFor(() => expect(groupNames()).toEqual(['alpha-model']))
    expect(upstreamNames()).toEqual(['up-b'])
  })

  it('a filter nothing matches renders the filter empty state', async () => {
    await mountPage()
    await search('no-such-name')
    expect(groupNames()).toEqual([])
    const empty = wrapper!.find('.empty-state')
    expect(empty.exists(), 'the filter empty state rendered').toBe(true)
    expect(empty.text()).toContain(zhCN.modelPricing.filterEmptyTitle)
  })

  it('reset restores the unfiltered list', async () => {
    await mountPage()
    await search('up-c')
    expect(groupNames()).toEqual(['beta-model'])
    await buttonByLabel(zhCN.modelPricing.resetFilter).trigger('click')
    await vi.waitFor(() => expect(groupNames()).toEqual(['alpha-model', 'beta-model']))
  })
})
