// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the agent-client attribution feature on the
// request-logs pages, in BOTH locales (zh-CN and en ship different copy, so
// each is asserted on the copy itself):
//
//   - TC-06 (list page): the Client Tool dropdown offers exactly the
//     recognizer's client enum (utils/agentClient.ts mirror), stays off the
//     wire untouched, and sends agent_client with exactly the selected enum
//     value — the exact-match param name — then drops it again on clear
//   - TC-06 (detail page): a log carrying agent attribution renders the
//     Client Tool row (localized display name) and the Tool Session ID row
//     (verbatim session value); a log without attribution renders neither
//     (no empty fields for non-agent callers)
//
// Fixtures speak the same identifiers the backend feature chain uses:
// claudeSessionID is the uuid from internal/router/agent_client_e2e_test.go,
// and 'claude-code' is the recognizer enum value for a claude-cli/ UA.
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
import { AGENT_CLIENTS } from '../../utils/agentClient'

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

// The same session uuid the backend's agent attribution e2e test uses
// (internal/router/agent_client_e2e_test.go: claudeSessionID).
const SESSION_ID = '0d51e7b2-9c40-4f8a-a6d3-1e5b7c9f2a48'
const REQUEST_ID = 'req-agent-1'

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

// Desktop viewport (same stub shape as the trace-id / SystemInfoPage
// tests): the list page takes the desktop NSelect branches, and
// useIsMobile (also used by FilterSelectField / ResponsiveDataTable) needs
// matchMedia.
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

// A full RequestLogDetail wire shape; the agent attribution pair is the arm
// under test ("" is what the backend serializes for NULL — non-agent
// callers and pre-column rows).
function detailFixture(agentClient: string, agentSessionId: string): RequestLogDetail {
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
    created_at: '2026-09-28T00:00:00Z',
    w3c_trace_id: '',
    agent_client: agentClient,
    agent_session_id: agentSessionId,
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

// The agent-client FilterSelectField, located by its localized "all" copy —
// the one placeholder no other filter control shares. findAllComponents
// always returns VueWrapper instances at runtime, but the generic
// component's inferred type degrades the array to DOMWrapper<Node>; the
// cast supplies just the two props this helper reads (props() is keyed off
// the instance's $props, hence the ComponentPublicInstance parameter).
type FilterSelectFieldProps = {
  placeholder?: string
  options?: Array<{ label: string; value: string }>
}
type FilterSelectFieldWrapper = VueWrapper<unknown, ComponentPublicInstance<FilterSelectFieldProps>>
function agentClientField(locale: Locale): FilterSelectFieldWrapper {
  const fields = wrapper!.findAllComponents(FilterSelectField) as unknown as FilterSelectFieldWrapper[]
  const field = fields.find((f) => f.props('placeholder') === copy[locale].allFilterAgentClient)
  expect(field, `agent-client filter select rendered (placeholder "${copy[locale].allFilterAgentClient}")`).toBeTruthy()
  return field!
}

beforeEach(() => {
  stubDesktopMatchMedia()
  listMock.mockResolvedValue(emptyPage())
  detailMock.mockResolvedValue(detailFixture('', ''))
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

describe.each<Locale>(['zh-CN', 'en'])('RequestLogDetailPage agent attribution fields (%s)', (locale) => {
  it('TC-06: renders Client Tool and Tool Session ID rows when the log carries attribution', async () => {
    detailMock.mockResolvedValue(detailFixture('claude-code', SESSION_ID))
    await mountHost(DetailHost, locale, `/request-logs/${REQUEST_ID}`)

    await vi.waitFor(() => expect(detailMock).toHaveBeenCalledTimes(1))
    await nextTick()
    const text = document.body.textContent ?? ''
    expect(text, 'the localized Client Tool label appears').toContain(copy[locale].fieldAgentClient)
    expect(text, 'the tool renders its display name, not the raw enum value').toContain(
      copy[locale].agentClientClaudeCode,
    )
    expect(text, 'the localized Tool Session ID label appears').toContain(copy[locale].fieldAgentSessionId)
    expect(text, 'the rendered session id equals the row value verbatim').toContain(SESSION_ID)
  })

  it('TC-06: renders neither field for a log without agent attribution', async () => {
    // "" is the backend's flattening of NULL — a non-agent caller (or a
    // pre-column row) carries no attribution at all.
    detailMock.mockResolvedValue(detailFixture('', ''))
    await mountHost(DetailHost, locale, `/request-logs/${REQUEST_ID}`)

    await vi.waitFor(() => expect(detailMock).toHaveBeenCalledTimes(1))
    await nextTick()
    const text = document.body.textContent ?? ''
    expect(text, 'no empty Client Tool field for non-agent requests').not.toContain(copy[locale].fieldAgentClient)
    expect(text, 'no empty Tool Session ID field for non-agent requests').not.toContain(
      copy[locale].fieldAgentSessionId,
    )
  })

  it('TC-06: renders the raw value for a client outside the enum (newer gateway)', async () => {
    // A row whose agent_client postdates this frontend's enum mirror must
    // still show the truth about the row rather than an empty field.
    detailMock.mockResolvedValue(detailFixture('some-new-tool', SESSION_ID))
    await mountHost(DetailHost, locale, `/request-logs/${REQUEST_ID}`)

    await vi.waitFor(() => expect(detailMock).toHaveBeenCalledTimes(1))
    await nextTick()
    const text = document.body.textContent ?? ''
    expect(text, 'the Client Tool label appears').toContain(copy[locale].fieldAgentClient)
    expect(text, 'an unknown enum value falls back to the raw value').toContain('some-new-tool')
  })
})

describe.each<Locale>(['zh-CN', 'en'])('RequestLogListPage agent-client filter (%s)', (locale) => {
  it('TC-06: offers exactly the recognizer client enum with localized labels', async () => {
    await mountHost(ListHost, locale, '/')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(1))

    const options = agentClientField(locale).props('options') as Array<{ label: string; value: string }>
    // The closed enum from the gateway recognizer, in filter-option order —
    // pinned against the shared mirror so dropdown and backend cannot
    // drift apart silently.
    expect(options.map((o) => o.value)).toEqual([...AGENT_CLIENTS])
    // Every option carries its localized label (the proper-noun copy both
    // locales share), never the raw i18n key.
    expect(options.map((o) => o.label)).toEqual([
      copy[locale].agentClientClaudeCode,
      copy[locale].agentClientCodex,
      copy[locale].agentClientOpencode,
      copy[locale].agentClientGeminiCli,
      copy[locale].agentClientQwenCode,
      copy[locale].agentClientCrush,
      copy[locale].agentClientCodewhale,
      copy[locale].agentClientCherryStudio,
      copy[locale].agentClientPi,
    ])
    // The control itself rendered: the "all client tools" placeholder copy
    // is in the DOM (desktop NSelect shows the placeholder while unset).
    expect(document.body.textContent ?? '').toContain(copy[locale].allFilterAgentClient)
  })

  it('TC-06: stays off the wire untouched, sends agent_client on select, drops it on clear', async () => {
    await mountHost(ListHost, locale, '/')

    // Mount-time load: the untouched filter sends no agent_client at all
    // (buildQuery drops empty values, so the backend's filter stays off).
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(1))
    expect(listMock.mock.calls[0][0], 'untouched filter keeps agent_client off the wire').not.toHaveProperty(
      'agent_client',
    )

    // Pick an option through the filter select (the emitted update:value
    // is what the dropdown's click handler produces); the search fires and
    // the exact enum value goes on the wire under the exact param name.
    agentClientField(locale).vm.$emit('update:value', 'claude-code')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(2))
    const selected = listMock.mock.calls[1][0]
    expect(selected.agent_client, 'exact param name carries the selected enum value').toBe('claude-code')

    // Clearing the select (the "all client tools" row) drops the
    // constraint again — no lingering empty-string param.
    agentClientField(locale).vm.$emit('update:value', null)
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(3))
    expect(listMock.mock.calls[2][0], 'cleared filter drops agent_client off the wire').not.toHaveProperty(
      'agent_client',
    )
  })
})
