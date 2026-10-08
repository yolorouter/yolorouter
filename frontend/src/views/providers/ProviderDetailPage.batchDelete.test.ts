// @vitest-environment happy-dom
//
// The provider detail page's models-tab BATCH delete: the selection column,
// the toolbar button, the per-model-deduplicated last-routable guard, and the
// partial-failure summary. The concurrency limiter itself is a pure function
// covered separately (src/utils/runWithLimit.test.ts, TC-12).
//
// Covered acceptance cases:
//
//   TC-05 all-success batch: every selected row's DELETE fires, the rows
//        disappear, the selection resets, and no failure summary is shown
//        (the success toast is)
//   TC-09 partial failure (deleteCandidate rejects on one row): the failed
//        row survives in the table and is named in the summary, the
//        successful rows still go, and the batch is never aborted
//   TC-11 the batch guard: rows of one model sharing both of its routable
//        candidates trigger ONE getModel for that model (deduplicated), the
//        batch is subtracted before counting the remainder, the consequence
//        warning names that model, and untouched models are neither fetched
//        nor affected
//
// Fixtures ride the same API-module mocks as the sibling page tests here;
// assertions read the zh-CN copy through the locale module so a copy edit
// does not fake a regression.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import { getProvider, listModelsForProvider, type Provider, type ProviderKey } from '../../api/providers'
import {
  deleteCandidate,
  getModel,
  listProviderCandidates,
  suggestCandidatePrice,
  type Model,
  type ModelCandidate,
  type ProviderCandidate,
} from '../../api/models'

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return { ...actual, getProvider: vi.fn(), listModelsForProvider: vi.fn() }
})

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return {
    ...actual,
    listProviderCandidates: vi.fn(),
    getModel: vi.fn(),
    deleteCandidate: vi.fn(),
    suggestCandidatePrice: vi.fn(),
  }
})

const getProviderMock = vi.mocked(getProvider)
const catalogueMock = vi.mocked(listModelsForProvider)
const listCandidatesMock = vi.mocked(listProviderCandidates)
const getModelMock = vi.mocked(getModel)
const deleteMock = vi.mocked(deleteCandidate)
const suggestMock = vi.mocked(suggestCandidatePrice)

// Must be imported after the vi.mock factories are registered.
import ProviderDetailPage from './ProviderDetailPage.vue'
import zhCN from '../../locales/zh-CN'
import { CANDIDATE_STATUS_ENABLED, VERIFICATION_PASSED } from '../../api/candidateStatus'

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

// A controllable MediaQueryList (same stub shape as the sibling tests):
// desktop throughout this file — the selection column and toolbar button are
// desktop surfaces.
function stubMatchMedia() {
  const listeners = new Set<(e: { matches: boolean }) => void>()
  const mql = {
    matches: false,
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
    key_suffix: 'wxyz',
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

// A settled, non-queued mapping row (nothing the detail page would adopt for
// background polling — these tests stay deterministic in their fetch counts).
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

// Each list response the page will pull, in order; the last one repeats for
// any further fetches.
function primeList(...lists: ProviderCandidate[][]) {
  for (const list of lists) listCandidatesMock.mockResolvedValueOnce({ list })
  if (lists.length > 0) listCandidatesMock.mockResolvedValue({ list: lists[lists.length - 1] })
}

// A model-detail candidate for the getModel fixture (the guard's routable
// data source); routable defaults to true like the sibling tests' fixture.
function modelCandidate(overrides: Partial<ModelCandidate> = {}): ModelCandidate {
  return {
    id: 1,
    provider_id: PROVIDER_ID,
    provider_name: 'demo-provider',
    provider_model_name: 'upstream-one',
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
    management_status: CANDIDATE_STATUS_ENABLED,
    sort_order: 1,
    verification_status: VERIFICATION_PASSED,
    routable: true,
    blocked_by: '',
    last_test_result: 0,
    last_test_duration_ms: 120,
    last_tested_at: '2026-09-01T00:00:00Z',
    binding_count: 0,
    ...overrides,
  }
}

function modelDetail(candidates: ModelCandidate[], id = 100, name = 'model-one'): Model {
  return {
    id,
    name,
    management_status: 1,
    running_status: 'available',
    scheduling_mode: 'failover',
    supports_image_input: null,
    output_modalities: ['text'],
    candidates,
    created_at: '2026-09-01T00:00:00Z',
  }
}

// The plain-guard getModel fixture: every batch row's own candidate (routable)
// PLUS an untouched routable sibling (id 999), so no model in the batch loses
// its last routable candidate.
function plainGuardOver(rows: ProviderCandidate[]) {
  getModelMock.mockImplementation(async (id: number) => {
    const own = rows.filter((r) => r.model_id === id)
    expect(own.length, `getModel(${id}) asked about a model in the fixture`).toBeGreaterThan(0)
    return modelDetail(
      [...own.map((r) => modelCandidate({ id: r.candidate_id })), modelCandidate({ id: 999 })],
      id,
      own[0].model_name,
    )
  })
}

async function mountPage() {
  getProviderMock.mockResolvedValue(providerFixture())
  catalogueMock.mockResolvedValue({ models: [], outcome: null, detail: '' })
  suggestMock.mockResolvedValue({
    input_price: 1.5,
    output_price: 2.5,
    cache_write_price: null,
    cache_read_price: null,
    audio_unit_price: null,
    source: 'history',
    catalog_updated_at: '',
  })
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

// The body row rendering a mapping, located by its model name (unique in
// every fixture here).
function rowTr(modelName: string): HTMLTableRowElement {
  const tr = [...document.body.querySelectorAll('tr')].find((el) => (el.textContent ?? '').includes(modelName))
  expect(tr, `row for ${modelName} rendered`).toBeTruthy()
  return tr!
}

// Ticks the row's selection checkbox the way a user does. naive-ui binds its
// click handler on the .n-checkbox element itself, so a bubbling click there
// toggles the row's checked key. The locator is any substring unique to the
// row — TC-11's two rows share a model name, so they are told apart by their
// upstream names.
async function checkRow(rowLocator: string) {
  const box = rowTr(rowLocator).querySelector('.n-checkbox')
  expect(box, `the selection checkbox on ${rowLocator}'s row rendered`).toBeTruthy()
  box!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await nextTick()
}

// The toolbar batch-delete button, found by its visible label — with or
// without the selection-count suffix, and the only button carrying that copy
// (the confirm dialog's positive button carries the plain common.delete copy).
function batchButton(): HTMLButtonElement {
  const btn = [...document.body.querySelectorAll('button')].find((b) =>
    (b.textContent ?? '').includes(zhCN.providers.batchDeleteButton),
  )
  expect(btn, 'the batch-delete toolbar button rendered').toBeTruthy()
  return btn!
}

// The most recently opened naive dialog, and its button by visible label.
function dialogButton(label: string): HTMLButtonElement {
  const dialogs = [...document.body.querySelectorAll('.n-dialog')]
  const dialog = dialogs[dialogs.length - 1]
  expect(dialog, 'a dialog is open').toBeTruthy()
  const btn = [...dialog!.querySelectorAll('button')].find((b) => (b.textContent ?? '').trim() === label)
  expect(btn, `dialog button "${label}" rendered`).toBeTruthy()
  return btn!
}

function bodyText(): string {
  return document.body.textContent ?? ''
}

// Pins a toast's TIER, not just its copy (same discipline as the model page
// tests): a batch that wrongly toasts success on failure would keep the text
// assertions green if the copies ever drift together.
function messageOfTier(tier: 'success' | 'warning', text: string) {
  const el = [...document.body.querySelectorAll(`.n-message--${tier}-type`)].find((e) =>
    (e.textContent ?? '').includes(text),
  )
  expect(el, `a ${tier} toast saying "${text}"`).toBeTruthy()
}

function noSuccessToast() {
  expect(document.body.querySelector('.n-message--success-type'), 'no success toast').toBeNull()
}

// The table no longer renders a row for the model (row-scoped on purpose: a
// closed dialog's copy can linger in the teleported DOM under happy-dom).
function rowGone(modelName: string): boolean {
  return ![...document.body.querySelectorAll('tr')].some((el) => (el.textContent ?? '').includes(modelName))
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

describe('ProviderDetailPage batch delete (TC-05, TC-09, TC-11)', () => {
  it('TC-05: an all-success batch deletes every selected row, resets the selection, and shows no failure summary', async () => {
    const rows = [
      candidateRow({ candidate_id: 11, model_id: 100, model_name: 'model-a', provider_model_name: 'upstream-a' }),
      candidateRow({ candidate_id: 12, model_id: 101, model_name: 'model-b', provider_model_name: 'upstream-b' }),
      candidateRow({ candidate_id: 13, model_id: 102, model_name: 'model-c', provider_model_name: 'upstream-c' }),
    ]
    primeList(rows, [])
    await mountPage()
    plainGuardOver(rows)
    deleteMock.mockResolvedValue(undefined)

    // Nothing selected: the button is inert and carries no count.
    expect(batchButton().disabled, 'the batch button starts disabled').toBe(true)

    await checkRow('model-a')
    await checkRow('model-b')
    await checkRow('model-c')
    const armed = batchButton()
    expect(armed.disabled, 'three checked rows arm the batch button').toBe(false)
    expect((armed.textContent ?? '').trim()).toBe(
      zhCN.providers.batchDeleteButtonWithCount.replace('{count}', '3'),
    )

    // The confirm asks about the whole selection, plainly (no model loses its
    // last routable candidate here).
    armed.click()
    const plainCopy = zhCN.providers.confirmBatchDeleteContent.replace('{count}', '3')
    await vi.waitFor(() => expect(bodyText()).toContain(plainCopy))
    dialogButton(zhCN.common.delete).click()

    // One DELETE per selected row, each with its row's ids.
    await vi.waitFor(() => expect(deleteMock).toHaveBeenCalledTimes(3))
    expect(deleteMock).toHaveBeenCalledWith(100, 11)
    expect(deleteMock).toHaveBeenCalledWith(101, 12)
    expect(deleteMock).toHaveBeenCalledWith(102, 13)

    // The refreshed table is empty, the selection is reset, and the success
    // toast — not a failure summary — reports the batch.
    await vi.waitFor(() => expect(rowGone('model-a')).toBe(true))
    expect(rowGone('model-b')).toBe(true)
    expect(rowGone('model-c')).toBe(true)
    await vi.waitFor(() =>
      expect(bodyText()).toContain(zhCN.providers.batchDeleteSucceeded.replace('{count}', '3')),
    )
    messageOfTier('success', zhCN.providers.batchDeleteSucceeded.replace('{count}', '3'))
    expect(bodyText(), 'no failure summary after a clean batch').not.toContain(
      zhCN.providers.batchDeleteFailedSummary.slice(0, 6),
    )
    expect(bodyText(), 'no failed-rows line after a clean batch').not.toContain(
      zhCN.providers.batchDeleteFailedRows.slice(0, 4),
    )
    const reset = batchButton()
    expect(reset.disabled, 'the selection resets after the batch').toBe(true)
    expect((reset.textContent ?? '').trim()).toBe(zhCN.providers.batchDeleteButton)
  })

  it('TC-09: a rejected delete keeps its row, lands in the summary, and never aborts the batch', async () => {
    const a = candidateRow({ candidate_id: 11, model_id: 100, model_name: 'model-a', provider_model_name: 'upstream-a' })
    const b = candidateRow({ candidate_id: 12, model_id: 101, model_name: 'model-b', provider_model_name: 'upstream-b' })
    const c = candidateRow({ candidate_id: 13, model_id: 102, model_name: 'model-c', provider_model_name: 'upstream-c' })
    // After the batch the server still returns the FAILED row alone.
    primeList([a, b, c], [b])
    await mountPage()
    plainGuardOver([a, b, c])
    deleteMock.mockImplementation((_modelId: number, candidateId: number) =>
      candidateId === 12 ? Promise.reject(new Error('boom')) : Promise.resolve(undefined),
    )

    await checkRow('model-a')
    await checkRow('model-b')
    await checkRow('model-c')
    batchButton().click()
    await vi.waitFor(() =>
      expect(bodyText()).toContain(zhCN.providers.confirmBatchDeleteContent.replace('{count}', '3')),
    )
    dialogButton(zhCN.common.delete).click()

    // The failure interrupted nothing: all three rows got their DELETE.
    await vi.waitFor(() => expect(deleteMock).toHaveBeenCalledTimes(3))

    // The failed row survives in the table; the two successful rows are gone.
    await vi.waitFor(() => expect(rowGone('model-a')).toBe(true))
    expect(rowGone('model-c')).toBe(true)
    expect(rowTr('model-b'), 'the failed row stays in the table').toBeTruthy()

    // The summary counts both sides and names the failed row; no success toast.
    await vi.waitFor(() =>
      expect(bodyText()).toContain(
        zhCN.providers.batchDeleteFailedSummary.replace('{passed}', '2').replace('{failed}', '1'),
      ),
    )
    expect(bodyText(), 'the failed row is named in the summary').toContain(
      zhCN.providers.batchDeleteFailedRows.replace('{rows}', 'model-b (upstream-b)'),
    )
    expect(bodyText()).not.toContain(zhCN.providers.batchDeleteSucceeded.replace('{count}', '3'))
    noSuccessToast()
    expect(batchButton().disabled, 'the selection resets after a partial batch too').toBe(true)
  })

  it('TC-11: the batch guard dedupes getModel per model, subtracts the batch, and leaves untouched models alone', async () => {
    // model-guarded owns exactly two routable candidates — BOTH in the batch.
    const guarded1 = candidateRow({
      candidate_id: 21,
      model_id: 200,
      model_name: 'model-guarded',
      provider_model_name: 'upstream-g1',
    })
    const guarded2 = candidateRow({
      candidate_id: 22,
      model_id: 200,
      model_name: 'model-guarded',
      provider_model_name: 'upstream-g2',
    })
    // model-plain's row is in the batch too but keeps another routable sibling.
    const plain = candidateRow({ candidate_id: 31, model_id: 201, model_name: 'model-plain' })
    // model-unrelated is not selected at all and must come through untouched.
    const unrelated = candidateRow({ candidate_id: 41, model_id: 202, model_name: 'model-unrelated' })
    primeList([guarded1, guarded2, plain, unrelated], [unrelated])
    await mountPage()
    getModelMock.mockImplementation(async (id: number) => {
      if (id === 200) return modelDetail([modelCandidate({ id: 21 }), modelCandidate({ id: 22 })], 200, 'model-guarded')
      if (id === 201)
        return modelDetail([modelCandidate({ id: 31 }), modelCandidate({ id: 999 })], 201, 'model-plain')
      // Fetching a model with no row in the batch is a bug, not a fixture gap.
      throw new Error(`unexpected getModel(${id})`)
    })
    deleteMock.mockResolvedValue(undefined)

    await checkRow('upstream-g1')
    // The second row of the SAME model: the prefetch must still talk to that
    // model once, not twice.
    await checkRow('upstream-g2')
    await checkRow('model-plain')
    batchButton().click()

    // Two affected models → exactly two getModel calls, none for model 202.
    await vi.waitFor(() => expect(getModelMock).toHaveBeenCalledTimes(2))
    expect(getModelMock).toHaveBeenCalledWith(200)
    expect(getModelMock).toHaveBeenCalledWith(201)
    expect(getModelMock).not.toHaveBeenCalledWith(202)
    expect(getModelMock.mock.calls.filter((call) => call[0] === 200)).toHaveLength(1)

    // The confirm escalates for model-guarded (the batch takes its last
    // routable candidates) and names ONLY that model.
    const warnCopy = zhCN.providers.confirmBatchDeleteLastRoutableContent
      .replace('{count}', '3')
      .replace('{models}', 'model-guarded')
    await vi.waitFor(() => expect(bodyText()).toContain(warnCopy))
    expect(bodyText(), 'the plain copy does not appear').not.toContain(
      zhCN.providers.confirmBatchDeleteContent.replace('{count}', '3'),
    )

    // Confirming still deletes the batch, and the untouched model's row
    // survives it.
    dialogButton(zhCN.common.delete).click()
    await vi.waitFor(() => expect(deleteMock).toHaveBeenCalledTimes(3))
    expect(deleteMock).toHaveBeenCalledWith(200, 21)
    expect(deleteMock).toHaveBeenCalledWith(200, 22)
    expect(deleteMock).toHaveBeenCalledWith(201, 31)
    await vi.waitFor(() => expect(rowGone('model-guarded')).toBe(true))
    expect(rowGone('model-plain')).toBe(true)
    expect(rowTr('model-unrelated'), 'the untouched model keeps its row').toBeTruthy()
  })
})
