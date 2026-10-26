// @vitest-environment happy-dom
//
// The models list' desktop bulk-price flow (merged in from the standalone
// pricing page): model-level selection — a leading checkbox column whose
// header select-all covers the CURRENT FILTERED models (all pages of them;
// the set of model ids persists across paging, mobile renders none of it) —
// a count-labeled toolbar entry that refuses batches past the endpoint's
// 2000-candidate cap (the cap counts the DEDUPED UNION of the selected
// models' candidates), then the two-phase drawer: an operation chooser
// (scale factor or apply_suggested) whose preview calls the endpoint with
// dry_run true, and a per-candidate preview whose confirm re-calls it with
// dry_run false, reloads the list, clears the selection, and summarizes
// applied/skipped. A selection never outlives a page-initiated refetch (every list reload
// clears it), and the drawer refuses to open when the candidate union is
// empty. A dry-run response landing after a close must not
// resurrect preview state — even when a reopen already started a fresh
// session before the response landed. Payload assertions mirror the backend
// binding exactly (candidate_ids / operation / factor / dry_run), including
// apply_suggested never putting a factor key on the wire.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive, { NCheckbox, NInputNumber, NRadioGroup } from 'naive-ui'
import { defineComponent } from 'vue'

import {
  listModels,
  postBulkPrice,
  type BulkPriceItem,
  type BulkPriceResult,
  type BulkPriceSlots,
  type Model,
  type ModelCandidate,
} from '../../api/models'
import { listModelsForProvider, listProviders } from '../../api/providers'
import { getSystemEndpoint } from '../../api/system'

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return { ...actual, listModels: vi.fn(), postBulkPrice: vi.fn() }
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
const postBulkPriceMock = vi.mocked(postBulkPrice)
const listProvidersMock = vi.mocked(listProviders)
const catalogueMock = vi.mocked(listModelsForProvider)
const endpointMock = vi.mocked(getSystemEndpoint)

// Must be imported after the vi.mock factories are registered.
import ModelListPage from './ModelListPage.vue'
import ModalDrawer from '../../components/common/ModalDrawer.vue'
import ModelEditModal from '../../components/models/ModelEditModal.vue'
import zhCN from '../../locales/zh-CN'

// The BULK drawer component. The page hosts several ModalDrawers (the edit
// and fallback modals render their own), so match the bulk flow by its 640px
// width PLUS its two phase titles — the fallback modal shares the width.
function bulkDrawer() {
  const bulk = wrapper!.findAllComponents(ModalDrawer).find(
    (drawer) =>
      drawer.props('maxWidth') === '640px' &&
      [zhCN.models.bulkDialogTitle, zhCN.models.bulkPreviewTitle].includes(drawer.props('title') as string),
  )
  expect(bulk, 'the bulk drawer rendered').toBeTruthy()
  return bulk!
}

// The bulk drawer's bound open state. (happy-dom never finishes naive-ui's
// leave transition, so the teleported DOM outlives the close — the prop is
// the truthful "is it closed" signal here.)
function drawerShow(): unknown {
  return bulkDrawer().props('show')
}

const Host = defineComponent({
  components: { ModelListPage },
  template: '<NConfigProvider><NDialogProvider><NMessageProvider><ModelListPage /></NMessageProvider></NDialogProvider></NConfigProvider>',
})

let wrapper: VueWrapper | null = null

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

// Two models x two providers: priced token candidates (ids 1 and 3), an
// unpriced token candidate (id 2), and a priced audio candidate (id 4). The
// bulk payload must carry all four ids — the deduped union of the selected
// models' candidates — even though only two rows were ticked.
const BULK_MODELS = () => [
  modelFixture(1, 'alpha-model', [
    candidateFixture({ id: 1, provider_id: 1, provider_name: 'Provider One', provider_model_name: 'up-a' }),
    candidateFixture({ id: 2, provider_id: 2, provider_name: 'Provider Two', provider_model_name: 'up-b', prices_declared: false, input_price: 0, output_price: 0 }),
  ]),
  modelFixture(2, 'beta-model', [
    candidateFixture({ id: 3, provider_id: 1, provider_name: 'Provider One', provider_model_name: 'up-c', input_price: 2, output_price: 4 }),
    candidateFixture({ id: 4, provider_id: 2, provider_name: 'Provider Two', provider_model_name: 'up-d', billing_mode: 'audio', audio_unit_price: 0.8, input_price: 0, output_price: 0 }),
  ]),
]

// The select-all scope fixture: under the unpriced-only filter only
// has-unpriced (candidates 11 and 13) stays visible; fully-priced must not
// join a select-all batch.
const FILTER_MODELS = () => [
  modelFixture(9, 'fully-priced', [candidateFixture({ id: 31, provider_name: 'alpha', provider_model_name: 'up-a', input_price: 1, output_price: 2 })]),
  modelFixture(7, 'has-unpriced', [
    candidateFixture({ id: 11, provider_name: 'alpha', provider_model_name: 'up-a', input_price: 2, output_price: 3 }),
    candidateFixture({ id: 13, provider_name: 'gamma', provider_model_name: 'up-g', input_price: 0, output_price: 0, prices_declared: false, routable: false, sort_order: 2 }),
  ]),
]

async function mountPage(models: Model[], matches = false, expectedRows = models.length) {
  stubMatchMedia(matches)
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
  await vi.waitFor(() =>
    expect(matches ? wrapper!.findAll('.rdt-card').length : wrapper!.findAll('tbody tr').length).toBe(expectedRows),
  )
}

// The desktop toolbar entry (label carries the selected MODEL count once rows
// are ticked).
function bulkToolbarButton(): HTMLButtonElement | undefined {
  return [...document.body.querySelectorAll('button')].find((b) => (b.textContent ?? '').includes(zhCN.models.bulkButton))
}

function checkboxByClass(className: string): VueWrapper {
  const box = (wrapper!.findAllComponents(NCheckbox) as unknown as VueWrapper[]).find((c) => c.classes().includes(className))
  expect(box, `checkbox ${className} rendered`).toBeTruthy()
  return box!
}

async function setChecked(className: string, checked: boolean) {
  checkboxByClass(className).vm.$emit('update:checked', checked)
  await flushPromises()
}

// Buttons rendered inside the drawer (it teleports to document.body).
function bodyButton(label: string): HTMLButtonElement | undefined {
  return [...document.body.querySelectorAll('button')].find((b) => (b.textContent ?? '').trim() === label)
}

async function clickBodyButton(label: string) {
  const button = bodyButton(label)
  expect(button, `button "${label}" rendered`).toBeTruthy()
  button!.click()
  await flushPromises()
}

function factorInput(): VueWrapper {
  const input = (wrapper!.findAllComponents(NInputNumber) as unknown as VueWrapper[]).find((c) => c.classes().includes('bulk-factor'))
  expect(input, 'the factor input rendered').toBeTruthy()
  return input!
}

function factorInputIfAny(): VueWrapper | null {
  return ((wrapper!.findAllComponents(NInputNumber) as unknown as VueWrapper[]).find((c) => c.classes().includes('bulk-factor')) ?? null) as VueWrapper | null
}

async function setFactor(value: number | null) {
  factorInput().vm.$emit('update:value', value)
  await flushPromises()
}

function slots(values: Partial<BulkPriceSlots>): BulkPriceSlots {
  return { input_price: 0, output_price: 0, cache_write_price: null, cache_read_price: null, audio_unit_price: null, ...values }
}

function bulkItem(overrides: Partial<BulkPriceItem> & { candidate_id: number }): BulkPriceItem {
  return {
    model_name: 'alpha-model',
    provider_name: 'Provider One',
    before: slots({}),
    after: slots({}),
    changed: false,
    ...overrides,
  }
}

// A dry-run answer for the union ids 1 (priced token) and 4 (priced audio):
// one token change, one audio change — ids 2 and 3 are the dry run's own
// business, the preview shows what the endpoint returned.
const SCALE_DRY_RUN: BulkPriceResult = {
  items: [
    bulkItem({ candidate_id: 1, before: slots({ input_price: 1.5, output_price: 2.5 }), after: slots({ input_price: 1.65, output_price: 2.75 }), changed: true }),
    bulkItem({
      candidate_id: 4,
      model_name: 'beta-model',
      provider_name: 'Provider Two',
      before: slots({ audio_unit_price: 0.8 }),
      after: slots({ audio_unit_price: 0.88 }),
      changed: true,
    }),
  ],
  applied: 2,
  skipped: 0,
}

const APPLY_RESULT: BulkPriceResult = {
  items: SCALE_DRY_RUN.items,
  applied: 2,
  skipped: 0,
}

// Open the drawer, run the scale preview with the given factor.
async function openScalePreview(factor: number | null, dryRunResult: BulkPriceResult) {
  const toolbar = bulkToolbarButton()
  expect(toolbar, 'the bulk toolbar button rendered').toBeTruthy()
  toolbar!.click()
  await flushPromises()
  await vi.waitFor(() => expect(document.body.querySelector('.bulk-choose')).toBeTruthy())
  await setFactor(factor)
  postBulkPriceMock.mockResolvedValueOnce(dryRunResult)
  await clickBodyButton(zhCN.models.bulkPreviewButton)
  await vi.waitFor(() => expect(document.body.querySelector('.bulk-preview')).toBeTruthy())
}

beforeEach(() => {
  listModelsMock.mockResolvedValue({ list: BULK_MODELS() })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

// ---- Desktop selection layer ----

describe('ModelListPage bulk selection', () => {
  it('desktop renders a LEADING selection column and a disabled count-labeled bulk button', async () => {
    await mountPage(BULK_MODELS())
    const headCells = wrapper!.findAll('thead th')
    expect(headCells.length, 'select-all column + expand + 7 data columns').toBe(9)
    expect(headCells[0]!.find('.models-select-all').exists(), 'the select-all leads the header row').toBe(true)
    expect(wrapper!.findAllComponents(NCheckbox).length, 'select-all + 2 row checkboxes').toBe(3)
    const toolbar = bulkToolbarButton()
    expect(toolbar, 'the bulk toolbar button rendered').toBeTruthy()
    expect((toolbar!.textContent ?? '').trim()).toBe(zhCN.models.bulkButton)
    expect(toolbar!.disabled, 'disabled with no selection').toBe(true)
  })

  it('ticking two model rows accumulates into the toolbar count and enables the button', async () => {
    await mountPage(BULK_MODELS())
    await setChecked('models-row-check--1', true)
    await setChecked('models-row-check--2', true)
    const toolbar = bulkToolbarButton()
    expect((toolbar!.textContent ?? '').trim()).toBe(zhCN.models.bulkButtonWithCount.replace('{count}', '2'))
    expect(toolbar!.disabled, 'enabled with a selection').toBe(false)
  })

  it('select-all under the unpriced-only filter selects exactly the filtered models (payload proof)', async () => {
    await mountPage(FILTER_MODELS())
    const switchEl = wrapper!.find('.filter-panel .n-switch')
    expect(switchEl.exists(), 'the unpriced-only switch rendered').toBe(true)
    await switchEl.trigger('click')
    await vi.waitFor(() => expect(wrapper!.findAll('tbody tr').length).toBe(1))
    await setChecked('models-select-all', true)
    const toolbar = bulkToolbarButton()
    expect((toolbar!.textContent ?? '').trim()).toBe(zhCN.models.bulkButtonWithCount.replace('{count}', '1'))

    await openScalePreview(1, { items: [], applied: 0, skipped: 2 })
    expect(postBulkPriceMock).toHaveBeenCalledTimes(1)
    expect(postBulkPriceMock.mock.calls[0]![0]).toEqual({
      candidate_ids: [11, 13],
      operation: 'scale',
      factor: 1,
      dry_run: true,
    })
  })

  it('row selections persist across pagination pages and the header tri-state follows the whole filtered set', async () => {
    const many = Array.from({ length: 21 }, (_, i) =>
      modelFixture(i + 1, `model-${i + 1}`, [candidateFixture({ id: 100 + i + 1, provider_name: `p${i + 1}`, provider_model_name: `up-${i + 1}` })]),
    )
    await mountPage(many, false, 20)
    await setChecked('models-row-check--1', true)
    expect((bulkToolbarButton()!.textContent ?? '').trim()).toBe(zhCN.models.bulkButtonWithCount.replace('{count}', '1'))

    // To page 2 (model-21): the selection survives the page change.
    const page2 = [...document.body.querySelectorAll('.n-data-table .n-pagination-item')].find((el) => (el.textContent ?? '').trim() === '2')
    expect(page2, 'the page-2 pager item rendered').toBeTruthy()
    ;(page2 as HTMLElement).click()
    await flushPromises()
    await vi.waitFor(() => expect(wrapper!.findAll('tbody tr').length).toBe(1))
    expect((bulkToolbarButton()!.textContent ?? '').trim()).toBe(zhCN.models.bulkButtonWithCount.replace('{count}', '1'))

    // The header select-all over the WHOLE filtered set (both pages) shows
    // partial and completes every filtered model, page 1 included.
    const header = checkboxByClass('models-select-all')
    expect(header.classes(), 'one of 21 selected shows indeterminate').toContain('n-checkbox--indeterminate')
    await setChecked('models-select-all', true)
    expect((bulkToolbarButton()!.textContent ?? '').trim()).toBe(zhCN.models.bulkButtonWithCount.replace('{count}', '21'))

    // Back to page 1: the first row's checkbox still reflects the set.
    const page1 = [...document.body.querySelectorAll('.n-data-table .n-pagination-item')].find((el) => (el.textContent ?? '').trim() === '1')
    ;(page1 as HTMLElement).click()
    await flushPromises()
    await vi.waitFor(() => expect(wrapper!.findAll('tbody tr').length).toBe(20))
    expect(checkboxByClass('models-row-check--1').classes()).toContain('n-checkbox--checked')
  })

  it('a refetch clears the selection: the bulk batch never outlives the list', async () => {
    await mountPage(BULK_MODELS())
    await setChecked('models-row-check--1', true)
    await setChecked('models-row-check--2', true)
    expect((bulkToolbarButton()!.textContent ?? '').trim()).toBe(zhCN.models.bulkButtonWithCount.replace('{count}', '2'))

    // Any refetch path lands in the same choke-point; here it is driven via
    // the edit modal's saved event (onEdited). The fresh list may have
    // reordered or dropped the ticked models — ghost ids would inflate the
    // count or send an empty candidate union — so the selection is dropped
    // with it.
    wrapper!.findComponent(ModelEditModal).vm.$emit('updated')
    await vi.waitFor(() => expect(listModelsMock).toHaveBeenCalledTimes(2))
    await flushPromises()
    expect((bulkToolbarButton()!.textContent ?? '').trim()).toBe(zhCN.models.bulkButton)
    expect(bulkToolbarButton()!.disabled, 'disabled with no selection').toBe(true)
    expect(checkboxByClass('models-row-check--1').classes(), 'the ticked row unticked').not.toContain('n-checkbox--checked')
    expect(checkboxByClass('models-row-check--2').classes(), 'the other ticked row unticked').not.toContain('n-checkbox--checked')
  })

  it('a ticked zero-candidate model shows in the count but openBulk refuses: warning copy, drawer never opens', async () => {
    await mountPage([modelFixture(5, 'bare-model', [])])
    await setChecked('models-row-check--5', true)
    const toolbar = bulkToolbarButton()
    expect((toolbar!.textContent ?? '').trim()).toBe(zhCN.models.bulkButtonWithCount.replace('{count}', '1'))
    expect(toolbar!.disabled, 'the button counts models, not candidates').toBe(false)
    toolbar!.click()
    await flushPromises()
    expect(drawerShow(), 'the drawer stays closed on an empty union').toBe(false)
    expect(document.body.querySelector('.bulk-choose')).toBeNull()
    expect(document.body.textContent).toContain(zhCN.models.bulkNothingSelected)
    expect(postBulkPriceMock, 'nothing was sent').not.toHaveBeenCalled()
  })

  it('mobile renders no checkboxes and no bulk toolbar', async () => {
    await mountPage(BULK_MODELS(), true)
    // naive-ui's checkbox renders as a div.n-checkbox (role=checkbox) —
    // there is no native input to look for.
    expect(document.body.querySelectorAll('.n-checkbox').length, 'no checkboxes on mobile').toBe(0)
    expect(bulkToolbarButton(), 'no bulk toolbar on mobile').toBeUndefined()
  })
})

// ---- The chooser -> preview -> apply state machine ----

describe('ModelListPage bulk flow', () => {
  it('scale: dry_run then apply payloads carry the deduped candidate union; success reloads, clears, and summarizes', async () => {
    await mountPage(BULK_MODELS())
    await setChecked('models-row-check--1', true)
    await setChecked('models-row-check--2', true)
    await openScalePreview(1.1, SCALE_DRY_RUN)

    expect(postBulkPriceMock).toHaveBeenCalledTimes(1)
    const previewPayload = postBulkPriceMock.mock.calls[0]![0]
    expect(previewPayload.candidate_ids, 'ALL candidates of both models, deduped').toEqual([1, 2, 3, 4])
    expect(previewPayload).toEqual({ candidate_ids: [1, 2, 3, 4], operation: 'scale', factor: 1.1, dry_run: true })

    postBulkPriceMock.mockResolvedValueOnce(APPLY_RESULT)
    await clickBodyButton(zhCN.models.bulkConfirmApply)
    await vi.waitFor(() => expect(postBulkPriceMock).toHaveBeenCalledTimes(2))
    const applyPayload = postBulkPriceMock.mock.calls[1]![0]
    expect(applyPayload).toEqual({ candidate_ids: [1, 2, 3, 4], operation: 'scale', factor: 1.1, dry_run: false })

    // Success: the list refetches, the drawer closes, the selection clears,
    // and the summary names both counts.
    await vi.waitFor(() => expect(listModelsMock).toHaveBeenCalledTimes(2))
    await vi.waitFor(() => expect(drawerShow()).toBe(false))
    await vi.waitFor(() =>
      expect(document.body.textContent).toContain(zhCN.models.bulkAppliedSummary.replace('{applied}', '2').replace('{skipped}', '0')),
    )
    await vi.waitFor(() => expect((bulkToolbarButton()!.textContent ?? '').trim()).toBe(zhCN.models.bulkButton))
  })

  it('apply_suggested: the payloads omit factor entirely and the factor input disappears', async () => {
    await mountPage(BULK_MODELS())
    await setChecked('models-row-check--1', true)
    const toolbar = bulkToolbarButton()!
    toolbar.click()
    await flushPromises()
    await vi.waitFor(() => expect(document.body.querySelector('.bulk-choose')).toBeTruthy())

    ;(wrapper!.findComponent(NRadioGroup) as VueWrapper).vm.$emit('update:value', 'apply_suggested')
    await flushPromises()
    expect(factorInputIfAny(), 'no factor input for apply_suggested').toBeNull()

    postBulkPriceMock.mockResolvedValueOnce({ items: [], applied: 1, skipped: 0 })
    await clickBodyButton(zhCN.models.bulkPreviewButton)
    await vi.waitFor(() => expect(document.body.querySelector('.bulk-preview')).toBeTruthy())
    expect(postBulkPriceMock.mock.calls[0]![0]).toEqual({ candidate_ids: [1, 2], operation: 'apply_suggested', dry_run: true })
    expect(Object.keys(postBulkPriceMock.mock.calls[0]![0]), 'no factor key').not.toContain('factor')

    postBulkPriceMock.mockResolvedValueOnce({ items: [], applied: 1, skipped: 0 })
    await clickBodyButton(zhCN.models.bulkConfirmApply)
    await vi.waitFor(() => expect(postBulkPriceMock).toHaveBeenCalledTimes(2))
    expect(postBulkPriceMock.mock.calls[1]![0]).toEqual({ candidate_ids: [1, 2], operation: 'apply_suggested', dry_run: false })
    expect(Object.keys(postBulkPriceMock.mock.calls[1]![0]), 'no factor key').not.toContain('factor')
  })

  it('a zero or negative factor is rejected client-side and never reaches the endpoint', async () => {
    await mountPage(BULK_MODELS())
    await setChecked('models-row-check--1', true)
    const toolbar = bulkToolbarButton()!
    toolbar.click()
    await flushPromises()
    await vi.waitFor(() => expect(document.body.querySelector('.bulk-choose')).toBeTruthy())

    for (const bad of [0, -1]) {
      await setFactor(bad)
      const previewButton = bodyButton(zhCN.models.bulkPreviewButton)
      expect(previewButton!.disabled, `factor ${bad} disables the preview`).toBe(true)
      expect(document.body.textContent).toContain(zhCN.models.bulkFactorInvalid)
    }
    // A cleared input blocks the flow quietly: no error nag, still no way through.
    await setFactor(null)
    expect(bodyButton(zhCN.models.bulkPreviewButton)!.disabled).toBe(true)
    expect(postBulkPriceMock, 'nothing was sent').not.toHaveBeenCalled()
  })

  it('the preview renders changed candidates normally and skipped ones greyed with their reason', async () => {
    await mountPage(BULK_MODELS())
    await setChecked('models-row-check--1', true)
    await openScalePreview(1.1, {
      items: [
        bulkItem({ candidate_id: 1, before: slots({ input_price: 1.5, output_price: 2.5 }), after: slots({ input_price: 1.65, output_price: 2.75 }), changed: true }),
        bulkItem({ candidate_id: 2, provider_name: 'Provider Two', changed: false, skip_reason: 'unpriced' }),
      ],
      applied: 1,
      skipped: 1,
    })
    const items = [...document.body.querySelectorAll('.bulk-preview__item')]
    expect(items).toHaveLength(2)
    expect(items[0]!.className, 'the changed row is not greyed').not.toContain('bulk-preview__item--skipped')
    expect(items[0]!.textContent).toContain('1.5 → 1.65')
    expect(items[0]!.textContent).toContain(zhCN.models.bulkChangedTag)
    expect(items[1]!.className, 'the skipped row is greyed').toContain('bulk-preview__item--skipped')
    expect(items[1]!.textContent).toContain(zhCN.models.bulkSkippedTag)
    expect(items[1]!.textContent).toContain(zhCN.models.bulkSkipUnpriced)
  })

  it('closing the drawer after a preview never applies', async () => {
    await mountPage(BULK_MODELS())
    await setChecked('models-row-check--1', true)
    await openScalePreview(1.1, SCALE_DRY_RUN)
    await clickBodyButton(zhCN.common.cancel)
    await vi.waitFor(() => expect(drawerShow()).toBe(false))
    expect(postBulkPriceMock, 'only the dry run was sent').toHaveBeenCalledTimes(1)
  })

  it('a dry-run response that lands after a close-and-reopen cannot flip the fresh session: the reopened drawer keeps the chooser', async () => {
    await mountPage(BULK_MODELS())
    await setChecked('models-row-check--1', true)
    await setChecked('models-row-check--2', true)
    const toolbar = bulkToolbarButton()!
    toolbar.click()
    await flushPromises()
    await vi.waitFor(() => expect(document.body.querySelector('.bulk-choose')).toBeTruthy())
    await setFactor(1.1)
    // Defer the dry run: it stays in flight until this test pulls it.
    let resolveDryRun: (result: BulkPriceResult) => void = () => {}
    postBulkPriceMock.mockReturnValueOnce(new Promise<BulkPriceResult>((resolve) => (resolveDryRun = resolve)))
    await clickBodyButton(zhCN.models.bulkPreviewButton)
    expect(document.body.querySelector('.bulk-preview'), 'still waiting on the dry run').toBeNull()

    // In flight the drawer is dismissal-locked (no cancel affordance), so
    // the close is driven the way cancel performs it internally — show goes
    // false, then the finished leave transition fires after-leave (happy-dom
    // never does). That pairs the seq bump with the reopen below exactly as
    // a real close-and-reopen would.
    expect(bulkDrawer().props('dismissable'), 'in flight: dismissal locked').toBe(false)
    bulkDrawer().vm.$emit('update:show', false)
    bulkDrawer().vm.$emit('after-leave')
    await flushPromises()
    expect(drawerShow()).toBe(false)
    bulkToolbarButton()!.click()
    await flushPromises()
    await vi.waitFor(() => expect(drawerShow()).toBe(true))
    expect(bulkDrawer().props('title')).toBe(zhCN.models.bulkDialogTitle)
    expect(bulkDrawer().props('dismissable'), 'reopened idle: dismissal unlocked').toBe(true)

    // The stale response lands against the open, REOPENED drawer: it must be
    // retired — no preview flip, the chooser stays, and nothing is captured
    // for a later confirm to apply with a factor this session never saw.
    resolveDryRun(SCALE_DRY_RUN)
    await flushPromises()
    expect(document.body.querySelector('.bulk-preview'), 'the stale preview must not flip the reopened drawer').toBeNull()
    expect(bulkDrawer().props('title'), 'still the chooser title').toBe(zhCN.models.bulkDialogTitle)
    expect(bodyButton(zhCN.models.bulkPreviewButton), 'the chooser confirm label is still showing').toBeTruthy()
    expect(bodyButton(zhCN.models.bulkConfirmApply), 'the apply label is not showing').toBeUndefined()
  })

  it('the drawer withholds dismissal only while a request is in flight, and the cancel affordance with it', async () => {
    await mountPage(BULK_MODELS())
    await setChecked('models-row-check--1', true)
    const toolbar = bulkToolbarButton()!
    toolbar.click()
    await flushPromises()
    await vi.waitFor(() => expect(document.body.querySelector('.bulk-choose')).toBeTruthy())

    // Idle chooser: cancel / mask / Esc all live.
    expect(bulkDrawer().props('dismissable'), 'idle chooser: dismissable').toBe(true)
    expect(bodyButton(zhCN.common.cancel), 'idle: the cancel button is offered').toBeTruthy()

    // Preview in flight: locked — the write is the point of no return, and
    // the preview fetch rides the same lock.
    let resolveDryRun: (result: BulkPriceResult) => void = () => {}
    postBulkPriceMock.mockReturnValueOnce(new Promise<BulkPriceResult>((resolve) => (resolveDryRun = resolve)))
    await setFactor(1.1)
    await clickBodyButton(zhCN.models.bulkPreviewButton)
    expect(bulkDrawer().props('dismissable'), 'preview in flight: locked').toBe(false)
    expect(bodyButton(zhCN.common.cancel), 'in flight: the cancel button is withheld').toBeUndefined()

    // Landed: back to eyes-on preview, no request in flight — walk-away
    // allowed again.
    resolveDryRun(SCALE_DRY_RUN)
    await flushPromises()
    await vi.waitFor(() => expect(document.body.querySelector('.bulk-preview')).toBeTruthy())
    expect(bulkDrawer().props('dismissable'), 'idle preview: dismissable').toBe(true)
    expect(bodyButton(zhCN.common.cancel), 'idle preview: the cancel button is back').toBeTruthy()

    // Apply in flight: sealed.
    let rejectApply: (err: unknown) => void = () => {}
    postBulkPriceMock.mockReturnValueOnce(new Promise<BulkPriceResult>((_, reject) => (rejectApply = reject)))
    await clickBodyButton(zhCN.models.bulkConfirmApply)
    expect(bulkDrawer().props('dismissable'), 'apply in flight: locked').toBe(false)
    expect(bodyButton(zhCN.common.cancel), 'apply in flight: the cancel button is withheld').toBeUndefined()

    // Refused: the spinner releases and dismissal unlocks for the retry.
    rejectApply(new Error('boom'))
    await flushPromises()
    expect(bulkDrawer().props('dismissable'), 'after the refusal: unlocked for the retry').toBe(true)
    expect(bodyButton(zhCN.common.cancel), 'after the refusal: the cancel button is back').toBeTruthy()
  })

  it('a retired apply releases no busy lock: its late finally leaves the fresh session spinner running', async () => {
    await mountPage(BULK_MODELS())
    await setChecked('models-row-check--1', true)
    await setChecked('models-row-check--2', true)
    await openScalePreview(1.1, SCALE_DRY_RUN)

    // Defer the apply write, then let it succeed: the drawer closes and the
    // post-apply reload starts — still deferred, so the apply's finally is
    // parked on the reload await.
    let resolveApply: (result: BulkPriceResult) => void = () => {}
    postBulkPriceMock.mockReturnValueOnce(new Promise<BulkPriceResult>((resolve) => (resolveApply = resolve)))
    await clickBodyButton(zhCN.models.bulkConfirmApply)
    let resolveReload: (value: { list: Model[] }) => void = () => {}
    listModelsMock.mockImplementationOnce(() => new Promise<{ list: Model[] }>((resolve) => (resolveReload = resolve)))
    resolveApply(APPLY_RESULT)
    await flushPromises()
    expect(drawerShow(), 'the apply closed the drawer').toBe(false)

    // The leave finishes (happy-dom needs the explicit signal): the close
    // retires the apply request and releases the lock.
    bulkDrawer().vm.$emit('after-leave')
    await flushPromises()

    // A fresh session starts and fires its own preview, deferred in flight:
    // its busy flag is now the live lock this test protects. The success
    // cleared the selection, so re-tick a row first.
    await setChecked('models-row-check--1', true)
    bulkToolbarButton()!.click()
    await flushPromises()
    await vi.waitFor(() => expect(document.body.querySelector('.bulk-choose')).toBeTruthy())
    await setFactor(1.1)
    postBulkPriceMock.mockReturnValueOnce(new Promise<BulkPriceResult>(() => {}))
    await clickBodyButton(zhCN.models.bulkPreviewButton)
    expect(document.body.querySelector('.bulk-preview'), 'the fresh preview is in flight').toBeNull()

    // NOW the stale apply's reload lands and its finally runs: the retired
    // request must leave the fresh session's busy lock alone — the drawer's
    // confirm keeps its loading state, so a stray click cannot fire a second
    // write behind the first session's back (the double-apply window).
    resolveReload({ list: BULK_MODELS() })
    await flushPromises()
    expect(bulkDrawer().props('loading'), 'the fresh session spinner survives the stale finally').toBe(true)
    expect(postBulkPriceMock, 'no extra write was fired').toHaveBeenCalledTimes(3)
  })

  it('a candidate union past the 2000 cap warns with the counts and never opens the drawer', async () => {
    // Three selected models x 700 candidates = a 2100-id union: the endpoint
    // binds candidate_ids to 1..2000 raw, so the gate fires on the UNION.
    const crowd = Array.from({ length: 700 }, (_, i) =>
      candidateFixture({ id: i + 1, provider_id: 1, provider_name: `Provider ${(i % 7) + 1}`, provider_model_name: `up-a-${i + 1}` }),
    )
    const crowd2 = Array.from({ length: 700 }, (_, i) =>
      candidateFixture({ id: 701 + i, provider_id: 2, provider_name: 'Provider Two', provider_model_name: `up-b-${i + 1}` }),
    )
    const crowd3 = Array.from({ length: 700 }, (_, i) =>
      candidateFixture({ id: 1401 + i, provider_id: 3, provider_name: 'Provider Three', provider_model_name: `up-c-${i + 1}` }),
    )
    await mountPage([modelFixture(1, 'alpha-model', crowd), modelFixture(2, 'beta-model', crowd2), modelFixture(3, 'gamma-model', crowd3)])
    await setChecked('models-row-check--1', true)
    await setChecked('models-row-check--2', true)
    await setChecked('models-row-check--3', true)
    const toolbar = bulkToolbarButton()
    expect(toolbar, 'the bulk toolbar button rendered').toBeTruthy()
    expect(toolbar!.disabled).toBe(false)
    toolbar!.click()
    await flushPromises()
    expect(drawerShow(), 'the drawer stays closed over the cap').toBe(false)
    expect(document.body.querySelector('.bulk-choose')).toBeNull()
    expect(document.body.textContent).toContain(zhCN.models.bulkOverCap.replace('{cap}', '2000').replace('{count}', '2100'))
    expect(postBulkPriceMock, 'nothing was sent').not.toHaveBeenCalled()
  })

  it('an all-skipped preview disables the apply button and says so', async () => {
    await mountPage(BULK_MODELS())
    await setChecked('models-row-check--1', true)
    await openScalePreview(1.1, {
      items: [
        bulkItem({ candidate_id: 1, changed: false, skip_reason: 'unchanged' }),
        bulkItem({ candidate_id: 2, provider_name: 'Provider Two', changed: false, skip_reason: 'unpriced' }),
      ],
      applied: 0,
      skipped: 2,
    })
    expect(bodyButton(zhCN.models.bulkConfirmApply)!.disabled, 'nothing to apply').toBe(true)
    expect(document.body.textContent).toContain(zhCN.models.bulkPreviewNothingToApply)
  })

  it('a failed apply reports the error and the drawer stays open', async () => {
    await mountPage(BULK_MODELS())
    await setChecked('models-row-check--1', true)
    await openScalePreview(1.1, SCALE_DRY_RUN)
    postBulkPriceMock.mockRejectedValueOnce(new Error('boom'))
    await clickBodyButton(zhCN.models.bulkConfirmApply)
    await flushPromises()
    expect(document.body.querySelector('.bulk-preview'), 'the drawer stays open on failure').toBeTruthy()
    // The failure path must not reload: the list only refetches on success.
    expect(listModelsMock).toHaveBeenCalledTimes(1)
  })
})
