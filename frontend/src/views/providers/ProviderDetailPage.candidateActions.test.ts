// @vitest-environment happy-dom
//
// The provider detail page's models-tab inline actions — the dropdown menu
// itself, the shared candidate edit modal, and the single-row delete with its
// last-routable-candidate guard. The batch's own new-menu seam lives here:
// every menu assertion OPENS the menu first (clicks the row's '⋯' trigger)
// and then reads the rendered items, rather than asserting on component
// props.
//
// Covered acceptance cases:
//
//   TC-01 every row's menu carries edit + delete — passed rows included (the
//        pre-batch table rendered nothing at all for them) — and the retest
//        entry appears only on rows without a passing verdict that the probe
//        queue does not already hold
//   TC-02 the edit modal backfills the row and saves a PATCH carrying the
//        row's model_id + candidate_id; the refreshed table shows the new values
//   TC-03 delete: confirm calls the API and the row disappears; cancel never
//        reaches the API
//   TC-04 the guard: deleting the model's last routable candidate escalates
//        the confirm copy to the consequence warning
//   TC-10 no guard: another routable candidate keeps the plain confirm copy
//   TC-18 a narrow viewport renders the same actions through the OptionSheet
//        bottom panel, with the same item set as the desktop menu
//
// Fixtures ride the same API-module mocks as every other page test here;
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
  updateCandidate,
  type Model,
  type ModelCandidate,
  type ProviderCandidate,
  type UpdateCandidateResult,
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
    updateCandidate: vi.fn(),
    suggestCandidatePrice: vi.fn(),
  }
})

const getProviderMock = vi.mocked(getProvider)
const catalogueMock = vi.mocked(listModelsForProvider)
const listCandidatesMock = vi.mocked(listProviderCandidates)
const getModelMock = vi.mocked(getModel)
const deleteMock = vi.mocked(deleteCandidate)
const updateMock = vi.mocked(updateCandidate)
const suggestMock = vi.mocked(suggestCandidatePrice)

// Must be imported after the vi.mock factories are registered.
import ProviderDetailPage from './ProviderDetailPage.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import zhCN from '../../locales/zh-CN'
import { NInputNumber } from 'naive-ui'
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

// A controllable MediaQueryList (same stub shape as DefaultLayout.test.ts):
// `setMobile` updates `matches` and notifies every registered listener,
// which is the surface useIsMobile (and naive-ui) consume.
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

// The row set TC-01 works against: one row per retest condition.
function menuFixtureRows() {
  return [
    // Passed: edit/delete only — this row had NO actions at all before the batch.
    candidateRow({ candidate_id: 11, model_name: 'model-passed' }),
    // Failed (probed, verdict FAILED): the retest entry joins edit/delete.
    candidateRow({
      candidate_id: 12,
      model_name: 'model-failed',
      management_status: CANDIDATE_STATUS_DISABLED,
      verification_status: VERIFICATION_FAILED,
      last_test_result: 7,
      last_test_error: 'upstream 500',
    }),
    // Untested leftover (no verdict, queue empty): retest still applies.
    candidateRow({
      candidate_id: 14,
      model_name: 'model-untested',
      management_status: CANDIDATE_STATUS_DISABLED,
      verification_status: VERIFICATION_UNTESTED,
      last_test_result: null,
      last_tested_at: null,
    }),
    // Held by the probe queue: its probe is already coming, no retest entry.
    candidateRow({
      candidate_id: 13,
      model_name: 'model-queued',
      management_status: CANDIDATE_STATUS_DISABLED,
      verification_status: VERIFICATION_UNTESTED,
      last_test_result: null,
      last_tested_at: null,
      queue_state: 'queued',
      auto_enable_on_pass: true,
    }),
  ]
}

// Each list response the page will pull, in order; the last one repeats for
// any further fetches.
function primeList(...lists: ProviderCandidate[][]) {
  for (const list of lists) listCandidatesMock.mockResolvedValueOnce({ list })
  if (lists.length > 0) listCandidatesMock.mockResolvedValue({ list: lists[lists.length - 1] })
}

// A model-detail candidate for the getModel fixture (the guard's routable
// data source).
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

function modelDetail(candidates: ModelCandidate[], id = 100, name = 'model-passed'): Model {
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

async function mountPage() {
  getProviderMock.mockResolvedValue(providerFixture())
  catalogueMock.mockResolvedValue({ models: [], outcome: null, detail: '' })
  // The edit modal's price look-up: a history-sourced suggestion whose values
  // match the stored row, so opening/editing a candidate never lands in the
  // modal's unresolved-price state (which would block a save under test).
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

// Opens a row's '⋯' actions menu the way a user does — clicking the trigger
// button — and returns the menu's rendered item labels (dividers excluded).
// This is the batch's own menu seam: the items only exist in the DOM once the
// menu is open, so the assertion covers both "the trigger opens a menu" and
// "the menu carries these entries".
async function openRowMenu(modelName: string): Promise<string[]> {
  const trigger = rowTr(modelName).querySelector('button')
  expect(trigger, `the actions trigger on ${modelName}'s row rendered`).toBeTruthy()
  trigger!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await nextTick()
  await nextTick()
  const menus = [...document.body.querySelectorAll('.n-dropdown-menu')]
  const menu = menus[menus.length - 1]
  expect(menu, `the actions menu for ${modelName} opened`).toBeTruthy()
  return [...menu!.querySelectorAll('.n-dropdown-option')]
    .filter((opt) => !opt.classList.contains('n-dropdown-option--divider'))
    .map((opt) => (opt.textContent ?? '').trim())
}

// Clicks the currently open menu's item by its visible label.
async function clickMenuItem(label: string) {
  const menus = [...document.body.querySelectorAll('.n-dropdown-menu')]
  const menu = menus[menus.length - 1]
  const item = [...menu!.querySelectorAll('.n-dropdown-option')].find(
    (opt) => (opt.textContent ?? '').trim() === label,
  )
  expect(item, `menu item "${label}" rendered`).toBeTruthy()
  // naive-ui binds the click handler on the option's inner body element, so
  // the event must originate there — dispatching on the outer option div
  // would bubble past it and never fire.
  const target = item!.querySelector('.n-dropdown-option-body') ?? item!
  target.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await nextTick()
  await nextTick()
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

describe('ProviderDetailPage candidate actions (TC-01..04, TC-10, TC-18)', () => {
  it('TC-01: every row menu carries edit + delete — passed rows included; retest stays conditional', async () => {
    primeList(menuFixtureRows())
    await mountPage()

    const edit = zhCN.models.editCandidate
    const del = zhCN.models.deleteCandidate
    const retest = zhCN.models.retest

    // The passed row's menu — empty before the batch — now edits and deletes.
    expect(await openRowMenu('model-passed')).toEqual([edit, del])
    // Failed and untested-leftover rows additionally offer the retest entry.
    expect(await openRowMenu('model-failed')).toEqual([edit, retest, del])
    expect(await openRowMenu('model-untested')).toEqual([edit, retest, del])
    // A row the probe queue holds gets no retest on top of its pending probe.
    expect(await openRowMenu('model-queued')).toEqual([edit, del])
  })

  it('TC-18: a narrow viewport serves the same item set through the OptionSheet bottom panel', async () => {
    stubMatchMedia(true)
    primeList(menuFixtureRows())
    await mountPage()

    // Mobile renders the table as cards; the row's actions trigger becomes a
    // labelled button (the common.actions copy) that raises the bottom sheet.
    const sheetLabels = async (modelName: string): Promise<string[]> => {
      const card = [...document.body.querySelectorAll('.rdt-card')].find((el) =>
        (el.textContent ?? '').includes(modelName),
      )
      expect(card, `card for ${modelName} rendered`).toBeTruthy()
      const trigger = card!.querySelector<HTMLButtonElement>('.rd-trigger-btn')
      expect(trigger, `the actions trigger on ${modelName}'s card rendered`).toBeTruthy()
      trigger!.click()
      await nextTick()
      await nextTick()
      const sheets = [...document.body.querySelectorAll('.option-sheet')]
      const sheet = sheets[sheets.length - 1]
      expect(sheet, `the option sheet for ${modelName} opened`).toBeTruthy()
      const labels = [...sheet!.querySelectorAll<HTMLButtonElement>('.option-sheet__option')].map((b) =>
        (b.textContent ?? '').trim(),
      )
      return labels
    }

    // Same item sets as the desktop menu asserted in TC-01.
    expect(await sheetLabels('model-passed')).toEqual([zhCN.models.editCandidate, zhCN.models.deleteCandidate])
    expect(await sheetLabels('model-failed')).toEqual([
      zhCN.models.editCandidate,
      zhCN.models.retest,
      zhCN.models.deleteCandidate,
    ])
  })

  it('TC-02: edit backfills the row and PATCHes with the row ids; the refreshed table shows the new values', async () => {
    const row = candidateRow({
      candidate_id: 11,
      model_id: 100,
      model_name: 'model-passed',
      provider_model_name: 'upstream-passed',
    })
    primeList([row], [{ ...row, provider_model_name: 'renamed-upstream', input_price: 9 }])
    await mountPage()
    updateMock.mockResolvedValue({
      candidate: modelCandidate({ id: 11, provider_model_name: 'renamed-upstream', input_price: 9 }),
      report: null,
      report_applied: true,
    } satisfies UpdateCandidateResult)

    await openRowMenu('model-passed')
    await clickMenuItem(zhCN.models.editCandidate)

    // The shared candidate modal opened with the row backfilled: edit title,
    // the row's provider model name, and the row's prices in the inputs —
    // scoped to the modal element, the table behind shows the same strings.
    const modals = [...document.body.querySelectorAll('.n-modal')]
    const modal = modals[modals.length - 1]
    expect(modal, 'the edit modal rendered').toBeTruthy()
    const modalText = modal!.textContent ?? ''
    expect(modalText).toContain(zhCN.models.editCandidate)
    expect(modalText).toContain('upstream-passed')
    expect(modalText, 'the outward model name is shown read-only').toContain('model-passed')
    const inputValues = [...modal!.querySelectorAll('input')].map((i) => i.value)
    expect(inputValues, 'input price backfilled').toContain('1.5')
    expect(inputValues, 'output price backfilled').toContain('2.5')

    // Rename the upstream model FIRST (the modal re-prices the new pair, and
    // a price typed before the rename would read as a stale edit and block
    // the save), then change the input price, then save.
    // Generic components (FilterSelectField) and naive-ui's exported
    // component types do not register as component selectors for
    // findAllComponents' typing, so the wrappers are cast before their vm
    // seams are driven.
    const nameField = wrapper!.findAllComponents(FilterSelectField)[0] as unknown as VueWrapper
    expect(nameField, 'the provider-model-name select rendered').toBeTruthy()
    nameField.vm.$emit('update:value', 'renamed-upstream')
    await nextTick()
    const numbers = wrapper!.findAllComponents(NInputNumber)
    expect(numbers.length, 'the token price grid rendered its number fields').toBeGreaterThanOrEqual(1)
    ;(numbers[0] as unknown as VueWrapper).vm.$emit('update:value', 9)
    await nextTick()

    const save = [...modal!.querySelectorAll('button')].find((b) => (b.textContent ?? '').trim() === zhCN.models.save)
    expect(save, 'the save button rendered').toBeTruthy()
    save!.click()

    // The PATCH carries the ROW's identifiers — the provider page's row ids
    // (model_id / candidate_id), not the model page's vocabulary — plus the
    // edited values.
    await vi.waitFor(() => expect(updateMock).toHaveBeenCalledTimes(1))
    expect(updateMock).toHaveBeenCalledWith(
      100,
      11,
      expect.objectContaining({ provider_model_name: 'renamed-upstream', input_price: 9 }),
    )
    // The saved mapping refreshes into the table.
    await vi.waitFor(() => expect(bodyText()).toContain('renamed-upstream'))
    expect(listCandidatesMock, 'a save refreshes the mapping list').toHaveBeenCalledTimes(2)
  })

  it('TC-03: delete confirms through a dialog then the row disappears; cancel never reaches the API', async () => {
    const passed = candidateRow({
      candidate_id: 11,
      model_id: 100,
      model_name: 'model-passed',
      provider_model_name: 'upstream-passed',
    })
    const other = candidateRow({ candidate_id: 15, model_id: 101, model_name: 'model-other' })
    primeList([passed, other], [other])
    await mountPage()
    // Another routable candidate on the model keeps this the plain confirm
    // (the guard arms only for the last routable one — TC-04/TC-10).
    getModelMock.mockResolvedValue(modelDetail([modelCandidate({ id: 11 }), modelCandidate({ id: 999 })]))
    deleteMock.mockResolvedValue(undefined)

    const plainCopy = zhCN.providers.confirmDeleteCandidateContent
      .replace('{model}', 'model-passed')
      .replace('{name}', 'upstream-passed')

    // Cancel path: the dialog asks, the answer "no" keeps everything.
    await openRowMenu('model-passed')
    await clickMenuItem(zhCN.models.deleteCandidate)
    await vi.waitFor(() => expect(bodyText()).toContain(plainCopy))
    expect(deleteMock, 'nothing is deleted before the confirm').not.toHaveBeenCalled()
    dialogButton(zhCN.providers.cancel).click()
    await nextTick()
    await nextTick()
    expect(deleteMock, 'the cancel path never reaches the API').not.toHaveBeenCalled()
    expect(bodyText()).toContain('model-passed')

    // Confirm path: the delete API fires with the row ids and the row goes.
    await openRowMenu('model-passed')
    await clickMenuItem(zhCN.models.deleteCandidate)
    await vi.waitFor(() => expect(bodyText()).toContain(plainCopy))
    dialogButton(zhCN.common.delete).click()
    await vi.waitFor(() => expect(deleteMock).toHaveBeenCalledWith(100, 11))
    // The row disappears from the TABLE. (A closed naive dialog's copy can
    // linger in the teleported DOM under happy-dom — its leave transition
    // never completes — so the assertion is row-scoped, not body-wide.)
    await vi.waitFor(() => {
      const rows = [...document.body.querySelectorAll('tr')]
      expect(rows.some((el) => (el.textContent ?? '').includes('model-passed'))).toBe(false)
    })
    expect(bodyText()).toContain('model-other')
  })

  it('TC-04: deleting the model\'s last routable candidate escalates the confirm to the consequence warning', async () => {
    const passed = candidateRow({
      candidate_id: 11,
      model_id: 100,
      model_name: 'model-passed',
      provider_model_name: 'upstream-passed',
    })
    primeList([passed], [])
    await mountPage()
    // The model's candidate set: the row itself routable, a sibling present
    // but NOT routable — the row is the last routable one.
    getModelMock.mockResolvedValue(
      modelDetail([
        modelCandidate({ id: 11, routable: true }),
        modelCandidate({ id: 12, routable: false, blocked_by: 'not_verified' }),
      ]),
    )
    deleteMock.mockResolvedValue(undefined)

    await openRowMenu('model-passed')
    await clickMenuItem(zhCN.models.deleteCandidate)
    // The guard prefetches the model's own routable data before the confirm.
    await vi.waitFor(() => expect(getModelMock).toHaveBeenCalledWith(100))
    const warnCopy = zhCN.providers.confirmDeleteCandidateLastRoutableContent
      .replace('{model}', 'model-passed')
      .replace('{name}', 'upstream-passed')
    const plainCopy = zhCN.providers.confirmDeleteCandidateContent
      .replace('{model}', 'model-passed')
      .replace('{name}', 'upstream-passed')
    await vi.waitFor(() => expect(bodyText()).toContain(warnCopy))
    expect(bodyText(), 'the plain confirm copy does not appear').not.toContain(plainCopy)

    // The warning is advisory: confirming still deletes.
    dialogButton(zhCN.common.delete).click()
    await vi.waitFor(() => expect(deleteMock).toHaveBeenCalledWith(100, 11))
  })

  it('TC-10: another routable candidate keeps the plain confirm copy', async () => {
    const passed = candidateRow({
      candidate_id: 11,
      model_id: 100,
      model_name: 'model-passed',
      provider_model_name: 'upstream-passed',
    })
    primeList([passed], [])
    await mountPage()
    getModelMock.mockResolvedValue(modelDetail([modelCandidate({ id: 11 }), modelCandidate({ id: 999 })]))

    await openRowMenu('model-passed')
    await clickMenuItem(zhCN.models.deleteCandidate)
    await vi.waitFor(() => expect(getModelMock).toHaveBeenCalledWith(100))
    const plainCopy = zhCN.providers.confirmDeleteCandidateContent
      .replace('{model}', 'model-passed')
      .replace('{name}', 'upstream-passed')
    const warnCopy = zhCN.providers.confirmDeleteCandidateLastRoutableContent
      .replace('{model}', 'model-passed')
      .replace('{name}', 'upstream-passed')
    await vi.waitFor(() => expect(bodyText()).toContain(plainCopy))
    expect(bodyText(), 'the consequence warning does not appear').not.toContain(warnCopy)
  })
})
