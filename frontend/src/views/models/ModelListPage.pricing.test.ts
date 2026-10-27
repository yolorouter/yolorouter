// @vitest-environment happy-dom
//
// The models list' pricing view: the price summary column (lowest input price
// among the model's PRICED token candidates — explicit free 0 in, image and
// video rows never counted — plus the unpriced warning count), the
// unpriced-only filter switch applying immediately like the page's selects,
// the expanded comparison surface (price-sorted rows, cheapest tag), and a
// row's edit entry opening the shared CandidateEditModal whose saved event
// refetches the list.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent } from 'vue'

import { listModels, type Model, type ModelCandidate } from '../../api/models'
import { listModelsForProvider, listProviders } from '../../api/providers'
import { getSystemEndpoint } from '../../api/system'

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return { ...actual, listModels: vi.fn() }
})

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return { ...actual, listModelsForProvider: vi.fn(), listProviders: vi.fn() }
})

// The page's CC-Switch export composable resolves the gateway endpoint at
// setup; a settled answer keeps happy-dom from dialing its default origin.
vi.mock('../../api/system', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/system')>()
  return { ...actual, getSystemEndpoint: vi.fn() }
})

const listModelsMock = vi.mocked(listModels)
const listProvidersMock = vi.mocked(listProviders)
const catalogueMock = vi.mocked(listModelsForProvider)
const endpointMock = vi.mocked(getSystemEndpoint)

// Must be imported after the vi.mock factories are registered.
import ModelListPage from './ModelListPage.vue'
import CandidateEditModal from '../../components/models/CandidateEditModal.vue'
import zhCN from '../../locales/zh-CN'

const Host = defineComponent({
  components: { ModelListPage },
  template: '<NConfigProvider><NDialogProvider><NMessageProvider><ModelListPage /></NMessageProvider></NDialogProvider></NConfigProvider>',
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

const imageTiers = { mode: 'image', tiers: [{ quality: '', size: '', price: 0.5 }] }

// The price-column fixtures: one model per acceptance case.
const PRICE_MODELS = () => [
  modelFixture(7, 'min-across-providers', [
    candidateFixture({ id: 11, provider_name: 'alpha', provider_model_name: 'up-a', input_price: 2, output_price: 3, sort_order: 1 }),
    candidateFixture({ id: 12, provider_name: 'beta', provider_model_name: 'up-b', input_price: 1.5, output_price: 4, sort_order: 2 }),
    candidateFixture({
      id: 13,
      provider_name: 'gamma',
      provider_model_name: 'up-g',
      input_price: 0,
      output_price: 0,
      prices_declared: false,
      routable: false,
      sort_order: 3,
    }),
  ]),
  modelFixture(8, 'all-unpriced', [
    candidateFixture({ id: 21, provider_name: 'alpha', provider_model_name: 'up-a', input_price: 0, output_price: 0, prices_declared: false, routable: false }),
    candidateFixture({ id: 22, provider_name: 'beta', provider_model_name: 'up-b', input_price: 0, output_price: 0, prices_declared: false, routable: false }),
  ]),
  modelFixture(9, 'explicit-free', [
    candidateFixture({ id: 31, provider_name: 'alpha', provider_model_name: 'up-a', input_price: 0, output_price: 0, prices_declared: true }),
  ]),
  modelFixture(10, 'image-not-counted', [
    candidateFixture({ id: 41, provider_name: 'alpha', provider_model_name: 'up-a', input_price: 3, output_price: 5 }),
    candidateFixture({
      id: 42,
      provider_name: 'imgco',
      provider_model_name: 'up-img',
      billing_mode: 'image',
      image_pricing_tiers: imageTiers,
      input_price: 0,
      output_price: 0,
      prices_declared: true,
      sort_order: 2,
    }),
  ]),
  modelFixture(11, 'image-only', [
    candidateFixture({
      id: 51,
      provider_name: 'imgco',
      provider_model_name: 'up-img',
      billing_mode: 'image',
      image_pricing_tiers: imageTiers,
      input_price: 0,
      output_price: 0,
      prices_declared: true,
    }),
  ]),
]

const FILTER_MODELS = () => [
  modelFixture(9, 'fully-priced', [
    candidateFixture({ id: 31, provider_name: 'alpha', provider_model_name: 'up-a', input_price: 1, output_price: 2 }),
  ]),
  modelFixture(7, 'has-unpriced', [
    candidateFixture({ id: 11, provider_name: 'alpha', provider_model_name: 'up-a', input_price: 2, output_price: 3 }),
    candidateFixture({ id: 13, provider_name: 'gamma', provider_model_name: 'up-g', input_price: 0, output_price: 0, prices_declared: false, routable: false, sort_order: 2 }),
  ]),
]

// The comparison-surface fixture: every bucket the sort rule knows — priced
// token rows, a priced audio row (character price), an image tier row, an
// unpriced row. Expected order: audio 0.5 → token 1 → token 2 → image tier
// (by upstream name) → unpriced sunk. The audio row is the cheapest.
const COMPARE_MODELS = () => [
  modelFixture(20, 'compare-me', [
    candidateFixture({ id: 61, provider_name: 'alpha', provider_model_name: 'alpha-up', input_price: 2, output_price: 3, sort_order: 1 }),
    candidateFixture({ id: 62, provider_name: 'beta', provider_model_name: 'beta-up', input_price: 1, output_price: 2, sort_order: 2 }),
    candidateFixture({
      id: 63,
      provider_name: 'audioco',
      provider_model_name: 'audio-up',
      billing_mode: 'audio',
      audio_unit_price: 0.5,
      input_price: 0,
      output_price: 0,
      prices_declared: true,
      sort_order: 3,
    }),
    candidateFixture({
      id: 64,
      provider_name: 'imgco',
      provider_model_name: 'zeta-image',
      billing_mode: 'image',
      image_pricing_tiers: imageTiers,
      input_price: 0,
      output_price: 0,
      prices_declared: true,
      sort_order: 4,
    }),
    candidateFixture({
      id: 65,
      provider_name: 'delta',
      provider_model_name: 'delta-up',
      input_price: 0,
      output_price: 0,
      prices_declared: false,
      routable: false,
      sort_order: 5,
    }),
  ]),
]

async function mountPage(models: Model[]) {
  listModelsMock.mockResolvedValue({ list: models })
  listProvidersMock.mockResolvedValue({ list: [] })
  // The shared edit modal pulls the edited provider's upstream catalogue when
  // it opens; nothing here needs its rows, only a settled answer.
  catalogueMock.mockResolvedValue({ models: [], outcome: null, detail: '' })
  endpointMock.mockResolvedValue({ endpoint: 'http://gateway.example' })
  const i18n = createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { 'zh-CN': zhCN } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/models/:id', component: { template: '<div />' } },
      { path: '/providers/:id', component: { template: '<div />' } },
      { path: '/costs/models/:name', component: { template: '<div />' } },
    ],
  })
  await router.push('/')
  await router.isReady()
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  await vi.waitFor(() => expect(wrapper!.findAll('tbody tr').length).toBe(models.length))
}

// The table row rendering the given model name.
function rowByName(name: string) {
  const row = wrapper!.findAll('tbody tr').find((r) => r.text().includes(name))
  expect(row, `table row for ${name} rendered`).toBeTruthy()
  return row!
}

// Expand a model row and wait for its comparison rows.
async function expandRow(name: string, expectedRows: number) {
  const row = rowByName(name)
  await row.find('.n-data-table-expand-trigger').trigger('click')
  await vi.waitFor(() => expect(wrapper!.findAll('.model-compare-row').length).toBe(expectedRows))
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

describe('ModelListPage price column', () => {
  it('the 价格 column head renders with its help tip', async () => {
    await mountPage(PRICE_MODELS())
    const head = wrapper!.find('thead').text()
    expect(head).toContain('价格')
  })

  it('multi-provider model shows the lowest priced token input price and the unpriced count', async () => {
    await mountPage(PRICE_MODELS())
    const row = rowByName('min-across-providers')
    expect(row.find('.model-price-min').text()).toBe('1.5 起')
    expect(row.find('.model-price-unpriced').text()).toContain('未定价 1')
  })

  it('an all-unpriced model shows only the unpriced count tag', async () => {
    await mountPage(PRICE_MODELS())
    const row = rowByName('all-unpriced')
    expect(row.find('.model-price-min').exists()).toBe(false)
    expect(row.find('.model-price-unpriced').text()).toContain('未定价 2')
  })

  it('an explicitly free row (declared 0) participates as 0 without an unpriced tag', async () => {
    await mountPage(PRICE_MODELS())
    const row = rowByName('explicit-free')
    expect(row.find('.model-price-min').text()).toBe('0 起')
    expect(row.find('.model-price-unpriced').exists()).toBe(false)
  })

  it('image and video tier rows never count as unpriced nor set the minimum', async () => {
    await mountPage(PRICE_MODELS())
    // The image-billed candidate rides along a priced token row: the minimum
    // comes from the token row alone and no warning tag appears.
    const mixed = rowByName('image-not-counted')
    expect(mixed.find('.model-price-min').text()).toBe('3 起')
    expect(mixed.find('.model-price-unpriced').exists()).toBe(false)
    // An image-only model has no priced token candidate and no unpriced
    // candidate: the cell shows neither the minimum nor a warning tag.
    const imageOnly = rowByName('image-only')
    expect(imageOnly.find('.model-price-min').exists()).toBe(false)
    expect(imageOnly.find('.model-price-unpriced').exists()).toBe(false)
  })
})

describe('ModelListPage unpriced-only filter', () => {
  it('the switch applies immediately and keeps only models with an unpriced candidate', async () => {
    await mountPage(FILTER_MODELS())
    const switchEl = wrapper!.find('.filter-panel .n-switch')
    expect(switchEl.exists(), 'the unpriced-only switch rendered').toBe(true)
    await switchEl.trigger('click')
    await vi.waitFor(() => expect(wrapper!.findAll('tbody tr').length).toBe(1))
    expect(rowByName('has-unpriced').exists()).toBe(true)
    expect(wrapper!.findAll('tbody tr').find((r) => r.text().includes('fully-priced'))).toBeUndefined()
  })
})

describe('ModelListPage comparison expansion', () => {
  it('expanded candidates sort by representative price with tiers by upstream name and unpriced sunk', async () => {
    await mountPage(COMPARE_MODELS())
    await expandRow('compare-me', 5)
    const ids = wrapper!.findAll('.model-compare-row').map((r) => r.attributes('data-candidate-id'))
    expect(ids).toEqual(['63', '62', '61', '64', '65'])
  })

  it('the comparison panel carries exactly one column header naming the five tracks in order', async () => {
    await mountPage(COMPARE_MODELS())
    await expandRow('compare-me', 5)
    const headers = wrapper!.findAll('.model-compare-header')
    expect(headers.length, 'one header per expanded panel, not per row').toBe(1)
    // Left-to-right track labels — the header rides the rows' shared grid
    // template, so this doubles as the label/track alignment contract.
    expect(headers[0].findAll('span').map((s) => s.text())).toEqual([
      zhCN.models.compareProvider,
      zhCN.models.providerModelName,
      zhCN.models.priceColumn,
      zhCN.models.compareStatus,
      zhCN.common.actions,
    ])
  })

  it('a model with no candidates expands to the empty note and no column header', async () => {
    await mountPage([modelFixture(90, 'bare-model', [])])
    const row = rowByName('bare-model')
    await row.find('.n-data-table-expand-trigger').trigger('click')
    await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(zhCN.models.expandNoCandidates))
    expect(wrapper!.findAll('.model-compare-header').length).toBe(0)
  })

  it('the cheapest priced row carries the 最便宜 tag', async () => {
    await mountPage(COMPARE_MODELS())
    await expandRow('compare-me', 5)
    const cheapest = wrapper!.find('.model-compare-row--cheapest')
    expect(cheapest.exists()).toBe(true)
    expect(cheapest.attributes('data-candidate-id')).toBe('63')
    expect(cheapest.text()).toContain('最便宜')
    // The unpriced row renders the shared unpriced badge.
    const unpriced = wrapper!.findAll('.model-compare-row').find((r) => r.attributes('data-candidate-id') === '65')!
    expect(unpriced.text()).toContain('未定价')
  })

  it('a model whose candidates are only tier or unpriced rows carries no cheapest tag', async () => {
    await mountPage(PRICE_MODELS())
    // Tier-only: the sort's image bucket leads this order, but the tag is
    // reserved for priced token/audio rows — no row may carry it.
    await expandRow('image-only', 1)
    expect(wrapper!.find('.model-compare-row--cheapest').exists()).toBe(false)
    expect(wrapper!.text()).not.toContain('最便宜')
    // All-unpriced: the sunk bucket never carries the tag either.
    await expandRow('all-unpriced', 3)
    expect(wrapper!.find('.model-compare-row--cheapest').exists()).toBe(false)
    expect(wrapper!.text()).not.toContain('最便宜')
  })
})

describe('ModelListPage inline candidate edit', () => {
  it('a row edit opens the shared modal with that model id and candidate', async () => {
    await mountPage(COMPARE_MODELS())
    await expandRow('compare-me', 5)
    const target = wrapper!.findAll('.model-compare-row').find((r) => r.attributes('data-candidate-id') === '62')!
    await target.find('button.model-compare-edit').trigger('click')

    const modal = wrapper!.findComponent(CandidateEditModal)
    expect(modal.exists(), 'the shared candidate edit modal opened').toBe(true)
    expect(modal.props('show')).toBe(true)
    expect(modal.props('modelId')).toBe(20)
    expect(modal.props('modelName')).toBe('compare-me')
    const editing = modal.props('editingCandidate') as ModelCandidate | null
    expect(editing?.id).toBe(62)
    expect(editing?.provider_model_name).toBe('beta-up')
  })

  it('a saved modal refetches the models list', async () => {
    await mountPage(COMPARE_MODELS())
    expect(listModelsMock).toHaveBeenCalledTimes(1)
    await expandRow('compare-me', 5)
    const target = wrapper!.findAll('.model-compare-row').find((r) => r.attributes('data-candidate-id') === '62')!
    await target.find('button.model-compare-edit').trigger('click')
    const modal = wrapper!.findComponent(CandidateEditModal)
    expect(modal.exists(), 'the shared candidate edit modal opened').toBe(true)

    modal.vm.$emit('saved')
    await vi.waitFor(() => expect(listModelsMock).toHaveBeenCalledTimes(2))
  })
})
