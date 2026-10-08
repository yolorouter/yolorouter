// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the request-log source filter's playground arm
// (acceptance case TC-08, frontend half), in BOTH locales (zh-CN and en ship
// different copy, so each is asserted on the copy itself):
//
//   - list page: the Source dropdown offers caller / vision_fallback /
//     playground with localized labels, stays off the wire untouched, sends
//     source=playground with exactly the marker value on select, and drops
//     it again on clear
//   - detail page: a playground row badges as Playground, a vision-fallback
//     row as Vision fallback — the tag follows the stored source value, so
//     the marker stays readable end to end
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick, type ComponentPublicInstance } from 'vue'

import {
  getRequestLogDetail,
  listRequestLogs,
  type RequestLogDetail,
  type RequestLogPage,
} from '../../api/requestLogs'
import { listProviders } from '../../api/providers'
import { listAPIKeys } from '../../api/apiKeys'
import FilterSelectField from '../../components/common/FilterSelectField.vue'

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

// The marker the backend stores and the handler whitelist accepts
// (internal/model/request_log.go: RequestLogSourcePlayground).
const PLAYGROUND_MARKER = 'playground'

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

// Desktop viewport (same stub shape as the agent-client / trace-id tests):
// the list page takes the desktop NSelect branches, and useIsMobile (also
// used by FilterSelectField / ResponsiveDataTable) needs matchMedia.
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

// A full RequestLogDetail wire shape; `source` is the arm under test (""
// is what the backend serializes for a normal caller row).
function detailFixture(source: string): RequestLogDetail {
  return {
    request_id: 'req-src-1',
    api_key_id: 1,
    username: 'alice',
    model_name: 'gpt-demo',
    request_path: '/v1/chat/completions',
    source,
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
    created_at: '2026-10-08T00:00:00Z',
    w3c_trace_id: '',
    agent_client: '',
    agent_session_id: '',
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
    stream_body: '',
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

// The source FilterSelectField, located by its localized "all" placeholder —
// the one no other filter control shares (same locating shape as the
// agent-client test; the cast supplies just the props this helper reads).
type FilterSelectFieldProps = {
  placeholder?: string
  options?: Array<{ label: string; value: string }>
}
type FilterSelectFieldWrapper = VueWrapper<unknown, ComponentPublicInstance<FilterSelectFieldProps>>
function sourceField(locale: Locale): FilterSelectFieldWrapper {
  const fields = wrapper!.findAllComponents(FilterSelectField) as unknown as FilterSelectFieldWrapper[]
  const field = fields.find((f) => f.props('placeholder') === copy[locale].allFilterSource)
  expect(field, `source filter select rendered (placeholder "${copy[locale].allFilterSource}")`).toBeTruthy()
  return field!
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

describe.each<Locale>(['zh-CN', 'en'])('RequestLogListPage source filter (%s)', (locale) => {
  it('TC-08: offers caller, vision_fallback and playground with localized labels', async () => {
    await mountHost(ListHost, locale, '/')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(1))

    const options = sourceField(locale).props('options') as Array<{ label: string; value: string }>
    // The backend handler's accept set, in dropdown order — pinned so the
    // option list and the whitelist cannot drift apart silently.
    expect(options.map((o) => o.value)).toEqual(['caller', 'vision_fallback', PLAYGROUND_MARKER])
    expect(options.map((o) => o.label)).toEqual([
      copy[locale].sourceCaller,
      copy[locale].sourceVisionFallback,
      copy[locale].sourcePlayground,
    ])
    // The control itself rendered: the "all sources" placeholder copy is in
    // the DOM (desktop NSelect shows the placeholder while unset).
    expect(document.body.textContent ?? '').toContain(copy[locale].allFilterSource)
  })

  it('TC-08: stays off the wire untouched, sends source=playground on select, drops it on clear', async () => {
    await mountHost(ListHost, locale, '/')

    // Mount-time load: the untouched filter sends no source at all
    // (buildQuery drops empty values, so the backend's filter stays off).
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(1))
    expect(listMock.mock.calls[0][0], 'untouched filter keeps source off the wire').not.toHaveProperty('source')

    // Pick the playground option through the filter select (the emitted
    // update:value is what the dropdown's click handler produces); the
    // search fires and the marker value goes on the wire under source.
    sourceField(locale).vm.$emit('update:value', PLAYGROUND_MARKER)
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(2))
    expect(listMock.mock.calls[1][0].source, 'the marker value goes on the wire verbatim').toBe(PLAYGROUND_MARKER)

    // Clearing the select (the "all sources" row) drops the constraint
    // again — no lingering empty-string param.
    sourceField(locale).vm.$emit('update:value', null)
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(3))
    expect(listMock.mock.calls[2][0], 'cleared filter drops source off the wire').not.toHaveProperty('source')
  })
})

describe.each<Locale>(['zh-CN', 'en'])('RequestLogDetailPage source badge (%s)', (locale) => {
  it('TC-08: badges a playground row as Playground, not Vision fallback', async () => {
    detailMock.mockResolvedValue(detailFixture(PLAYGROUND_MARKER))
    await mountHost(DetailHost, locale, '/request-logs/req-src-1')

    await vi.waitFor(() => expect(detailMock).toHaveBeenCalledTimes(1))
    await nextTick()
    const text = document.body.textContent ?? ''
    expect(text, 'the localized request-source label appears').toContain(copy[locale].fieldSource)
    expect(text, 'a playground row wears the Playground badge').toContain(copy[locale].sourceBadgePlayground)
    expect(text, 'a playground row is NOT mislabeled as a vision-fallback sub-call').not.toContain(
      copy[locale].sourceBadge,
    )
  })

  it('TC-08: still badges a vision_fallback row as Vision fallback', async () => {
    detailMock.mockResolvedValue(detailFixture('vision_fallback'))
    await mountHost(DetailHost, locale, '/request-logs/req-src-1')

    await vi.waitFor(() => expect(detailMock).toHaveBeenCalledTimes(1))
    await nextTick()
    const text = document.body.textContent ?? ''
    expect(text, 'a sub-call row keeps its Vision fallback badge').toContain(copy[locale].sourceBadge)
    expect(text, 'a sub-call row does not wear the Playground badge').not.toContain(copy[locale].sourceBadgePlayground)
  })

  it('TC-08: renders no source field for a normal caller row', async () => {
    detailMock.mockResolvedValue(detailFixture(''))
    await mountHost(DetailHost, locale, '/request-logs/req-src-1')

    await vi.waitFor(() => expect(detailMock).toHaveBeenCalledTimes(1))
    await nextTick()
    const text = document.body.textContent ?? ''
    expect(text, 'no empty source field for plain caller rows').not.toContain(copy[locale].fieldSource)
  })
})
