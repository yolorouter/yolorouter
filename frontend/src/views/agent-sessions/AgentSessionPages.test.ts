// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the tool-session pages (TC-04), in BOTH
// locales (zh-CN and en ship different copy, so each is asserted on the
// copy itself):
//
//   - list rendering: one row per session with the aggregate columns —
//     request + success counts, in/out token sums, the known-cost total,
//     the from/to first/last-seen doublet — and the "incl. N rows of
//     unknown cost" marker that appears ONLY on sessions carrying
//     unknown-cost rows (the known/unknown doublet's disclosure half)
//   - tool filter interaction: the dropdown offers exactly the recognizer
//     client enum (utils/agentClient.ts mirror), sends agent_client with
//     exactly the selected enum value, and drops it on clear
//   - detail drawer (session-detail UX TC-03): clicking a timeline row
//     opens the in-place request drawer INSTEAD of navigating; the row's
//     full detail (bodies) is fetched on demand only; the drawer's "view
//     full details" button hops to the existing /request-logs/:requestId
//     page — the interaction the pre-upgrade "row click jumps" case
//     covered, rewritten for the drawer
//   - detail column headers: each of the six data columns carries its
//     "?" help glyph (columnTitle/HelpLabel) with the column's own tip
//     as the glyph's aria-label
//   - page-top copy: the support-scope note names the three session-header
//     tools and points tools without a session header at the Log Audit
//     page (by its menu name)
//
// Fixtures reuse the identifiers/aggregate values the backend feature
// chain's own tests use (session A: 7 requests / 3 successes / 192+298
// tokens / 1660 known micros — the repository test's hand-computed row).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick, type ComponentPublicInstance } from 'vue'

import {
  getAgentSessionDetail,
  listAgentSessions,
  type AgentSessionDetail,
  type AgentSessionPage,
} from '../../api/agentSessions'
import { getRequestLogDetail, type RequestLogDetail } from '../../api/requestLogs'
import type { RequestLogRow } from '../../api/requestLogs'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import SessionRequestDrawer from '../../components/agent-sessions/SessionRequestDrawer.vue'
import { AGENT_CLIENTS } from '../../utils/agentClient'

vi.mock('../../api/agentSessions', () => ({
  listAgentSessions: vi.fn(),
  getAgentSessionDetail: vi.fn(),
}))

// The drawer fetches the request-log detail (bodies) itself; only that one
// export is needed at runtime.
vi.mock('../../api/requestLogs', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/requestLogs')>()
  return { ...actual, getRequestLogDetail: vi.fn() }
})

const listMock = vi.mocked(listAgentSessions)
const detailMock = vi.mocked(getAgentSessionDetail)
const logDetailMock = vi.mocked(getRequestLogDetail)

// Must be imported after the vi.mock factory is registered.
import AgentSessionListPage from './AgentSessionListPage.vue'
import AgentSessionDetailPage from './AgentSessionDetailPage.vue'
import en from '../../locales/en'
import zhCN from '../../locales/zh-CN'

const SESSION_A = '0d51e7b2-9c40-4f8a-a6d3-1e5b7c9f2a48'
const SESSION_B = 'b-session-0002'
const REQUEST_1 = 'req-sess-1'
const REQUEST_2 = 'req-sess-2'

type Locale = 'en' | 'zh-CN'
const copy = { en: en.agentSessions, 'zh-CN': zhCN.agentSessions } as const
const navCopy = { en: en.nav, 'zh-CN': zhCN.nav } as const

let wrapper: VueWrapper | null = null

// App.vue nests pages under n-config-provider > n-message-provider; the
// pages' useMessage() throws without that ancestry. Each page gets its own
// Host because the two pages mount at different routes.
const ListHost = defineComponent({
  components: { AgentSessionListPage },
  template: '<NConfigProvider><NMessageProvider><AgentSessionListPage /></NMessageProvider></NConfigProvider>',
})
const DetailHost = defineComponent({
  components: { AgentSessionDetailPage },
  template: '<NConfigProvider><NMessageProvider><AgentSessionDetailPage /></NMessageProvider></NConfigProvider>',
})

// Desktop viewport (same stub shape as the request-logs page tests): the
// list page takes the desktop NSelect branch, and useIsMobile (also used
// by FilterSelectField / ResponsiveDataTable) needs matchMedia.
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
      { path: '/agent-sessions', component: { template: '<div />' } },
      { path: '/agent-sessions/:sessionId', component: { template: '<div />' } },
      // The list page's pointer RouterLinks to the bare /request-logs, so
      // that path must resolve too — without it Vue Router warns about an
      // unmatched location on every list-page mount (same hygiene the
      // DefaultLayout test keeps for its shell links).
      { path: '/request-logs', component: { template: '<div />' } },
      { path: '/request-logs/:requestId', component: { template: '<div />' } },
    ],
  })
  await router.push(startPath)
  await router.isReady()
  return router
}

// The mounted pages' router (memory history). Held module-level so the
// detail-jump test can read currentRoute after the row click.
let currentRouter: Router | null = null

async function mountHost(host: ReturnType<typeof defineComponent>, locale: Locale, startPath: string) {
  const i18n = createI18n({ legacy: false, locale, fallbackLocale: 'zh-CN', messages: { en, 'zh-CN': zhCN } })
  currentRouter = await makeRouter(startPath)
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(host, { attachTo: document.body, global: { plugins: [pinia, currentRouter, i18n, naive] } })
  await nextTick()
  await nextTick()
}

// The two aggregate rows reuse the backend repository test's hand-computed
// request/success/token/known-cost figures, with ONE frontend-side
// deviation: A's unknown_cost_count is 2 here, not the backend matrix's 1
// (agent_session_query_test.go "want 1 (a5)") — a set value, not a reuse,
// so the rendered marker carries its own N. B is fully priced (no marker).
function listFixture(): AgentSessionPage {
  return {
    total: 2,
    page: 1,
    page_size: 20,
    list: [
      {
        agent_session_id: SESSION_A,
        agent_client: 'claude-code',
        request_count: 7,
        success_count: 3,
        input_tokens: 192,
        output_tokens: 298,
        known_cost_micros: 1660,
        unknown_cost_count: 2,
        first_seen_at: '2026-09-28T08:00:00Z',
        last_seen_at: '2026-09-28T09:30:00Z',
      },
      {
        agent_session_id: SESSION_B,
        agent_client: 'codex',
        request_count: 3,
        success_count: 2,
        input_tokens: 1011,
        output_tokens: 2002,
        known_cost_micros: 5003,
        unknown_cost_count: 0,
        first_seen_at: '2026-09-27T10:00:00Z',
        last_seen_at: '2026-09-27T10:05:00Z',
      },
    ],
  }
}

// One timeline row of the session detail, in the RequestLogRow wire shape
// the detail endpoint reuses from the request-log list.
function timelineRow(requestId: string, model: string, status: RequestLogRow['status_class'], createdAt: string): RequestLogRow {
  return {
    request_id: requestId,
    api_key_id: 1,
    username: 'alice',
    model_name: model,
    request_path: '/v1/chat/completions',
    source: '',
    parent_request_id: '',
    provider_id: 1,
    provider_name: 'demo-provider',
    is_stream: false,
    status_code: 200,
    status_class: status,
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
    final_provider_model: `${model}-upstream`,
    duration_ms: 123,
    created_at: createdAt,
  }
}

function detailFixture(): AgentSessionDetail {
  return {
    agent_session_id: SESSION_A,
    agent_client: 'claude-code',
    requests: [
      timelineRow(REQUEST_1, 'model-one', 'success', '2026-09-28T08:00:00Z'),
      // The second row is a stream call (its reply lives in the SSE
      // capture the drawer's on-demand fetch inlines as stream_body).
      { ...timelineRow(REQUEST_2, 'model-two', 'failed', '2026-09-28T08:30:00Z'), is_stream: true },
    ],
  }
}

// The full request-log detail the drawer's on-demand fetch resolves to.
// REQUEST_1 is a plain non-stream call (inline response body); REQUEST_2 is
// a stream call — response_body empty, the reply living in the SSE capture
// the backend inlines as stream_body.
function logDetailFixture(base: RequestLogRow, over: Partial<RequestLogDetail>): RequestLogDetail {
  return {
    ...base,
    w3c_trace_id: '',
    agent_client: 'claude-code',
    agent_session_id: SESSION_A,
    usage_meter: '',
    attempts_detail: [],
    settled_input_price: null,
    settled_output_price: null,
    settled_cache_write_price: null,
    settled_cache_read_price: null,
    image_pricing_snapshot: '',
    upstream_url: '',
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
    ...over,
  }
}

const CHAT_REQUEST_BODY = JSON.stringify({
  model: 'model-two',
  messages: [{ role: 'user', content: '抽屉里的用户提问' }],
})
const OPENAI_STREAM_CAPTURE = [
  'data: {"choices":[{"delta":{"role":"assistant","content":"流式"}}]}',
  '',
  'data: {"choices":[{"delta":{"content":"回复已合并"}}]}',
  '',
  'data: [DONE]',
  '',
].join('\n')

// The agent-client FilterSelectField, located by its localized "all"
// placeholder (the one control this page's filter bar carries).
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

// The rendered form of a template message with {n} replaced by a number —
// for counting how often the unknown-cost marker appears in the DOM.
function markerRegex(locale: Locale): RegExp {
  return new RegExp(copy[locale].costUnknownNote.replace('{n}', '\\d+'))
}

// The table's short locale timestamp, computed with the same options the
// shared formatShortClock (utils/format.ts) — which both agent-session
// pages render their timestamps through — uses, so the expected strings
// track whatever locale/timezone this process runs under instead of
// pinning one ICU output. What the assertion pins is WHICH fixture
// timestamp lands under WHICH label — not the formatting itself.
function shortTime(iso: string): string {
  return new Date(iso).toLocaleString(undefined, {
    year: '2-digit',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}

beforeEach(() => {
  stubDesktopMatchMedia()
  listMock.mockResolvedValue(listFixture())
  detailMock.mockResolvedValue(detailFixture())
  logDetailMock.mockImplementation(async (requestId) => {
    const [row] = detailFixture().requests.filter((r) => r.request_id === requestId)
    if (requestId === REQUEST_2) {
      // Stream call: empty inline response, reply merged from the capture.
      return logDetailFixture({ ...row, is_stream: true }, {
        request_body: CHAT_REQUEST_BODY,
        response_body: '',
        stream_body: OPENAI_STREAM_CAPTURE,
        has_stream_body: true,
      })
    }
    return logDetailFixture(row, {
      request_body: JSON.stringify({ model: 'model-one', messages: [{ role: 'user', content: '第一条请求的提问' }] }),
      response_body: JSON.stringify({ choices: [{ message: { role: 'assistant', content: '第一条请求的回复' } }] }),
    })
  })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  currentRouter = null
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe.each<Locale>(['zh-CN', 'en'])('AgentSessionListPage aggregates and filter (%s)', (locale) => {
  it('TC-04: renders one row per session with the aggregate columns and the unknown-cost marker only where it applies', async () => {
    await mountHost(ListHost, locale, '/agent-sessions')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(1))
    await nextTick()

    const text = document.body.textContent ?? ''
    // Aggregate values straight from the fixture: request + success counts…
    expect(text, 'session A request count').toContain('7')
    expect(text, 'session A success sub-line').toContain(copy[locale].successCountLine.replace('{n}', '3'))
    expect(text, 'session B success sub-line').toContain(copy[locale].successCountLine.replace('{n}', '2'))
    // …token sums…
    expect(text, 'session A input token sum').toContain('192')
    expect(text, 'session A output token sum').toContain('298')
    expect(text, 'session B input token sum').toContain('1011')
    // …known-cost totals (micros -> major unit, 6dp — formatMicros).
    expect(text, 'session A known-cost total').toContain('0.001660')
    expect(text, 'session B known-cost total').toContain('0.005003')
    // …and the from/to time doublet: each row renders its OWN first and
    // last seen under the right label. A cell wired to the wrong field
    // (say, both lines reading last_seen_at) or rows swapping values
    // fails here — the fixture's four timestamps are pairwise distinct.
    const rowOf = (id: string) => [...document.body.querySelectorAll('tr')].find((tr) => (tr.textContent ?? '').includes(id))
    for (const [id, first, last] of [
      [SESSION_A, '2026-09-28T08:00:00Z', '2026-09-28T09:30:00Z'],
      [SESSION_B, '2026-09-27T10:00:00Z', '2026-09-27T10:05:00Z'],
    ] as const) {
      const rowText = rowOf(id)?.textContent ?? ''
      expect(rowText, `${id} renders its own first_seen_at under the from label`).toContain(
        copy[locale].firstSeenLabel + shortTime(first),
      )
      expect(rowText, `${id} renders its own last_seen_at under the to label`).toContain(
        copy[locale].lastSeenLabel + shortTime(last),
      )
    }
    // Both session ids and both tool display names are in the table.
    expect(text).toContain(SESSION_A)
    expect(text).toContain(SESSION_B)
    expect(text, 'claude-code renders its display name').toContain('Claude Code')
    expect(text, 'codex renders its display name').toContain('Codex')

    // The unknown-cost marker: rendered exactly once (session A, n=2)…
    expect(text, 'session A unknown-cost marker with its row count').toContain(
      copy[locale].costUnknownNote.replace('{n}', '2'),
    )
    const markers = text.match(new RegExp(markerRegex(locale), 'g')) ?? []
    expect(markers.length, 'only the session with unknown-cost rows carries the marker').toBe(1)
  })

  it('TC-04: offers the recognizer enum, sends agent_client on select, drops it on clear', async () => {
    await mountHost(ListHost, locale, '/agent-sessions')

    // Mount-time load: the untouched filter sends page 1 / page_size 20 and
    // no agent_client at all (the cleared dropdown means "all tools").
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(1))
    expect(listMock.mock.calls[0][0], 'default pagination envelope').toMatchObject({ page: 1, page_size: 20 })
    expect(listMock.mock.calls[0][0], 'untouched filter keeps agent_client off the wire').not.toHaveProperty('agent_client')

    // The closed enum from the gateway recognizer, in filter-option order.
    const options = agentClientField(locale).props('options') as Array<{ label: string; value: string }>
    expect(options.map((o) => o.value)).toEqual([...AGENT_CLIENTS])

    // Select an option: the exact enum value goes on the wire under the
    // exact param name, restarting from page 1.
    agentClientField(locale).vm.$emit('update:value', 'claude-code')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(2))
    const selected = listMock.mock.calls[1][0]
    expect(selected.agent_client, 'exact param name carries the selected enum value').toBe('claude-code')
    expect(selected.page, 'a filter change restarts at page 1').toBe(1)

    // Clearing the select drops the constraint again.
    agentClientField(locale).vm.$emit('update:value', null)
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(3))
    expect(listMock.mock.calls[2][0], 'cleared filter drops agent_client off the wire').not.toHaveProperty('agent_client')
  })
})

describe.each<Locale>(['zh-CN', 'en'])('AgentSessionListPage page-top copy (%s)', (locale) => {
  it('TC-04: states the three-tool support scope and points at Log Audit by its menu name', async () => {
    await mountHost(ListHost, locale, '/agent-sessions')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(1))
    await nextTick()

    const text = document.body.textContent ?? ''
    // Support scope: names the three session-header tools verbatim.
    expect(text, 'scope note names the three tools').toContain('Claude Code / Codex / OpenCode')
    // Pointer: the sentence around the link plus the menu-name link text.
    expect(text).toContain(copy[locale].pointerPre)
    expect(text, 'the link text is the nav menu name').toContain(navCopy[locale].logAudit)
    expect(text).toContain(copy[locale].pointerPost)
    // The link really points at the log-audit page.
    const link = document.body.querySelector('a[href="/request-logs"]')
    expect(link, 'pointer link navigates to /request-logs').toBeTruthy()
  })
})

describe.each<Locale>(['zh-CN', 'en'])('AgentSessionDetailPage timeline and drawer (%s)', (locale) => {
  it('session-detail UX TC-03: row click opens the in-place drawer (no navigation), bodies load on demand, and the drawer hops to /request-logs/:requestId', async () => {
    await mountHost(DetailHost, locale, `/agent-sessions/${SESSION_A}`)
    await vi.waitFor(() => expect(detailMock).toHaveBeenCalledTimes(1))
    await nextTick()

    const text = document.body.textContent ?? ''
    // The timeline carries both requests in fixture (chronological) order,
    // with their five-class status tags rendered, plus the summary card's
    // aggregates (both locales render every new piece's copy — parity).
    expect(text).toContain('model-one')
    expect(text).toContain('model-two')
    expect(text, 'success row renders its five-class status tag').toContain(
      locale === 'zh-CN' ? zhCN.requestLogs.status_success : en.requestLogs.status_success,
    )
    expect(text, 'failed row renders its five-class status tag').toContain(locale === 'zh-CN' ? zhCN.requestLogs.status_failed : en.requestLogs.status_failed)
    expect(text, 'summary card renders its success sub-line').toContain(copy[locale].successCountLine.replace('{n}', '1'))

    // Every timeline column header carries its "?" help glyph, and the
    // glyph's aria-label is that column's own tip — the six columns now
    // include the waterfall (the glyph is located by role/aria-label
    // because NIcon renders it as a plain <i>, and its a11y attributes are
    // the only machine-readable trace of the tip).
    const helpTips = [...document.body.querySelectorAll('th [role="img"]')].map((el) => el.getAttribute('aria-label'))
    expect(helpTips, 'six column headers, six help glyphs, each with its own tip').toEqual([
      copy[locale].col_created_tip,
      copy[locale].col_model_tip,
      copy[locale].col_status_tip,
      copy[locale].detail_col_tokens_tip,
      copy[locale].col_duration_tip,
      copy[locale].col_waterfall_tip,
    ])

    // On-demand loading: no request-log detail fetch before any row click.
    expect(logDetailMock, 'no detail fetch before a row click').not.toHaveBeenCalled()

    // Click the second row (located by its model name; a MouseEvent so the
    // row's left-button guard reads a real button=0 like a browser click):
    // the drawer opens IN PLACE — the SPA does not navigate — and fetches
    // exactly that row's detail.
    const rowTwo = [...document.body.querySelectorAll('tr')].find((tr) => (tr.textContent ?? '').includes('model-two'))
    expect(rowTwo, 'the model-two timeline row rendered').toBeTruthy()
    rowTwo!.dispatchEvent(new MouseEvent('click', { bubbles: true, button: 0 }))
    await vi.waitFor(() => expect(logDetailMock, 'drawer fetches the clicked row on demand').toHaveBeenCalledTimes(1))
    expect(logDetailMock.mock.calls[0][0]).toBe(REQUEST_2)
    expect(currentRouter!.currentRoute.value.path, 'row click stays on the session page').toBe(`/agent-sessions/${SESSION_A}`)

    // Drawer content: the row's identity, the translated request bubble,
    // and — this row being a stream call — the reply merged from the SSE
    // capture into one readable bubble.
    await vi.waitFor(() => expect(document.body.textContent, 'drawer shows the request id').toContain(REQUEST_2))
    const drawerText = document.body.textContent ?? ''
    expect(drawerText, 'request-side bubble text').toContain('抽屉里的用户提问')
    expect(drawerText, 'stream reply merged to full text').toContain('流式回复已合并')
    expect(drawerText, 'stream-merge note on the response section').toContain(copy[locale].drawerStreamMergedNote)

    // The drawer's "view full details" button navigates to the existing
    // request-detail page with that row's id (the pre-upgrade row-click
    // destination, now one hop deeper).
    const viewFull = [...document.body.querySelectorAll('button')].find((b) => (b.textContent ?? '').includes(copy[locale].drawerViewFull))
    expect(viewFull, 'the view-full-details button rendered').toBeTruthy()
    viewFull!.click()
    await vi.waitFor(() =>
      expect(currentRouter!.currentRoute.value.path, 'drawer hop opens the request detail for that row').toBe(
        `/request-logs/${REQUEST_2}`,
      ),
    )

    // Open/close: after the hop closed it, clicking the first row reopens
    // the drawer for THAT row (a fresh on-demand fetch), and the drawer's
    // mask click closes it again. Closed-state assertions read the drawer
    // component's show prop: under the test transition stub the slide-out
    // animation's after-leave never fires, so the rendered shell lingers
    // in the DOM — the v-model state is the honest open/close signal.
    const drawerComp = wrapper!.findComponent(SessionRequestDrawer)
    expect(drawerComp.exists(), 'the drawer component is part of the page').toBe(true)
    await vi.waitFor(() => expect(drawerComp.props('show'), 'the hop closed the drawer').toBe(false))
    const rowOne = [...document.body.querySelectorAll('tr')].find((tr) => (tr.textContent ?? '').includes('model-one'))
    rowOne!.dispatchEvent(new MouseEvent('click', { bubbles: true, button: 0 }))
    await vi.waitFor(() => {
      expect(drawerComp.props('show'), 'the other row reopens the drawer').toBe(true)
      expect(logDetailMock, 'reopening fetches the newly clicked row').toHaveBeenCalledTimes(2)
      expect(logDetailMock.mock.calls[1][0]).toBe(REQUEST_1)
    })
    await vi.waitFor(() => expect(document.body.textContent, 'second drawer shows its own reply').toContain('第一条请求的回复'))
    document.body.querySelector('.n-drawer-mask')!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(drawerComp.props('show'), 'mask click closes the drawer').toBe(false))
  })
})
