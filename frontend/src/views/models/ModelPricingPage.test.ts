// @vitest-environment happy-dom
//
// The model pricing page's baseline: the /model-pricing component fetches
// GET /api/admin/models once on mount (plus on the manual refresh), builds
// one collapsed group per model with candidates (model-name asc, models
// without mappings never appear), and the group headers carry the pricing
// summary — provider count, lowest priced token input price, unpriced
// count — and the row sort inside a group. Assertions read the zh-CN copy
// through the locale modules so a copy edit cannot fake a failure, while a
// wiring change (dropped metric, wrong sort, broken fetch) still fails.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent } from 'vue'

import { listModels, type Model, type ModelCandidate } from '../../api/models'

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return { ...actual, listModels: vi.fn() }
})

const listModelsMock = vi.mocked(listModels)

// Must be imported after the vi.mock factories are registered.
import ModelPricingPage from './ModelPricingPage.vue'
import zhCN from '../../locales/zh-CN'

// App.vue nests pages under n-config-provider > n-message-provider; the same
// host here means the page mounts exactly as it does inside the shell.
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
    provider_model_name: 'alpha-model',
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

function modelFixture(id: number, name: string, candidates: ModelCandidate[], managementStatus = 1): Model {
  return {
    id,
    name,
    management_status: managementStatus,
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

async function mountPage(models: Model[]) {
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
  await vi.waitFor(() => expect(listModelsMock).toHaveBeenCalled())
  await vi.waitFor(() => expect(wrapper!.find('.pricing-group').exists() || wrapper!.find('.empty-state').exists()).toBe(true))
}

// The rendered group section whose header names the given model.
function groupByName(name: string) {
  const group = wrapper!.findAll('.pricing-group').find((g) => g.find('.pricing-group__name').text() === name)
  expect(group, `group for model ${name} rendered`).toBeTruthy()
  return group!
}

// Expands every group so the rows render, then returns the group's rows.
async function expandedRows(name: string) {
  const expandAll = wrapper!.findAll('button').find((b) => (b.text() ?? '').includes(zhCN.modelPricing.expandAll))
  expect(expandAll, 'the expand-all control rendered').toBeTruthy()
  expandAll!.trigger('click')
  await vi.waitFor(() => expect(wrapper!.findAll('.pricing-row').length).toBeGreaterThan(0))
  return groupByName(name).findAll('.pricing-row')
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

describe('ModelPricingPage data fetch', () => {
  it('fetches the models list once on mount and keeps the page header', async () => {
    await mountPage([modelFixture(1, 'a-model', [candidateFixture()])])
    expect(listModelsMock).toHaveBeenCalledTimes(1)
    expect(wrapper!.find('h1.page-header__title').text()).toContain(zhCN.modelPricing.pageTitle)
  })

  it('the refresh button refetches the list', async () => {
    await mountPage([modelFixture(1, 'a-model', [candidateFixture()])])
    expect(listModelsMock).toHaveBeenCalledTimes(1)
    const refresh = wrapper!.findAll('button').find((b) => (b.text() ?? '').includes(zhCN.modelPricing.refresh))
    expect(refresh, 'refresh button rendered').toBeTruthy()
    refresh!.trigger('click')
    await vi.waitFor(() => expect(listModelsMock).toHaveBeenCalledTimes(2))
  })
})

describe('ModelPricingPage grouping and group headers', () => {
  it('renders one collapsed group per model with candidates, model-name asc, and hides mapping-less models', async () => {
    await mountPage([
      modelFixture(2, 'b-model', [candidateFixture({ id: 2, provider_name: 'beta', provider_model_name: 'beta-up' })]),
      modelFixture(1, 'a-model', [candidateFixture({ id: 1, provider_name: 'alpha', provider_model_name: 'alpha-up' })]),
      modelFixture(3, 'c-empty', []),
    ])
    const names = wrapper!.findAll('.pricing-group__name').map((n) => n.text())
    expect(names).toEqual(['a-model', 'b-model'])
    // Default collapsed: no mapping rows render until a group is expanded.
    expect(wrapper!.findAll('.pricing-row').length).toBe(0)
    // A model without candidates never appears at all.
    expect(wrapper!.text()).not.toContain('c-empty')
  })

  it('the mixed billing group header counts providers, takes the lowest priced token input price, and excludes image/video rows from the unpriced count', async () => {
    await mountPage([
      modelFixture(1, 'gpt-mixed', [
        candidateFixture({ id: 1, provider_name: 'pa', input_price: 2.5, output_price: 5 }),
        candidateFixture({ id: 2, provider_name: 'pb', input_price: 1.5, output_price: 3 }),
        candidateFixture({ id: 3, provider_name: 'pc', billing_mode: 'audio', audio_unit_price: 0.8, input_price: 0, output_price: 0 }),
        candidateFixture({
          id: 4,
          provider_name: 'pd',
          billing_mode: 'image',
          input_price: 0,
          output_price: 0,
          image_pricing_tiers: { mode: 'standard', tiers: [{ quality: '', size: '', price: 0.5 }] },
        }),
        // An anomalous image row the server flagged undeclared: image rows
        // always carry their tier prices, so it must NOT count as unpriced.
        candidateFixture({ id: 5, provider_name: 'pe', billing_mode: 'image', prices_declared: false, image_pricing_tiers: null }),
        candidateFixture({ id: 6, provider_name: 'pf', prices_declared: false, input_price: 0, output_price: 0 }),
        candidateFixture({
          id: 7,
          provider_name: 'pg',
          billing_mode: 'video',
          input_price: 0,
          output_price: 0,
          video_pricing_tiers: { tiers: [{ resolution: '', purchase_price: 0.1, sell_price: 0.2 }] },
        }),
      ]),
    ])
    const group = groupByName('gpt-mixed')
    expect(group.find('.pricing-group__providers').text()).toContain(zhCN.modelPricing.providerCount)
    expect(group.find('.pricing-group__providers').text()).toContain('7')
    // Lowest priced token input price: 1.5 (the audio character price and
    // the tier rows never join this comparison).
    expect(group.find('.pricing-group__min-price').exists(), 'the lowest input price renders').toBe(true)
    expect(group.find('.pricing-group__min-price').text()).toContain('1.5')
    // Exactly one unpriced row (pf) — the flagged image row does not count.
    expect(group.find('.pricing-group__unpriced').exists(), 'the unpriced warning renders').toBe(true)
    expect(group.find('.pricing-group__unpriced').text()).toContain('1')
  })

  it('an all-unpriced group hides the lowest price and warns with the full count', async () => {
    await mountPage([
      modelFixture(1, 'gpt-unpriced', [
        candidateFixture({ id: 1, provider_name: 'pa', prices_declared: false, input_price: 0, output_price: 0 }),
        candidateFixture({ id: 2, provider_name: 'pb', prices_declared: false, input_price: 0, output_price: 0 }),
      ]),
    ])
    const group = groupByName('gpt-unpriced')
    expect(group.find('.pricing-group__min-price').exists(), 'no lowest price without a declared token row').toBe(false)
    expect(group.find('.pricing-group__unpriced').text()).toContain('2')
  })

  it('an explicitly free declared token row participates and the minimum reads 0', async () => {
    await mountPage([
      modelFixture(1, 'gpt-free', [
        candidateFixture({ id: 1, provider_name: 'pa', prices_declared: true, input_price: 0, output_price: 0 }),
        candidateFixture({ id: 2, provider_name: 'pb', prices_declared: true, input_price: 3, output_price: 6 }),
      ]),
    ])
    const group = groupByName('gpt-free')
    expect(group.find('.pricing-group__min-price').exists(), 'the lowest price renders for a 0-priced declared row').toBe(true)
    expect(group.find('.pricing-group__min-price').text()).toContain('0')
    expect(group.find('.pricing-group__unpriced').exists(), 'a declared free row is not unpriced').toBe(false)
  })

  it('a group with only priced audio rows hides the lowest input price and carries no unpriced warning', async () => {
    await mountPage([
      modelFixture(1, 'tts-model', [
        candidateFixture({ id: 1, provider_name: 'pa', billing_mode: 'audio', audio_unit_price: 0.9, input_price: 0, output_price: 0 }),
        candidateFixture({ id: 2, provider_name: 'pb', billing_mode: 'audio', audio_unit_price: 1.2, input_price: 0, output_price: 0 }),
      ]),
    ])
    const group = groupByName('tts-model')
    expect(group.find('.pricing-group__min-price').exists(), 'audio character prices never mix into the token minimum').toBe(false)
    expect(group.find('.pricing-group__unpriced').exists(), 'priced audio rows are not unpriced').toBe(false)
    expect(group.find('.pricing-group__providers').text()).toContain('2')
  })

  it('a disabled model carries the disabled badge on its group header', async () => {
    await mountPage([
      modelFixture(1, 'off-model', [candidateFixture()], 2),
      modelFixture(2, 'on-model', [candidateFixture({ id: 2 })], 1),
    ])
    expect(groupByName('off-model').find('.pricing-group__model-disabled').exists(), 'disabled badge on the off model').toBe(true)
    expect(groupByName('on-model').find('.pricing-group__model-disabled').exists(), 'no badge on the on model').toBe(false)
  })
})

describe('ModelPricingPage row sorting', () => {
  it('sorts a heterogeneous group by representative price asc, then tier rows by upstream name, unpriced last', async () => {
    await mountPage([
      modelFixture(1, 'gpt-hetero', [
        // Priced rows: audio 0.5 < token 1.0 < token 3.0 (name order must
        // NOT override the price order among priced rows).
        candidateFixture({ id: 1, provider_name: 'pz', provider_model_name: 'up-zeta', input_price: 3, output_price: 4 }),
        candidateFixture({ id: 2, provider_name: 'pa', provider_model_name: 'up-alpha-token', input_price: 1, output_price: 2 }),
        candidateFixture({ id: 3, provider_name: 'py', provider_model_name: 'up-audio', billing_mode: 'audio', audio_unit_price: 0.5, input_price: 0, output_price: 0 }),
        // Tier rows follow the priced rows, ordered by upstream name.
        candidateFixture({
          id: 4,
          provider_name: 'pb',
          provider_model_name: 'b-video-up',
          billing_mode: 'video',
          video_pricing_tiers: { tiers: [{ resolution: '', purchase_price: 0.1, sell_price: 0.2 }] },
        }),
        candidateFixture({
          id: 5,
          provider_name: 'pc',
          provider_model_name: 'a-image-up',
          billing_mode: 'image',
          image_pricing_tiers: { mode: 'standard', tiers: [{ quality: '', size: '', price: 0.5 }] },
        }),
        // Unpriced rows sink to the bottom.
        candidateFixture({ id: 6, provider_name: 'pd', provider_model_name: 'up-free', prices_declared: false, input_price: 0, output_price: 0 }),
      ]),
    ])
    const rows = await expandedRows('gpt-hetero')
    const upstreams = rows.map((row) => row.find('.pricing-row__upstream-name').text())
    expect(upstreams).toEqual(['up-audio', 'up-alpha-token', 'up-zeta', 'a-image-up', 'b-video-up', 'up-free'])
  })
})
