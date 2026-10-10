// @vitest-environment happy-dom
//
// The model detail page's first mounted-DOM test suite — the regression
// baseline for the provider-page inline-actions batch. This batch makes ZERO
// changes to this page; every assertion below pins EXISTING behavior so the
// later tickets' "the model page did not regress" claim is a runnable check,
// not a declaration (TC-13):
//
//   - route-chain rendering: the candidates from the detail endpoint appear
//     as table rows
//   - edit modal backfill: opening a row's edit action fills the reused
//     candidate modal with that row's values (baseline for the edit modal
//     the provider page is about to reuse)
//   - delete guard copy: the confirm dialog escalates for the first
//     candidate vs. naming the remaining routable count for the rest
//   - reorder: the up/down buttons call the order endpoint with the row and
//     direction, and the boundary buttons stay disabled
//   - candidate status toggle: the management switch calls the status
//     endpoint with the toggled value
//   - binding-count column visibility: shown only under balanced scheduling
//     with >=2 view-routable candidates
//   - retest feedback: per-row retest announces THIS run's outcome (pass
//     toast / named failure toast) or the superseded notice
//
// Fixtures ride the same API-module mocks as every other page test here; the
// assertions read the zh-CN copy through the locale module so a copy edit
// does not fake a regression, while a wiring change (wrong row, wrong count,
// missing column) still fails.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import {
  deleteCandidate,
  getModel,
  getModelImpact,
  reorderCandidate,
  retestCandidate,
  setCandidateStatus,
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
    reorderCandidate: vi.fn(),
    setCandidateStatus: vi.fn(),
    retestCandidate: vi.fn(),
    deleteCandidate: vi.fn(),
  }
})

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return {
    ...actual,
    // The edit modal reloads the provider's model catalogue when it opens on
    // a candidate (watch on form.providerId); silent-failure is the
    // component's own contract, so resolving an empty catalogue keeps the
    // open path deterministic.
    listModelsForProvider: vi.fn(),
  }
})

const getModelMock = vi.mocked(getModel)
const getImpactMock = vi.mocked(getModelImpact)
const reorderMock = vi.mocked(reorderCandidate)
const setStatusMock = vi.mocked(setCandidateStatus)
const retestMock = vi.mocked(retestCandidate)
const deleteMock = vi.mocked(deleteCandidate)
const catalogueMock = vi.mocked(listModelsForProvider)

// Must be imported after the vi.mock factories are registered.
import ModelDetailPage from './ModelDetailPage.vue'
import ResponsiveDropdown from '../../components/common/ResponsiveDropdown.vue'
import zhCN from '../../locales/zh-CN'
import { VERIFICATION_PASSED, VERIFICATION_UNTESTED } from '../../api/candidateStatus'

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

// Desktop viewport (same stub shape as the other page tests): useIsMobile
// needs matchMedia, and the desktop branches are the ones asserted here.
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

// The three-row route chain every baseline assertion works against:
//
//   row 1 (id 1)  alpha/alpha-model — first candidate, passed, routable
//   row 2 (id 2)  beta/beta-model   — passed, routable (delete guard: the
//                                      remaining-routable count arm)
//   row 3 (id 3)  gamma/gamma-model — unverified, blocked, disabled
function modelFixture(overrides: Partial<Model> = {}, candidateOverrides: Partial<ModelCandidate>[] = []): Model {
  const candidates = [
    candidateFixture(),
    candidateFixture({
      id: 2,
      provider_id: 11,
      provider_name: 'beta',
      provider_model_name: 'beta-model',
      input_price: 3,
      output_price: 4,
      cache_read_price: null,
      supports_streaming: null,
      supports_function_calling: true,
      sort_order: 2,
      binding_count: 2,
    }),
    candidateFixture({
      id: 3,
      provider_id: 12,
      provider_name: 'gamma',
      provider_model_name: 'gamma-model',
      management_status: 0,
      sort_order: 3,
      verification_status: VERIFICATION_UNTESTED,
      routable: false,
      blocked_by: 'not_verified',
      binding_count: 0,
    }),
  ]
  candidateOverrides.forEach((o, i) => Object.assign(candidates[i], o))
  return {
    id: MODEL_ID,
    name: 'gpt-demo',
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
    ...overrides,
  }
}

function primeModel(model: Model) {
  getModelMock.mockResolvedValue(model)
  getImpactMock.mockResolvedValue({
    allowlisted_keys: [],
    allow_all_key_count: 0,
    candidate_count: model.candidates.length,
    recent_request_count: 0,
    recent_window_days: 30,
  })
  catalogueMock.mockResolvedValue({ models: [], outcome: null, detail: '' })
}

async function mountPage(model: Model) {
  primeModel(model)
  const i18n = createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { 'zh-CN': zhCN } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      // The delete flow leaves for the list; resolving keeps router warnings
      // out of the output.
      { path: '/models', component: { template: '<div />' } },
      { path: '/models/:id', component: { template: '<div />' } },
    ],
  })
  await router.push(`/models/${MODEL_ID}`)
  await router.isReady()
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  // Wait for the RENDERED page, not just the fetch: the detail fetch
  // resolving and the (large) route-chain table flushing take a few ticks
  // past the API call, and every assertion below reads mounted rows.
  await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(zhCN.models.tabRoute))
  await nextTick()
}

// The body row rendering a candidate, located by its provider model name
// (unique in every fixture here).
function rowTr(providerModelName: string): HTMLTableRowElement {
  const tr = [...document.body.querySelectorAll('tr')].find((el) => (el.textContent ?? '').includes(providerModelName))
  expect(tr, `row for ${providerModelName} rendered`).toBeTruthy()
  return tr!
}

// Each row's actions cell renders one ResponsiveDropdown ('⋯', desktop);
// findAllComponents returns them in document order, i.e. row order. Driving
// the component's own `select` event is the established seam for dropdown
// menus in this suite (real menu clicks are covered by the batch's own new
// tests, not this baseline).
function pickRowAction(rowIndex: number, key: string) {
  const dropdowns = wrapper!.findAllComponents(ResponsiveDropdown)
  expect(dropdowns.length, 'one actions dropdown per candidate row').toBeGreaterThanOrEqual(rowIndex + 1)
  dropdowns[rowIndex].vm.$emit('select', key)
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

// The retest feedback is two-TIER on purpose: a pass lands as a green toast,
// a named failure as a yellow one, a superseded run as a plain notice. The
// tier is the behavior, so assert the toast's own type element — not just
// that the text appeared somewhere.
function messageOfTier(tier: 'success' | 'warning' | 'info', text: string) {
  const el = [...document.body.querySelectorAll(`.n-message--${tier}-type`)].find((e) => (e.textContent ?? '').includes(text))
  expect(el, `a ${tier} toast saying "${text}"`).toBeTruthy()
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

describe('ModelDetailPage baseline (TC-13)', () => {
  it('renders the route chain from the detail endpoint', async () => {
    await mountPage(modelFixture())
    const text = document.body.textContent ?? ''
    expect(text).toContain('gpt-demo')
    expect(text).toContain('alpha-model')
    expect(text).toContain('beta-model')
    expect(text).toContain('gamma-model')
  })

  it('backfills the candidate edit modal with the row values when editing', async () => {
    await mountPage(modelFixture())
    // Row 2 (index 1): beta-model with input 3 / output 4.
    pickRowAction(1, 'edit')
    await nextTick()
    await nextTick()

    // The reused modal opened in edit shape with the row's data: the edit
    // title, the row's provider model name in the (tag) select, and the
    // row's prices in the number inputs. Scoped to the modal element — the
    // table behind it shows the same names, so body-wide assertions would
    // pass even with an empty form.
    const modals = [...document.body.querySelectorAll('.n-modal')]
    const modal = modals[modals.length - 1]
    expect(modal, 'the edit modal rendered').toBeTruthy()
    const modalText = modal!.textContent ?? ''
    expect(modalText).toContain(zhCN.models.editCandidate)
    expect(modalText).toContain('beta-model')
    expect(modalText, 'the outward model name is shown read-only').toContain('gpt-demo')
    const inputValues = [...modal!.querySelectorAll('input')].map((i) => i.value)
    expect(inputValues, 'input price backfilled').toContain('3')
    expect(inputValues, 'output price backfilled').toContain('4')
  })

  it('delete guard: the first candidate gets the first-choice warning copy, and confirming deletes', async () => {
    await mountPage(modelFixture())
    pickRowAction(0, 'delete')
    await nextTick()
    await nextTick()

    expect(document.body.textContent ?? '').toContain(zhCN.models.confirmDeleteFirstCandidateContent)
    expect(deleteMock, 'nothing is deleted before the confirm').not.toHaveBeenCalled()

    dialogButton(zhCN.models.save).click()
    await vi.waitFor(() => expect(deleteMock).toHaveBeenCalledWith(MODEL_ID, 1))
    // A successful delete reloads the detail.
    await vi.waitFor(() => expect(getModelMock).toHaveBeenCalledTimes(2))
  })

  it('delete guard: a later candidate is warned with the remaining routable count, and cancel deletes nothing', async () => {
    await mountPage(modelFixture())
    // Row 2: candidates 1 and 3 flank it, but only candidate 1 is routable,
    // so exactly ONE routable candidate would remain.
    pickRowAction(1, 'delete')
    await nextTick()
    await nextTick()

    const expected = zhCN.models.confirmDeleteCandidateContent.replace('{count}', '1')
    expect(document.body.textContent ?? '').toContain(expected)
    expect(deleteMock).not.toHaveBeenCalled()

    dialogButton(zhCN.models.cancel).click()
    await nextTick()
    await nextTick()
    expect(deleteMock, 'the cancel path never reaches the API').not.toHaveBeenCalled()
  })

  it('reorder: boundary buttons stay disabled and a move calls the order endpoint with row + direction', async () => {
    await mountPage(modelFixture())

    const firstRow = rowTr('alpha-model')
    const lastRow = rowTr('gamma-model')
    const upOf = (tr: HTMLTableRowElement) => tr.querySelector<HTMLButtonElement>(`button[title="${zhCN.models.moveUp}"]`)!
    const downOf = (tr: HTMLTableRowElement) => tr.querySelector<HTMLButtonElement>(`button[title="${zhCN.models.moveDown}"]`)!
    expect(upOf(firstRow).disabled, 'the first row cannot move up').toBe(true)
    expect(downOf(lastRow).disabled, 'the last row cannot move down').toBe(true)
    expect(upOf(lastRow).disabled).toBe(false)

    downOf(firstRow).click()
    await vi.waitFor(() => expect(reorderMock).toHaveBeenCalledWith(MODEL_ID, 1, 'down'))
    // The move reloads the detail.
    await vi.waitFor(() => expect(getModelMock).toHaveBeenCalledTimes(2))
  })

  it('candidate status toggle: the management switch calls the status endpoint with the toggled value', async () => {
    await mountPage(modelFixture())
    // Row 2's candidate is enabled, so a click switches it OFF.
    const sw = rowTr('beta-model').querySelector<HTMLElement>('.n-switch')
    expect(sw, 'management switch rendered').toBeTruthy()
    expect(sw!.getAttribute('aria-checked')).toBe('true')
    sw!.click()
    await vi.waitFor(() => expect(setStatusMock).toHaveBeenCalledWith(MODEL_ID, 2, false))
  })

  it('binding-count column: hidden under failover, shown under balanced with two routable candidates', async () => {
    await mountPage(modelFixture({ scheduling_mode: 'failover' }))
    expect(document.body.textContent ?? '').not.toContain(zhCN.models.bindingCountColumn)

    wrapper!.unmount()
    document.body.innerHTML = ''
    vi.clearAllMocks()
    await mountPage(modelFixture({ scheduling_mode: 'balanced' }))
    const text = document.body.textContent ?? ''
    expect(text).toContain(zhCN.models.bindingCountColumn)
    // The gauge renders the candidates' own counts (row 1 carries 4).
    expect(rowTr('alpha-model').textContent ?? '').toContain('4')

    // Balanced alone is not enough: with only one view-routable candidate
    // the relay binds nothing and the column hides again.
    wrapper!.unmount()
    document.body.innerHTML = ''
    vi.clearAllMocks()
    await mountPage(modelFixture({ scheduling_mode: 'balanced' }, [{}, { routable: false, blocked_by: 'not_verified' }]))
    expect(document.body.textContent ?? '').not.toContain(zhCN.models.bindingCountColumn)
  })

  it('retest feedback: each row announces its own run outcome, or the superseded notice', async () => {
    await mountPage(modelFixture())
    // One outcome per row, in click order: a pass, a lost commit race, and
    // a named failure (rate limited — backend TestOutcome 5).
    retestMock
      .mockResolvedValueOnce({ candidate: modelFixture().candidates[0], applied: true })
      .mockResolvedValueOnce({ candidate: modelFixture().candidates[1], applied: false })
      .mockResolvedValueOnce({
        candidate: { ...modelFixture().candidates[2], last_test_result: 5 },
        applied: true,
      })

    pickRowAction(0, 'retest')
    await vi.waitFor(() => expect(retestMock).toHaveBeenCalledWith(MODEL_ID, 1))
    await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(zhCN.models.testPassed))
    messageOfTier('success', zhCN.models.testPassed)

    pickRowAction(1, 'retest')
    await vi.waitFor(() => expect(retestMock).toHaveBeenCalledWith(MODEL_ID, 2))
    await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(zhCN.providers.retestSuperseded))
    messageOfTier('info', zhCN.providers.retestSuperseded)

    pickRowAction(2, 'retest')
    await vi.waitFor(() => expect(retestMock).toHaveBeenCalledWith(MODEL_ID, 3))
    const failureText = `${zhCN.models.testFailed}: ${zhCN.providers.outcomeRateLimited}`
    await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(failureText))
    messageOfTier('warning', failureText)
  })
})
