// @vitest-environment happy-dom
//
// The provider detail page's mapping-table search + verification-result
// filter. The same composable drives the model detail page's route table
// (ModelDetailPage.candidateFilter.test.ts asserts that leg); together the
// two files pin the "both pages behave identically" requirement by running
// the same semantics against each page's own row shape.
//
// Covered acceptance cases:
//
//   TC-06 search: substring match over model name and provider model name,
//        case-insensitive, over the fully-fetched list; the placeholder
//        composes the existing column-title terms (models.name /
//        models.providerModelName), coining no new vocabulary
//   TC-07 the three verification buckets: passed = verification_status
//        PASSED, failed = FAILED, unverified = UNTESTED — which includes
//        BOTH the probed-but-inconclusive row and the never-probed row —
//        and clearing the select restores the full list
//   TC-15 the new copy is present in zh-CN and en, the unverified bucket is
//        deliberately NOT worded as "untested", and neither locale contains
//        a coined "Upstream model name" term in either locale's spelling
//   TC-16 search + bucket intersect; a combination nothing matches renders
//        the tab's existing empty state; clearing either condition restores
//        that condition's own result
//   TC-17 the filter sees the WHOLE list, not the current page: after
//        flipping to page 2, typing a keyword restarts at page 1
//
// Fixtures ride the same API-module mocks as every other page test here;
// assertions read the zh-CN copy through the locale module so a copy edit
// does not fake a regression.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive, { NInput } from 'naive-ui'
import { defineComponent, nextTick, type ComponentPublicInstance } from 'vue'

import { getProvider, type Provider, type ProviderKey } from '../../api/providers'
import { listProviderCandidates, type ProviderCandidate } from '../../api/models'

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return { ...actual, getProvider: vi.fn() }
})

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return { ...actual, listProviderCandidates: vi.fn() }
})

const getProviderMock = vi.mocked(getProvider)
const listCandidatesMock = vi.mocked(listProviderCandidates)

// Must be imported after the vi.mock factories are registered.
import ProviderDetailPage from './ProviderDetailPage.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import zhCN from '../../locales/zh-CN'
import en from '../../locales/en'
import {
  CANDIDATE_STATUS_DISABLED,
  CANDIDATE_STATUS_ENABLED,
  VERIFICATION_FAILED,
  VERIFICATION_PASSED,
  VERIFICATION_UNTESTED,
} from '../../api/candidateStatus'

const PROVIDER_ID = 5

// App.vue nests pages under n-config-provider > n-message-provider >
// n-dialog-provider; the page's useMessage()/useDialog() throw without that
// ancestry.
const Host = defineComponent({
  components: { ProviderDetailPage },
  template:
    '<NConfigProvider><NMessageProvider><NDialogProvider><ProviderDetailPage /></NDialogProvider></NMessageProvider></NConfigProvider>',
})

let wrapper: VueWrapper | null = null

// A controllable MediaQueryList (same stub shape as the other page tests):
// desktop is what the filter bar asserts here.
function stubMatchMedia(initialMobile: boolean) {
  const listeners = new Set<(e: { matches: boolean }) => void>()
  const mql = {
    matches: initialMobile,
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

function keyFixture(): ProviderKey {
  return {
    id: 1,
    label: 'primary',
    key_prefix: 'sk-abcd',
    sort_order: 1,
    test_model: 'gpt-x',
    management_status: CANDIDATE_STATUS_ENABLED,
    verification_status: VERIFICATION_PASSED,
    needs_reentry: false,
    last_test_result: 0,
    last_test_model: 'gpt-x',
    last_test_duration_ms: 120,
    last_tested_at: '2026-09-01T00:00:00Z',
    last_test_targets: null,
  }
}

function providerFixture(keys: ProviderKey[] = [keyFixture()]): Provider {
  return {
    id: PROVIDER_ID,
    name: 'demo-provider',
    provider_type: 'openai',
    base_url: 'https://api.example.com',
    note: '',
    management_status: CANDIDATE_STATUS_ENABLED,
    running_status: 'available',
    keys,
    created_at: '2026-09-01T00:00:00Z',
    protocol_endpoints: '',
  }
}

// A settled mapping row (nothing the detail page would adopt for background
// polling — these tests stay deterministic in their fetch counts).
function candidateRow(overrides: Partial<ProviderCandidate> = {}): ProviderCandidate {
  return {
    candidate_id: 1,
    model_id: 100,
    model_name: 'model-one',
    provider_model_name: 'upstream-one',
    input_price: 1.5,
    output_price: 2.5,
    cache_write_price: null,
    cache_read_price: null,
    billing_mode: 'token',
    image_pricing_tiers: null,
    video_pricing_tiers: null,
    audio_unit_price: null,
    max_output: 4096,
    management_status: CANDIDATE_STATUS_ENABLED,
    verification_status: VERIFICATION_PASSED,
    last_test_result: 0,
    last_tested_at: '2026-09-01T00:00:00Z',
    last_test_error: null,
    queue_state: '',
    auto_enable_on_pass: false,
    ...overrides,
  }
}

// The row set the bucket assertions work against — one row per verification
// flavor, including the two UNTESTED kinds the unverified bucket must merge:
//
//   passed        verification PASSED
//   failed        verification FAILED (probed, decisive verdict)
//   inconclusive  verification UNTESTED but last_tested_at stamped — a probe
//                 DID run and produced no verdict
//   never-probed  verification UNTESTED, never tested at all
function bucketFixtureRows(): ProviderCandidate[] {
  return [
    candidateRow({ candidate_id: 11, model_name: 'model-passed', provider_model_name: 'up-passed' }),
    candidateRow({
      candidate_id: 12,
      model_name: 'model-failed',
      provider_model_name: 'up-failed',
      management_status: CANDIDATE_STATUS_DISABLED,
      verification_status: VERIFICATION_FAILED,
      last_test_result: 7,
      last_test_error: 'upstream 500',
    }),
    candidateRow({
      candidate_id: 13,
      model_name: 'model-inconclusive',
      provider_model_name: 'up-incon',
      management_status: CANDIDATE_STATUS_DISABLED,
      verification_status: VERIFICATION_UNTESTED,
      last_test_result: 7,
      last_test_error: 'rate limited',
    }),
    candidateRow({
      candidate_id: 14,
      model_name: 'model-never-probed',
      provider_model_name: 'up-never',
      management_status: CANDIDATE_STATUS_DISABLED,
      verification_status: VERIFICATION_UNTESTED,
      last_test_result: null,
      last_tested_at: null,
    }),
  ]
}

async function mountPage() {
  getProviderMock.mockResolvedValue(providerFixture())
  const i18n = createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { 'zh-CN': zhCN } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/providers', component: { template: '<div />' } },
      { path: '/providers/:id', component: { template: '<div />' } },
    ],
  })
  await router.push(`/providers/${PROVIDER_ID}`)
  await router.isReady()
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(zhCN.providers.tabKeys))
  await nextTick()
  // The models tab is NOT the landing tab; click its header like a user does.
  const tab = [...document.body.querySelectorAll('.n-tabs-tab')].find((el) =>
    (el.textContent ?? '').includes(zhCN.providers.tabModels),
  )
  expect(tab, `the models tab header (${zhCN.providers.tabModels}) rendered`).toBeTruthy()
  tab!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await nextTick()
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

// The mapping table's search input, located by its composed placeholder so
// the assertion doubles as a check on the placeholder terms (TC-06/TC-15).
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
  const expected = `${zhCN.models.name} / ${zhCN.models.providerModelName}`
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

// The mapping table's verification-result select.
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
  stubMatchMedia(false)
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('ProviderDetailPage mapping search + verification filter (TC-06/07/15/16/17)', () => {
  it('TC-06: substring search over model name and provider model name, case-insensitive, over the full list', async () => {
    listCandidatesMock.mockResolvedValue({ list: bucketFixtureRows() })
    await mountPage()

    // By model name.
    await typeKeyword('model-failed')
    expectRows(['model-failed'], ['model-passed', 'model-inconclusive', 'model-never-probed'])

    // By provider model name.
    await typeKeyword('up-incon')
    expectRows(['model-inconclusive'], ['model-passed', 'model-failed', 'model-never-probed'])

    // Case-insensitive substring, and clearing restores the full list.
    await typeKeyword('MODEL-FAILED')
    expectRows(['model-failed'], ['model-passed', 'model-inconclusive', 'model-never-probed'])
    await typeKeyword('')
    expectRows(['model-passed', 'model-failed', 'model-inconclusive', 'model-never-probed'], [])

    // The placeholder composes the two searchable columns' existing title
    // terms — the providerModelName term in particular is reused, not
    // re-coined (TC-15's no-new-vocabulary rule).
    const placeholder = searchField().props('placeholder') as string
    expect(placeholder).toBe(`${zhCN.models.name} / ${zhCN.models.providerModelName}`)
    expect(placeholder).toContain(zhCN.models.providerModelName)
  })

  it('TC-07: buckets map onto the raw three-value verification_status; unverified merges the inconclusive and never-probed rows', async () => {
    listCandidatesMock.mockResolvedValue({ list: bucketFixtureRows() })
    await mountPage()

    await pickBucket('passed')
    expectRows(['model-passed'], ['model-failed', 'model-inconclusive', 'model-never-probed'])

    await pickBucket('failed')
    expectRows(['model-failed'], ['model-passed', 'model-inconclusive', 'model-never-probed'])

    // The unverified bucket holds BOTH UNTESTED flavors: the row a probe ran
    // on without a verdict, and the row no probe was ever scheduled for.
    await pickBucket('unverified')
    expectRows(['model-inconclusive', 'model-never-probed'], ['model-passed', 'model-failed'])

    // Back to "all": clearing the select restores the full list.
    await pickBucket(null)
    expectRows(['model-passed', 'model-failed', 'model-inconclusive', 'model-never-probed'], [])
  })

  it('TC-15: new copy is bilingual, the bucket wording avoids "untested", and no locale coins an upstream-model-name term', async () => {
    listCandidatesMock.mockResolvedValue({ list: bucketFixtureRows() })
    await mountPage()

    // The select's options: the existing passed/failed terms plus the new
    // unverified term, in passed/failed/unverified order with stable bucket values.
    const options = filterSelect().props('options') as Array<{ label: string; value: string }>
    expect(options).toEqual([
      { label: zhCN.providers.verificationPassed, value: 'passed' },
      { label: zhCN.providers.verificationFailed, value: 'failed' },
      { label: zhCN.providers.verificationUnverified, value: 'unverified' },
    ])
    // The filter's own terms come from the locales on BOTH sides — the
    // parity test enforces key parity; this pins the keys exist and the
    // unverified wording is deliberately not the "untested" one.
    expect(zhCN.providers.verificationUnverified).toBeTruthy()
    expect(en.providers.verificationUnverified).toBeTruthy()
    expect(zhCN.providers.verificationUnverified).not.toBe(zhCN.providers.verificationUntested)
    expect(en.providers.verificationUnverified).not.toBe(en.providers.verificationUntested)
    expect(zhCN.providers.verificationFilterAll).toBeTruthy()
    expect(en.providers.verificationFilterAll).toBeTruthy()
    expect(filterSelect().props('placeholder')).toBe(zhCN.providers.verificationFilterAll)
    // The label reuses the existing verification-result term.
    expect(filterSelect().props('label')).toBe(zhCN.models.probeResultTitle)

    // No message in either locale coins an "upstream model name" style term
    // for what the product calls the provider model name.
    const leafValues = (tree: unknown): string[] => {
      if (typeof tree === 'string') return [tree]
      if (tree !== null && typeof tree === 'object') return Object.values(tree).flatMap(leafValues)
      return []
    }
    for (const value of [...leafValues(zhCN), ...leafValues(en)]) {
      expect(value, 'no coined 上游模型名 term').not.toContain('上游模型名')
      expect(value, 'no coined "Upstream model name" term').not.toContain('Upstream model name')
    }
  })

  it('TC-16: search and bucket intersect; no match renders the existing empty state; clearing either restores', async () => {
    listCandidatesMock.mockResolvedValue({ list: bucketFixtureRows() })
    await mountPage()

    // Both conditions at once: the keyword alone matches every row, the
    // bucket alone one row — together only their intersection survives.
    await typeKeyword('model-')
    await pickBucket('passed')
    expectRows(['model-passed'], ['model-failed', 'model-inconclusive', 'model-never-probed'])

    // A combination nothing matches: the tab's existing empty copy shows in
    // the table's empty slot and no row renders.
    await typeKeyword('up-never')
    expectRows([], ['model-passed', 'model-failed', 'model-inconclusive', 'model-never-probed'])
    expect(bodyText()).toContain(zhCN.providers.modelsEmpty)

    // Clearing the BUCKET (keeping the keyword) restores that keyword's own
    // result — exactly the never-probed row.
    await pickBucket(null)
    expectRows(['model-never-probed'], ['model-passed', 'model-failed', 'model-inconclusive'])

    // Clearing the KEYWORD (no bucket active) restores everything.
    await typeKeyword('')
    expectRows(['model-passed', 'model-failed', 'model-inconclusive', 'model-never-probed'], [])
  })

  it('TC-17: the filter sees the whole list — after flipping to page 2, a keyword restarts at page 1', async () => {
    // 21 settled rows: bulk-01..bulk-20 fill page 1, tail-row is page 2's
    // only row (page size defaults to 20).
    const rows: ProviderCandidate[] = []
    for (let i = 1; i <= 20; i++) {
      rows.push(
        candidateRow({
          candidate_id: i,
          model_id: 100 + i,
          model_name: `bulk-${String(i).padStart(2, '0')}`,
          provider_model_name: `up-${String(i).padStart(2, '0')}`,
        }),
      )
    }
    rows.push(candidateRow({ candidate_id: 21, model_id: 121, model_name: 'tail-row', provider_model_name: 'up-tail' }))
    listCandidatesMock.mockResolvedValue({ list: rows })
    await mountPage()

    // Flip to page 2 like a user: only the tail row renders there.
    const page2 = [...document.body.querySelectorAll('.n-pagination .n-pagination-item')].find(
      (el) => (el.textContent ?? '').trim() === '2',
    )
    expect(page2, 'the pager offers a page 2').toBeTruthy()
    page2!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await nextTick()
    await nextTick()
    expectRows(['tail-row'], ['bulk-01', 'bulk-02'])

    // Typing a keyword matches rows that live on page 1 of the unfiltered
    // list — if the filter only saw the current page (or did not reset the
    // pager), nothing would render. With the reset, page 1's hits show.
    await typeKeyword('bulk-')
    expectRows(['bulk-01', 'bulk-02', 'bulk-20'], ['tail-row'])

    // Clearing the keyword restores the full list — still on page 1, so the
    // tail row stays one pager click away rather than lost past the end.
    await typeKeyword('')
    expectRows(['bulk-01', 'bulk-20'], ['tail-row'])
  })
})
