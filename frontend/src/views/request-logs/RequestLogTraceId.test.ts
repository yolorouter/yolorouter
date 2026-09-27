// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the trace-id visibility feature on the request
// logs pages, in BOTH locales (zh-CN and en ship different copy, so each is
// asserted on the copy itself):
//
//   - TC-02 (detail page): a log carrying a W3C trace-id renders a "Trace ID"
//     row whose value equals the row's value; a log without one renders no
//     such row (no empty field for untraced requests)
//   - TC-03 (list page): typing a trace-id into the Trace ID filter sends
//     w3c_trace_id on the wire with exactly the trimmed typed value — the
//     exact-match param name, no fuzzy wildcard decoration — while an
//     untouched filter stays off the wire entirely
//
// The trace-id value is the W3C official example from the backend's
// traceparent e2e test (internal/router/traceparent_e2e_test.go), keeping
// the fixtures speaking the same identifier the whole feature chain uses.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import {
  getRequestLogDetail,
  listRequestLogs,
  type RequestLogDetail,
  type RequestLogPage,
} from '../../api/requestLogs'
import { listProviders } from '../../api/providers'
import { listAPIKeys } from '../../api/apiKeys'

vi.mock('../../api/requestLogs', () => ({
  listRequestLogs: vi.fn(),
  exportRequestLogsCSV: vi.fn(),
  getRequestLogDetail: vi.fn(),
  streamRequestLogBody: vi.fn(),
}))
vi.mock('../../api/providers', () => ({
  listProviders: vi.fn(),
}))
vi.mock('../../api/apiKeys', () => ({
  listAPIKeys: vi.fn(),
  toAPIKeyOptions: () => [],
}))
vi.mock('../../composables/useUserOptions', () => ({
  useUserOptions: () => ({ userOptions: [], loadUserOptions: vi.fn() }),
}))

const listMock = vi.mocked(listRequestLogs)
const detailMock = vi.mocked(getRequestLogDetail)
const providersMock = vi.mocked(listProviders)
const apiKeysMock = vi.mocked(listAPIKeys)

// Must be imported after the vi.mock factories are registered.
import RequestLogDetailPage from './RequestLogDetailPage.vue'
import RequestLogListPage from './RequestLogListPage.vue'
import en from '../../locales/en'
import zhCN from '../../locales/zh-CN'

// The W3C official example trace-id (traceparent_e2e_test.go: validTraceID).
const TRACE_ID = '4bf92f3577b34da6a3ce929d0e0e4736'
const REQUEST_ID = 'req-traced-1'

type Locale = 'en' | 'zh-CN'
const copy = { en: en.requestLogs, 'zh-CN': zhCN.requestLogs } as const

let wrapper: VueWrapper | null = null

// App.vue nests pages under n-config-provider > n-message-provider; the
// pages' useMessage() throws without that ancestry. Each page gets its own
// Host because the two pages need different router routes.
const DetailHost = defineComponent({
  components: { RequestLogDetailPage },
  template: '<NConfigProvider><NMessageProvider><RequestLogDetailPage /></NMessageProvider></NConfigProvider>',
})
const ListHost = defineComponent({
  components: { RequestLogListPage },
  template: '<NConfigProvider><NMessageProvider><RequestLogListPage /></NMessageProvider></NConfigProvider>',
})

// Desktop viewport (same stub shape as the SystemInfoPage tests): the list
// page takes the desktop datetimerange + NDataTable branches, and useIsMobile
// (also used by FilterSelectField / ResponsiveDataTable) needs matchMedia.
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

async function makeRouter(startPath: string) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/request-logs', component: { template: '<div />' } },
      { path: '/request-logs/:requestId', component: { template: '<div />' } },
    ],
  })
  await router.push(startPath)
  await router.isReady()
  return router
}

async function mountHost(host: ReturnType<typeof defineComponent>, locale: Locale, startPath: string) {
  const i18n = createI18n({ legacy: false, locale, fallbackLocale: 'zh-CN', messages: { en, 'zh-CN': zhCN } })
  const router = await makeRouter(startPath)
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  await nextTick()
  await nextTick()
}

// A full RequestLogDetail wire shape; `w3cTraceId` is the arm under test
// ("" is what the backend serializes for NULL — untraced or pre-column rows).
function detailFixture(w3cTraceId: string): RequestLogDetail {
  return {
    request_id: REQUEST_ID,
    api_key_id: 1,
    username: 'alice',
    model_name: 'gpt-demo',
    request_path: '/v1/chat/completions',
    source: '',
    parent_request_id: '',
    provider_id: 1,
    provider_name: 'demo-provider',
    is_stream: false,
    status_code: 200,
    status_class: 'success',
    input_tokens: 10,
    output_tokens: 20,
    cache_write_tokens: 0,
    cache_read_tokens: 0,
    image_count: 0,
    usage_seconds: 0,
    usage_characters: 0,
    image_unit_price: null,
    cost_micros: 1000,
    cost_known: true,
    fail_reason: null,
    attempts: 1,
    key_switches: 0,
    failovers: 0,
    final_provider_model: 'gpt-demo-upstream',
    duration_ms: 123,
    created_at: '2026-09-27T00:00:00Z',
    w3c_trace_id: w3cTraceId,
    usage_meter: '',
    attempts_detail: [],
    settled_input_price: null,
    settled_output_price: null,
    settled_cache_write_price: null,
    settled_cache_read_price: null,
    image_pricing_snapshot: '',
    upstream_url: 'https://upstream.example/v1/chat/completions',
    request_headers: '',
    request_body: '',
    upstream_request_body: '',
    response_body: '',
    upstream_response_body: '',
    stream_body_path: '',
    stream_body_truncated: false,
    has_stream_body: false,
    compress_estimated_tokens_saved: 0,
    compress_estimated_cost_saved_micros: 0,
    compress_skip_reason: '',
    compressors_applied: '',
    compressed_request_body: '',
  }
}

function emptyPage(): RequestLogPage {
  return { total: 0, page: 1, page_size: 20, list: [] }
}

// The trace-id filter input, located by its localized placeholder (the
// filter panel's inputs carry no dedicated classes).
function traceFilterInput(locale: Locale): HTMLInputElement {
  const input = [...document.body.querySelectorAll<HTMLInputElement>('input')].find(
    (i) => i.placeholder === copy[locale].filterTraceId,
  )
  expect(input, `trace-id filter input (placeholder "${copy[locale].filterTraceId}") rendered`).toBeTruthy()
  return input!
}

// NInput keeps its v-model in sync per keystroke, so a native input event
// with the new string drives the filter state exactly like typing does (the
// same driving shape as the GeneralSettingsPage tests).
function setInputValue(input: HTMLInputElement, value: string) {
  input.value = value
  input.dispatchEvent(new Event('input'))
}

beforeEach(() => {
  stubDesktopMatchMedia()
  listMock.mockResolvedValue(emptyPage())
  detailMock.mockResolvedValue(detailFixture(''))
  providersMock.mockResolvedValue({ list: [] })
  apiKeysMock.mockResolvedValue({ list: [], total: 0, page: 1, page_size: 200 })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe.each<Locale>(['zh-CN', 'en'])('RequestLogDetailPage trace-id field (%s)', (locale) => {
  it('TC-02: renders the Trace ID row with the row value when the log carries a trace-id', async () => {
    detailMock.mockResolvedValue(detailFixture(TRACE_ID))
    await mountHost(DetailHost, locale, `/request-logs/${REQUEST_ID}`)

    await vi.waitFor(() => expect(detailMock).toHaveBeenCalledTimes(1))
    await nextTick()
    const text = document.body.textContent ?? ''
    expect(text, 'the localized Trace ID label appears').toContain(copy[locale].fieldTraceId)
    expect(text, 'the rendered value equals the row value').toContain(TRACE_ID)
  })

  it('TC-02: renders no Trace ID row for a log without tracing context', async () => {
    // "" is the backend's flattening of NULL — no tracing context at all.
    detailMock.mockResolvedValue(detailFixture(''))
    await mountHost(DetailHost, locale, `/request-logs/${REQUEST_ID}`)

    await vi.waitFor(() => expect(detailMock).toHaveBeenCalledTimes(1))
    await nextTick()
    const text = document.body.textContent ?? ''
    expect(text, 'no empty Trace ID field for untraced requests').not.toContain(copy[locale].fieldTraceId)
  })
})

describe.each<Locale>(['zh-CN', 'en'])('RequestLogListPage trace-id filter (%s)', (locale) => {
  it('TC-03: stays off the wire untouched, then sends w3c_trace_id verbatim on typing', async () => {
    await mountHost(ListHost, locale, '/')

    // Mount-time load: the untouched filter sends no w3c_trace_id at all
    // (buildQuery drops empty strings, so the backend's filter stays off).
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(1))
    expect(listMock.mock.calls[0][0], 'untouched filter keeps w3c_trace_id off the wire').not.toHaveProperty(
      'w3c_trace_id',
    )

    // Type the trace-id (with stray whitespace, as a sloppy paste leaves)
    // and let the 300ms debounce fire the search.
    setInputValue(traceFilterInput(locale), ` ${TRACE_ID} `)
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(2), { timeout: 2_000 })

    const params = listMock.mock.calls[1][0]
    // Param name + exact-match semantics: the value is the trimmed input,
    // verbatim — no fuzzy wildcard decoration is added on the wire.
    expect(params.w3c_trace_id, 'exact param name carries the trimmed typed value').toBe(TRACE_ID)
    expect(params.w3c_trace_id, 'exact-match semantics: no wildcard characters decorate the value').not.toMatch(
      /[%*~]/,
    )
  })
})
