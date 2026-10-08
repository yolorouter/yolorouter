// @vitest-environment happy-dom
//
// The model detail page's route-table search + verification-result filter —
// the second leg of the both-pages requirement. The provider detail page's
// mapping table runs the same composable (ProviderDetailPage.candidateFilter
// .test.ts asserts that leg against the ProviderCandidate row shape); this
// file pins the identical semantics against this page's ModelCandidate rows
// and its own two searchable name columns (provider / provider model name).
//
// Covered acceptance cases:
//
//   TC-06 search: substring match over provider name and provider model
//        name, case-insensitive, over the model's whole candidate chain;
//        the placeholder composes the existing column-title terms
//        (models.provider / models.providerModelName)
//   TC-07 the three verification buckets on this page's rows too, with the
//        unverified bucket merging the probed-but-inconclusive and the
//        never-probed rows; clearing the select restores the whole chain
//   TC-16 search + bucket intersect; no match renders the tab's existing
//        empty state; clearing either condition restores
//   TC-17 the filter sees the whole chain — after flipping to page 2, a
//        keyword restarts at page 1
//
// Fixtures ride the same API-module mocks as the page's baseline suite;
// assertions read the zh-CN copy through the locale module so a copy edit
// does not fake a regression.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive, { NInput } from 'naive-ui'
import { defineComponent, nextTick, type ComponentPublicInstance } from 'vue'

import {
  getModel,
  getModelImpact,
  type Model,
  type ModelCandidate,
} from '../../api/models'
import { listModelsForProvider } from '../../api/providers'

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return {
    ...actual,
    getModel: vi.fn(),
    getModelImpact: vi.fn(),
  }
})

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return { ...actual, listModelsForProvider: vi.fn() }
})

const getModelMock = vi.mocked(getModel)
const getImpactMock = vi.mocked(getModelImpact)
const catalogueMock = vi.mocked(listModelsForProvider)

// Must be imported after the vi.mock factories are registered.
import ModelDetailPage from './ModelDetailPage.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import zhCN from '../../locales/zh-CN'
import { VERIFICATION_FAILED, VERIFICATION_PASSED, VERIFICATION_UNTESTED } from '../../api/candidateStatus'

const MODEL_ID = 7

// App.vue nests pages under n-config-provider > n-message-provider >
// n-dialog-provider; the page's useMessage()/useDialog() throw without that
// ancestry.
const Host = defineComponent({
  components: { ModelDetailPage },
  template:
    '<NConfigProvider><NMessageProvider><NDialogProvider><ModelDetailPage /></NDialogProvider></NMessageProvider></NConfigProvider>',
})

let wrapper: VueWrapper | null = null

// Desktop viewport (same stub shape as the page's other tests): useIsMobile
// needs matchMedia, and the desktop filter bar is what is asserted here.
function stubDesktopMatchMedia() {
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
    cache_read_price: 0.1,
    max_output: 4096,
    billing_mode: 'token',
    image_pricing_tiers: null,
    video_pricing_tiers: null,
    audio_unit_price: null,
    supports_streaming: true,
    supports_function_calling: null,
    management_status: 1,
    sort_order: 1,
    verification_status: VERIFICATION_PASSED,
    routable: true,
    blocked_by: '',
    last_test_result: 0,
    last_test_duration_ms: 120,
    last_tested_at: '2026-09-01T00:00:00Z',
    binding_count: 4,
    ...overrides,
  }
}

// The chain the bucket assertions work against — one candidate per
// verification flavor, including the two UNTESTED kinds the unverified
// bucket must merge:
//
//   alpha  verification PASSED
//   beta   verification FAILED (probed, decisive verdict)
//   gamma  verification UNTESTED but last_tested_at stamped — a probe DID
//          run and produced no verdict
//   delta  verification UNTESTED, never tested at all
function bucketFixtureCandidates(): ModelCandidate[] {
  return [
    candidateFixture(),
    candidateFixture({
      id: 2,
      provider_id: 11,
      provider_name: 'beta',
      provider_model_name: 'beta-model',
      verification_status: VERIFICATION_FAILED,
      last_test_result: 7,
      routable: false,
      blocked_by: 'not_verified',
      sort_order: 2,
    }),
    candidateFixture({
      id: 3,
      provider_id: 12,
      provider_name: 'gamma',
      provider_model_name: 'gamma-model',
      verification_status: VERIFICATION_UNTESTED,
      last_test_result: 7,
      routable: false,
      blocked_by: 'not_verified',
      sort_order: 3,
    }),
    candidateFixture({
      id: 4,
      provider_id: 13,
      provider_name: 'delta',
      provider_model_name: 'delta-model',
      verification_status: VERIFICATION_UNTESTED,
      last_test_result: null,
      last_tested_at: null,
      routable: false,
      blocked_by: 'not_verified',
      sort_order: 4,
    }),
  ]
}

function modelFixture(candidates: ModelCandidate[]): Model {
  return {
    id: MODEL_ID,
    name: 'gpt-demo',
    management_status: 1,
    running_status: 'available',
    scheduling_mode: 'failover',
    supports_image_input: null,
    output_modalities: ['text'],
    candidates,
    created_at: '2026-09-01T00:00:00Z',
  }
}

async function mountPage(candidates: ModelCandidate[]) {
  getModelMock.mockResolvedValue(modelFixture(candidates))
  getImpactMock.mockResolvedValue({
    allowlisted_keys: [],
    allow_all_key_count: 0,
    candidate_count: candidates.length,
    recent_request_count: 0,
    recent_window_days: 30,
  })
  catalogueMock.mockResolvedValue({ models: [], outcome: null, detail: '' })
  const i18n = createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { 'zh-CN': zhCN } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/models', component: { template: '<div />' } },
      { path: '/models/:id', component: { template: '<div />' } },
    ],
  })
  await router.push(`/models/${MODEL_ID}`)
  await router.isReady()
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(zhCN.models.tabRoute))
  await nextTick()
}

// The rows currently rendered in the table, as one text blob per <tr>.
function rowTexts(): string[] {
  return [...document.body.querySelectorAll('tr')].map((el) => el.textContent ?? '')
}

function expectRows(visible: string[], absent: string[]) {
  for (const name of visible) {
    expect(rowTexts().some((text) => text.includes(name)), `row for ${name} is rendered`).toBe(true)
  }
  for (const name of absent) {
    expect(rowTexts().some((text) => text.includes(name)), `row for ${name} is NOT rendered`).toBe(false)
  }
}

// The route table's search input, located by its composed placeholder so
// the assertion doubles as a check on the placeholder terms (TC-06).
// Naive-ui's exported component types do not register as component selectors
// for findAllComponents' typing, so the wrappers are cast to a shape that
// exposes just the props read here (same approach as the request-logs page
// tests); emits go through a bare VueWrapper cast.
type SearchInputWrapper = VueWrapper<unknown, ComponentPublicInstance<{ placeholder?: string }>>
type FilterSelectWrapper = VueWrapper<
  unknown,
  ComponentPublicInstance<{
    label?: string
    placeholder?: string
    options?: Array<{ label: string; value: string }>
  }>
>

function searchField(): SearchInputWrapper {
  const expected = `${zhCN.models.provider} / ${zhCN.models.providerModelName}`
  const inputs = wrapper!.findAllComponents(NInput) as unknown as SearchInputWrapper[]
  const field = inputs.find((c) => c.props('placeholder') === expected)
  expect(field, `the search input (placeholder "${expected}") rendered`).toBeTruthy()
  return field!
}

async function typeKeyword(keyword: string) {
  ;(searchField() as unknown as VueWrapper).vm.$emit('update:value', keyword)
  await nextTick()
  await nextTick()
}

// The route table's verification-result select.
function filterSelect(): FilterSelectWrapper {
  const selects = wrapper!.findAllComponents(FilterSelectField) as unknown as FilterSelectWrapper[]
  expect(selects.length, 'the verification filter select rendered').toBeGreaterThanOrEqual(1)
  return selects[0]
}

async function pickBucket(value: 'passed' | 'failed' | 'unverified' | null) {
  ;(filterSelect() as unknown as VueWrapper).vm.$emit('update:value', value)
  await nextTick()
  await nextTick()
}

function bodyText(): string {
  return document.body.textContent ?? ''
}

beforeEach(() => {
  stubDesktopMatchMedia()
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('ModelDetailPage route-table search + verification filter (TC-06/07/16/17)', () => {
  it('TC-06: substring search over provider name and provider model name, case-insensitive, over the whole chain', async () => {
    await mountPage(bucketFixtureCandidates())

    // By provider name.
    await typeKeyword('beta')
    expectRows(['beta-model'], ['alpha-model', 'gamma-model', 'delta-model'])

    // By provider model name.
    await typeKeyword('gamma-model')
    expectRows(['gamma-model'], ['alpha-model', 'beta-model', 'delta-model'])

    // Case-insensitive substring, and clearing restores the whole chain.
    await typeKeyword('BETA')
    expectRows(['beta-model'], ['alpha-model', 'gamma-model', 'delta-model'])
    await typeKeyword('')
    expectRows(['alpha-model', 'beta-model', 'gamma-model', 'delta-model'], [])

    // The placeholder composes the two searchable columns' existing title
    // terms — the providerModelName term is reused, not re-coined.
    const placeholder = searchField().props('placeholder') as string
    expect(placeholder).toBe(`${zhCN.models.provider} / ${zhCN.models.providerModelName}`)
    expect(placeholder).toContain(zhCN.models.providerModelName)
  })

  it('TC-07: buckets map onto the raw three-value verification_status; unverified merges the inconclusive and never-probed rows', async () => {
    await mountPage(bucketFixtureCandidates())

    await pickBucket('passed')
    expectRows(['alpha-model'], ['beta-model', 'gamma-model', 'delta-model'])

    await pickBucket('failed')
    expectRows(['beta-model'], ['alpha-model', 'gamma-model', 'delta-model'])

    // The unverified bucket holds BOTH UNTESTED flavors: the row a probe ran
    // on without a verdict, and the row no probe was ever scheduled for.
    await pickBucket('unverified')
    expectRows(['gamma-model', 'delta-model'], ['alpha-model', 'beta-model'])

    // Back to "all": clearing the select restores the whole chain.
    await pickBucket(null)
    expectRows(['alpha-model', 'beta-model', 'gamma-model', 'delta-model'], [])

    // Same option set as the provider page's filter (the both-pages
    // consistency leg): the existing passed/failed terms plus the new
    // unverified term, with stable bucket values.
    const options = filterSelect().props('options') as Array<{ label: string; value: string }>
    expect(options).toEqual([
      { label: zhCN.providers.verificationPassed, value: 'passed' },
      { label: zhCN.providers.verificationFailed, value: 'failed' },
      { label: zhCN.providers.verificationUnverified, value: 'unverified' },
    ])
  })

  it('TC-16: search and bucket intersect; no match renders the existing empty state; clearing either restores', async () => {
    await mountPage(bucketFixtureCandidates())

    // Both conditions at once: the keyword alone matches all four provider
    // model names, the bucket alone two rows — together their intersection.
    await typeKeyword('model')
    await pickBucket('unverified')
    expectRows(['gamma-model', 'delta-model'], ['alpha-model', 'beta-model'])

    // A combination nothing matches: the tab's existing empty copy shows in
    // the table's empty slot and no row renders.
    await typeKeyword('zzz-no-match')
    expectRows([], ['alpha-model', 'beta-model', 'gamma-model', 'delta-model'])
    expect(bodyText()).toContain(zhCN.models.routeChainEmpty)

    // Clearing the BUCKET (keeping the keyword) restores that keyword's own
    // result — nothing, since the keyword matches no row on its own.
    await pickBucket(null)
    expectRows([], ['alpha-model', 'beta-model', 'gamma-model', 'delta-model'])

    // Clearing the KEYWORD (no bucket active) restores the whole chain.
    await typeKeyword('')
    expectRows(['alpha-model', 'beta-model', 'gamma-model', 'delta-model'], [])
  })

  it('TC-17: the filter sees the whole chain — after flipping to page 2, a keyword restarts at page 1', async () => {
    // 21 candidates: bulk-01..bulk-20 fill page 1, tail-row is page 2's only
    // row (page size defaults to 20).
    const candidates: ModelCandidate[] = []
    for (let i = 1; i <= 20; i++) {
      const n = String(i).padStart(2, '0')
      candidates.push(
        candidateFixture({
          id: i,
          provider_id: 100 + i,
          provider_name: `bulk-${n}`,
          provider_model_name: `bm-${n}`,
          sort_order: i,
        }),
      )
    }
    candidates.push(candidateFixture({ id: 21, provider_id: 121, provider_name: 'tail-row', provider_model_name: 'bm-tail', sort_order: 21 }))
    await mountPage(candidates)

    // Flip to page 2 like a user: only the tail row renders there.
    const page2 = [...document.body.querySelectorAll('.n-pagination .n-pagination-item')].find(
      (el) => (el.textContent ?? '').trim() === '2',
    )
    expect(page2, 'the pager offers a page 2').toBeTruthy()
    page2!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await nextTick()
    await nextTick()
    expectRows(['bm-tail'], ['bulk-01', 'bulk-02'])

    // Typing a keyword matches rows that live on page 1 of the unfiltered
    // chain — if the filter only saw the current page (or did not reset the
    // pager), nothing would render. With the reset, page 1's hits show.
    await typeKeyword('bulk-')
    expectRows(['bulk-01', 'bulk-02', 'bulk-20'], ['tail-row'])

    // Clearing the keyword restores the whole chain — still on page 1, so
    // the tail row stays one pager click away rather than lost past the end.
    await typeKeyword('')
    expectRows(['bulk-01', 'bulk-20'], ['bm-tail'])
  })
})
