// @vitest-environment happy-dom
//
// The provider detail page's wiring of the shared mapping table — the seam
// the shared-table refactor added. The page's own baseline behaviors are
// pinned by the earlier suites (candidateActions / batchDelete /
// candidateFilter / the baseline file); what this file pins is the INJECTION:
// the page feeds the shared table the right column config, the right row
// identity adapter, and the right action handlers.
//
// Covered acceptance cases:
//
//   TC-04 (page leg) the probe badge column is configured in, the capability
//        tri-state columns are not — this page's rows carry the probe-queue
//        fields, not the capability flags
//   TC-03 (page leg) row actions resolve the provider row's own ids —
//        candidate_id + model_id — end to end: the delete guard prefetches
//        getModel(row.model_id), the confirm escalates on the last routable
//        candidate, and the DELETE call carries both ids; the edit modal
//        opens backfilled from the row
//   wiring the busy row the page tracks (its single-flight retest) spins
//        exactly that row's menu
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
  retestCandidate,
  suggestCandidatePrice,
  type Model,
  type ModelCandidate,
  type ProviderCandidate,
} from '../../api/models'

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return { ...actual, getProvider: vi.fn(), listModelsForProvider: vi.fn() }
})

// The page header's proxy-warning icon resolves its stance from the global
// outbound-proxy setting, fetched on every mount — mock it to the direct
// (off) stance so the icon stays bare and no suite here reaches the wire.
import { getOutboundProxy } from '../../api/systemSettings'

vi.mock('../../api/systemSettings', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/systemSettings')>()
  return { ...actual, getOutboundProxy: vi.fn() }
})
vi.mocked(getOutboundProxy).mockResolvedValue({ mode: 'off', url: '', version: 0 })

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return {
    ...actual,
    listProviderCandidates: vi.fn(),
    getModel: vi.fn(),
    deleteCandidate: vi.fn(),
    retestCandidate: vi.fn(),
    suggestCandidatePrice: vi.fn(),
  }
})

const getProviderMock = vi.mocked(getProvider)
const catalogueMock = vi.mocked(listModelsForProvider)
const listCandidatesMock = vi.mocked(listProviderCandidates)
const getModelMock = vi.mocked(getModel)
const deleteMock = vi.mocked(deleteCandidate)
const retestMock = vi.mocked(retestCandidate)
const suggestMock = vi.mocked(suggestCandidatePrice)

// Must be imported after the vi.mock factories are registered.
import ProviderDetailPage from './ProviderDetailPage.vue'
import ResponsiveDropdown from '../../components/common/ResponsiveDropdown.vue'
import zhCN from '../../locales/zh-CN'
import {
  CANDIDATE_STATUS_DISABLED,
  CANDIDATE_STATUS_ENABLED,
  VERIFICATION_FAILED,
  VERIFICATION_PASSED,
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
    proxy_mode: '',
    proxy_url: '',
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

function modelDetail(candidates: ModelCandidate[], id = 100, name = 'model-one'): Model {
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

async function mountPage(rows: ProviderCandidate[]) {
  getProviderMock.mockResolvedValue(providerFixture())
  catalogueMock.mockResolvedValue({ models: [], outcome: null, detail: '' })
  listCandidatesMock.mockResolvedValue({ list: rows })
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

// Opens a row's '⋯' actions menu the way a user does and returns the menu's
// rendered item labels (dividers excluded).
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
  stubMatchMedia()
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('ProviderDetailPage shared-table wiring (TC-04/TC-03 page legs)', () => {
  it('injects the probe badge column and not the capability columns', async () => {
    await mountPage([candidateRow()])
    expect(bodyText(), 'the probe badge column header rendered').toContain(zhCN.providers.candidateProbeStatus)
    expect(bodyText(), 'no streaming capability column on this page').not.toContain(zhCN.models.supportsStreaming)
    expect(bodyText(), 'no function-calling capability column on this page').not.toContain(
      zhCN.models.supportsFunctionCalling,
    )
  })

  it('row actions resolve the provider row ids end to end — guard, escalation, DELETE, and the edit modal', async () => {
    const passed = candidateRow({
      candidate_id: 11,
      model_id: 100,
      model_name: 'model-passed',
      provider_model_name: 'upstream-passed',
    })
    await mountPage([passed])
    // The row is the model's LAST routable candidate — the guard context the
    // page injects must escalate the confirm.
    getModelMock.mockResolvedValue(
      modelDetail([modelCandidate({ id: 11, routable: true }), modelCandidate({ id: 12, routable: false })]),
    )
    deleteMock.mockResolvedValue(undefined)

    // The edit context: the modal opens backfilled from the row.
    await openRowMenu('model-passed')
    await clickMenuItem(zhCN.models.editCandidate)
    const modals = [...document.body.querySelectorAll('.n-modal')]
    const modal = modals[modals.length - 1]
    expect(modal, 'the edit modal rendered').toBeTruthy()
    expect(modal!.textContent ?? '', 'the edit modal carries the row').toContain('upstream-passed')
    const close = [...modal!.querySelectorAll('button')].find((b) => (b.getAttribute('aria-label') ?? b.textContent ?? '') !== '')
    close?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await nextTick()

    // The delete: the guard prefetches the ROW's model_id, the confirm
    // escalates for the last routable candidate, and the DELETE carries both
    // row ids.
    await openRowMenu('model-passed')
    await clickMenuItem(zhCN.models.deleteCandidate)
    await vi.waitFor(() => expect(getModelMock).toHaveBeenCalledWith(100))
    const warnCopy = zhCN.providers.confirmDeleteCandidateLastRoutableContent
      .replace('{model}', 'model-passed')
      .replace('{name}', 'upstream-passed')
    await vi.waitFor(() => expect(bodyText()).toContain(warnCopy))
    dialogButton(zhCN.common.delete).click()
    await vi.waitFor(() => expect(deleteMock).toHaveBeenCalledWith(100, 11))
  })

  it('the busy row the page tracks spins exactly that row’s menu', async () => {
    // Two FAILED rows — failed rows carry the retest entry, so the sibling's
    // menu has the (single-flight-disabled) entry to assert.
    const failed = {
      verification_status: VERIFICATION_FAILED,
      management_status: CANDIDATE_STATUS_DISABLED,
      last_test_result: 7,
      last_test_error: 'upstream 500',
    }
    await mountPage([
      candidateRow({ candidate_id: 11, model_id: 100, model_name: 'model-one', provider_model_name: 'up-one', ...failed }),
      candidateRow({ candidate_id: 12, model_id: 101, model_name: 'model-two', provider_model_name: 'up-two', ...failed }),
    ])
    // A retest that never settles keeps row 1's single-flight action busy
    // for the rest of the test (teardown unmounts the page before anything
    // would need the promise resolved).
    retestMock.mockImplementation(() => new Promise<never>(() => {}))

    await openRowMenu('model-one')
    await clickMenuItem(zhCN.models.retest)
    await vi.waitFor(() => expect(retestMock).toHaveBeenCalledWith(100, 11))

    // Exactly the retesting row's trigger disables; the sibling stays
    // operable, and the single-flight rule disables the retest entry on the
    // sibling's menu.
    const dropdowns = wrapper!.findAllComponents(ResponsiveDropdown)
    expect(dropdowns[0].props('disabled'), 'the retesting row disables its trigger').toBe(true)
    expect(dropdowns[1].props('disabled'), 'the sibling stays operable').toBe(false)
    const siblingOptions = dropdowns[1].props('options') as Array<{ key?: string; disabled?: boolean; type?: string }>
    expect(
      siblingOptions.find((o) => o.key === 'retest')?.disabled,
      "the sibling's retest entry disables while a retest is in flight",
    ).toBe(true)
  })
})
