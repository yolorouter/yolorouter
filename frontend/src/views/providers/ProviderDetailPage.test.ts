// @vitest-environment happy-dom
//
// The provider detail page's first mounted-DOM test suite — the regression
// baseline for the provider-page inline-actions batch. This batch CHANGES the
// models tab's actions column (edit/delete for every row) and adds search +
// filtering; the assertions below pin the two behaviors that must survive it
// untouched (TC-14):
//
//   - the existing retest conditional rendering: the actions cell offers a
//     retest button ONLY to rows without a passing verdict that the probe
//     queue does not already hold — passed rows and queued/probing rows get
//     none — with the verification badge each state renders
//   - the client-side pagination of the models tab: the table slices the
//     fully-fetched list and the pager turns pages over it
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

import { getProvider, type Provider, type ProviderKey } from '../../api/providers'
import { listProviderCandidates, type ProviderCandidate } from '../../api/models'

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return { ...actual, getProvider: vi.fn() }
})

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return { ...actual, listProviderCandidates: vi.fn(), retestCandidate: vi.fn() }
})

const getProviderMock = vi.mocked(getProvider)
const listCandidatesMock = vi.mocked(listProviderCandidates)

// Must be imported after the vi.mock factories are registered.
import ProviderDetailPage from './ProviderDetailPage.vue'
import zhCN from '../../locales/zh-CN'
import { CANDIDATE_STATUS_DISABLED, CANDIDATE_STATUS_ENABLED, VERIFICATION_FAILED, VERIFICATION_PASSED, VERIFICATION_UNTESTED } from '../../api/candidateStatus'

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

// Desktop viewport (same stub shape as the other page tests): useIsMobile
// needs matchMedia, and the desktop table branches are the ones asserted.
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
// background polling — those tests stay deterministic in their fetch counts).
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

async function mountPage(rows: ProviderCandidate[]) {
  getProviderMock.mockResolvedValue(providerFixture())
  listCandidatesMock.mockResolvedValue({ list: rows })
  const i18n = createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { 'zh-CN': zhCN } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      // The delete flow leaves for the list; resolving keeps router warnings
      // out of the output.
      { path: '/providers', component: { template: '<div />' } },
      { path: '/providers/:id', component: { template: '<div />' } },
    ],
  })
  await router.push(`/providers/${PROVIDER_ID}`)
  await router.isReady()
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  // Wait for the RENDERED page, not just the fetches: the keys table and
  // candidates flush a few ticks after the API calls resolve.
  await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(zhCN.providers.tabKeys))
  await nextTick()
}

// The models tab is NOT the landing tab; click its header like a user does.
async function openModelsTab() {
  const tab = [...document.body.querySelectorAll('.n-tabs-tab')].find((el) =>
    (el.textContent ?? '').includes(zhCN.providers.tabModels),
  )
  expect(tab, `the models tab header (${zhCN.providers.tabModels}) rendered`).toBeTruthy()
  tab!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await nextTick()
  await nextTick()
}

// The models-tab body row rendering a mapping, located by its model name
// (unique in every fixture here).
function rowTr(modelName: string): HTMLTableRowElement {
  const tr = [...document.body.querySelectorAll('tr')].find((el) => (el.textContent ?? '').includes(modelName))
  expect(tr, `row for ${modelName} rendered`).toBeTruthy()
  return tr!
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

describe('ProviderDetailPage baseline (TC-14)', () => {
  it('retest renders only for rows without a passing verdict that the queue does not hold', async () => {
    await mountPage([
      // Passed and enabled: no retest entry, "passed · enabled" badge.
      candidateRow({ candidate_id: 11, model_name: 'model-passed' }),
      // Failed (probed, verdict FAILED): the retest entry.
      candidateRow({
        candidate_id: 12,
        model_name: 'model-failed',
        management_status: CANDIDATE_STATUS_DISABLED,
        verification_status: VERIFICATION_FAILED,
        last_test_result: 7,
        last_test_error: 'upstream 500',
      }),
      // Held by the probe queue: its probe is already coming, no button.
      candidateRow({
        candidate_id: 13,
        model_name: 'model-queued',
        management_status: CANDIDATE_STATUS_DISABLED,
        verification_status: VERIFICATION_UNTESTED,
        last_test_result: null,
        last_tested_at: null,
        last_test_error: null,
        queue_state: 'queued',
        auto_enable_on_pass: true,
      }),
    ])
    await openModelsTab()

    const passedRow = rowTr('model-passed')
    expect(passedRow.textContent ?? '').toContain(zhCN.models.importStatePassed)
    expect(passedRow.querySelectorAll('button'), 'a passed row offers no retest').toHaveLength(0)

    const failedRow = rowTr('model-failed')
    expect(failedRow.textContent ?? '').toContain(zhCN.models.importStateFailed)
    expect(failedRow.textContent ?? '').toContain(zhCN.models.retest)
    expect(failedRow.textContent ?? '').toContain('upstream 500')

    const queuedRow = rowTr('model-queued')
    expect(queuedRow.textContent ?? '').toContain(zhCN.models.importStateQueued)
    expect(queuedRow.querySelectorAll('button'), 'a queue-held row offers no retest on top of its pending probe').toHaveLength(0)
  })

  it('client-side pagination: the models tab slices the fetched list and the pager turns pages', async () => {
    // 21 settled rows against the default 20-per-page: exactly two pages,
    // and nothing here arms the background poll (every row passed).
    const rows = Array.from({ length: 21 }, (_, i) =>
      candidateRow({
        candidate_id: i + 1,
        model_id: 100 + i,
        model_name: `model-${String(i + 1).padStart(2, '0')}`,
        provider_model_name: `upstream-${String(i + 1).padStart(2, '0')}`,
      }),
    )
    await mountPage(rows)
    // One fetch for the mount and NO background polling: settled rows are
    // not adopted (resumeQueuePollingFromLoadedRows finds nothing owed).
    await vi.waitFor(() => expect(listCandidatesMock).toHaveBeenCalledTimes(1))
    await openModelsTab()

    // Page 1: the first row is on screen, the 21st is not.
    expect(bodyText()).toContain('model-01')
    expect(bodyText()).not.toContain('model-21')

    const page2 = [...document.body.querySelectorAll('.n-pagination-item')].find(
      (el) => (el.textContent ?? '').trim() === '2',
    )
    expect(page2, 'the pager offers a second page').toBeTruthy()
    page2!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(bodyText()).toContain('model-21'))

    // The slice moved, not grew: page 2 shows only the overflow row.
    expect(bodyText()).not.toContain('model-01')
    expect(bodyText()).not.toContain('model-20')

    // The page turn stays client-side: re-assert the fetch count AFTER the
    // turn — the whole test must still have seen exactly one call. A pager
    // rewired to a reload (e.g. server-side pagination) turns this red
    // instead of surviving green on the mount-time count alone.
    expect(listCandidatesMock, 'turning the page must not refetch').toHaveBeenCalledTimes(1)
  })
})
