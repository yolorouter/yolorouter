// @vitest-environment happy-dom
//
// The model detail page's wiring of the shared mapping table — the seam the
// shared-table refactor added. The page's baseline behaviors are pinned by
// ModelDetailPage.test.ts / ModelDetailPage.candidateFilter.test.ts; what
// this file pins is the INJECTION: the page feeds the shared table the right
// column config, its row-identity adapter (row.id + the page's one model),
// and the action handlers.
//
// Covered acceptance cases:
//
//   TC-04 (page leg) the capability tri-state columns are configured in, the
//        probe badge column is not — this page's rows carry the capability
//        flags, not the probe-queue fields
//   TC-03 (page leg) row actions resolve the model page's own ids end to
//        end: the '⋯' menu's retest/delete speak (MODEL_ID, row.id), an
//        unconfirmed capability tag's click retests the same way, and the
//        delete confirm (the page's guard context) fires the DELETE with the
//        same pair
//
// Fixtures ride the same API-module mocks as the page's baseline suites;
// assertions read the zh-CN copy through the locale module so a copy edit
// does not fake a regression.
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
  retestCandidate,
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
    retestCandidate: vi.fn(),
    deleteCandidate: vi.fn(),
  }
})

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return { ...actual, listModelsForProvider: vi.fn() }
})

const getModelMock = vi.mocked(getModel)
const getImpactMock = vi.mocked(getModelImpact)
const retestMock = vi.mocked(retestCandidate)
const deleteMock = vi.mocked(deleteCandidate)
const catalogueMock = vi.mocked(listModelsForProvider)

// Must be imported after the vi.mock factories are registered.
import ModelDetailPage from './ModelDetailPage.vue'
import zhCN from '../../locales/zh-CN'
import { VERIFICATION_PASSED } from '../../api/candidateStatus'

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

function modelFixture(candidates: ModelCandidate[]): Model {
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

// The body row rendering a candidate, located by its provider model name
// (unique in every fixture here).
function rowTr(providerModelName: string): HTMLTableRowElement {
  const tr = [...document.body.querySelectorAll('tr')].find((el) => (el.textContent ?? '').includes(providerModelName))
  expect(tr, `row for ${providerModelName} rendered`).toBeTruthy()
  return tr!
}

// Opens a row's '⋯' actions menu the way a user does and returns the menu's
// rendered item labels (dividers excluded).
async function openRowMenu(providerModelName: string): Promise<string[]> {
  // This page's rows carry the reorder up/down buttons (titled) BEFORE the
  // untitled '⋯' actions trigger — the last untitled button is the menu's.
  const trigger = [...rowTr(providerModelName).querySelectorAll('button')]
    .filter((b) => !b.getAttribute('title'))
    .pop()
  expect(trigger, `the actions trigger on ${providerModelName}'s row rendered`).toBeTruthy()
  trigger!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await nextTick()
  await nextTick()
  const menus = [...document.body.querySelectorAll('.n-dropdown-menu')]
  const menu = menus[menus.length - 1]
  expect(menu, `the actions menu for ${providerModelName} opened`).toBeTruthy()
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

describe('ModelDetailPage shared-table wiring (TC-04/TC-03 page legs)', () => {
  it('injects the capability columns and not the probe badge column', async () => {
    await mountPage([candidateFixture()])
    expect(bodyText(), 'the streaming capability column header rendered').toContain(zhCN.models.supportsStreaming)
    expect(bodyText(), 'the function-calling capability column header rendered').toContain(zhCN.models.supportsFunctionCalling)
    expect(bodyText(), 'no probe badge column on this page').not.toContain(zhCN.providers.candidateProbeStatus)
  })

  it('row actions resolve row.id + the page model id end to end — menu retest, capability tag, and delete', async () => {
    const rows = [
      candidateFixture({ id: 1, provider_model_name: 'alpha-model', supports_streaming: true, sort_order: 1 }),
      // sort_order > 1 keeps the delete confirm off the first-choice copy;
      // one routable sibling remains, so the guard names the count.
      candidateFixture({ id: 2, provider_name: 'beta', provider_model_name: 'beta-model', sort_order: 2 }),
      // An unconfirmed capability: its '?' tag is the retest entry the
      // capability column renders.
      candidateFixture({
        id: 3,
        provider_name: 'gamma',
        provider_model_name: 'gamma-model',
        supports_streaming: null,
        // Not routable, so deleting beta leaves exactly ONE routable
        // candidate — the count the guard's confirm names.
        routable: false,
        blocked_by: 'not_verified',
        sort_order: 3,
      }),
    ]
    await mountPage(rows)
    retestMock.mockResolvedValue({ candidate: rows[2], applied: true })
    deleteMock.mockResolvedValue(undefined)

    // The '⋯' menu's retest speaks the model page's pair.
    await openRowMenu('beta-model')
    await clickMenuItem(zhCN.models.retest)
    await vi.waitFor(() => expect(retestMock).toHaveBeenCalledWith(MODEL_ID, 2))

    // The unconfirmed capability tag retests the same way.
    const unknown = [...rowTr('gamma-model').querySelectorAll('.n-tag')].find(
      (el) => (el.textContent ?? '').trim() === '?',
    )
    expect(unknown, "the unconfirmed capability's '?' tag rendered").toBeTruthy()
    unknown!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(retestMock).toHaveBeenCalledWith(MODEL_ID, 3))

    // The delete confirm (the page's guard context) fires the DELETE with
    // the same pair.
    await openRowMenu('beta-model')
    await clickMenuItem(zhCN.models.deleteCandidate)
    const expected = zhCN.models.confirmDeleteCandidateContent.replace('{count}', '1')
    await vi.waitFor(() => expect(bodyText()).toContain(expected))
    dialogButton(zhCN.models.save).click()
    await vi.waitFor(() => expect(deleteMock).toHaveBeenCalledWith(MODEL_ID, 2))
  })
})
