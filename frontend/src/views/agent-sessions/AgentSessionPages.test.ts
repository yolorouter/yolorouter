// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the tool-session pages, in BOTH locales (zh-CN
// and en ship different copy, so each is asserted on the copy itself):
//
//   list page (unchanged surface, regression-pinned):
//   - list rendering: one row per session with the aggregate columns —
//     request + success counts, in/out token sums, the known-cost total,
//     the from/to first/last-seen doublet — and the "incl. N rows of
//     unknown cost" marker that appears ONLY on sessions carrying
//     unknown-cost rows (the known/unknown doublet's disclosure half)
//   - tool filter interaction: the dropdown offers exactly the recognizer
//     client enum (utils/agentClient.ts mirror), sends agent_client with
//     exactly the selected enum value, and drops it on clear
//   - page-top copy: the support-scope note names the three supported
//     tools and states the two session-ID routes (live header capture
//     plus the startup backfill), and points tools with no session
//     identifier at the Log Audit page (by its menu name)
//
//   detail page (the session replay — assembly under the cursor-paginated
//   contract; the lazy-paging behavior itself has its own file,
//   AgentSessionDetailPage.lazyload.test.ts):
//   - assembly: one fetch renders the replay's first page — the two-line
//     info bar (tool / session id / request count / from-to clock +
//     elapsed span; tokens in-out / known cost + unknown marker / success
//     ratio), one system-prompt row per request block (the request's own
//     plainly "System Prompt" (no first-seen/change-count phrasing; the
//     historical versions live inside the expansion as secondary entries,
//     each annotated with its request id and truncation mark), and the
//     per-request turns in chronological order with their metadata signs,
//     failure status tags, and request-detail links
//   - conversation model grading: only a human input renders a user
//     bubble; a tool result renders as a compact card whose tool name
//     comes from the CROSS-TURN correlation (a previous turn's streamed
//     tool_use id → name); machine-injected context (system reminders,
//     command echoes) collapses behind one "injected context (N sections)"
//     line; an all-thinking+tool_use stream turn renders a non-empty
//     reply — collapsed thinking disclosure + a call card — never the
//     bare empty bubble the old merge painted
//   - assistant source arms: inline / stream (ONE merged bubble plus the
//     merged-note) / resend / none (the explicit "not recorded"
//     placeholder, never an empty bubble)
//   - fetch discipline: the transcript API is called exactly once (no
//     polling, no per-request follow-up)
//   - states: 14006 renders the not-found empty state with the back
//     button; a transport failure renders the localized copy inline
//   - anchor: a #req-<requestId> hash makes the FIRST fetch anchor at
//     that turn (the inclusive after cursor) and scrolls it into view via
//     scrollIntoView on the block element (the app shell scrolls inside
//     layout containers, never the window); no hash opens on the latest
//     page and lands the viewport on the newest turn
//   - text rendering: markdown, the long-part clamp, image placeholders,
//     and tool_use labels with their argument-detail block ride the shared
//     bubble renderer unchanged
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
  getAgentSessionTranscript,
  getAgentSessionTranscriptSystem,
  listAgentSessions,
  type AgentSessionPage,
  type AgentSessionTranscript,
  type AgentSessionTranscriptSystem,
} from '../../api/agentSessions'
import { APIError, NetworkError } from '../../api/client'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import { AGENT_CLIENTS } from '../../utils/agentClient'

vi.mock('../../api/agentSessions', () => ({
  listAgentSessions: vi.fn(),
  getAgentSessionTranscript: vi.fn(),
  getAgentSessionTranscriptSystem: vi.fn(),
}))

const listMock = vi.mocked(listAgentSessions)
const transcriptMock = vi.mocked(getAgentSessionTranscript)
const transcriptSystemMock = vi.mocked(getAgentSessionTranscriptSystem)

// Must be imported after the vi.mock factory is registered.
import AgentSessionListPage from './AgentSessionListPage.vue'
import AgentSessionDetailPage from './AgentSessionDetailPage.vue'
import en from '../../locales/en'
import zhCN from '../../locales/zh-CN'

const SESSION_A = '0d51e7b2-9c40-4f8a-a6d3-1e5b7c9f2a48'
const SESSION_B = 'b-session-0002'
const REQUEST_1 = 'req-sess-1'
const REQUEST_2 = 'req-sess-2'
const REQUEST_3 = 'req-sess-3'
const REQUEST_4 = 'req-sess-4'

type Locale = 'en' | 'zh-CN'
const copy = { en: en.agentSessions, 'zh-CN': zhCN.agentSessions } as const
const logsCopy = { en: en.requestLogs, 'zh-CN': zhCN.requestLogs } as const

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
      // The replay turns' "request details" links point at the request-log
      // detail route; it must resolve or Vue Router warns on every click.
      { path: '/request-logs', component: { template: '<div />' } },
      { path: '/request-logs/:requestId', component: { template: '<div />' } },
    ],
  })
  await router.push(startPath)
  await router.isReady()
  return router
}

// The mounted pages' router (memory history). Held module-level so the
// navigation tests can read currentRoute after a click.
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

// An OpenAI SSE capture exactly as the on-disk .stream file holds it —
// the stream-sourced turn's raw.
const STREAM_CAPTURE = [
  'data: {"choices":[{"delta":{"role":"assistant","content":"Hello"}}]}',
  '',
  'data: {"choices":[{"delta":{"content":" streamed"}}]}',
  '',
  'data: {"choices":[{"delta":{"content":" reply"}}]}',
  '',
  'data: [DONE]',
  '',
].join('\n')

// The whole-session replay fixture: four turns covering the assistant
// source arms (inline / stream / resend / none), a failed turn, a
// truncated-request turn, and a two-entry system timeline (first value
// plus one change). It models a session that fits one page — both
// has_more flags false — so this file's assembly assertions run without
// any paging chrome. The session aggregate reuses the list fixture's
// session-A figures with request_count lowered to the four blocks that
// exist here, so the info bar's numbers and the turns agree.
function transcriptFixture(): AgentSessionTranscript {
  return {
    session: {
      agent_session_id: SESSION_A,
      agent_client: 'claude-code',
      request_count: 4,
      success_count: 3,
      input_tokens: 192,
      output_tokens: 298,
      known_cost_micros: 1660,
      unknown_cost_count: 2,
      first_seen_at: '2026-09-28T08:00:00Z',
      last_seen_at: '2026-09-28T09:30:00Z',
    },
    // Metadata-only entries — the raw prompt text is the separate
    // on-expand fetch (systemRawFor below), index-aligned with this
    // list.
    system: [
      { seen_at_request: REQUEST_1, truncated: false, raw_length: 28 },
      // The changed value arrives truncated, so the timeline entry's
      // truncation mark gets its rendering assertion too.
      { seen_at_request: REQUEST_3, truncated: true, raw_length: 34 },
    ],
    blocks: [
      {
        // Inline reply turn: OpenAI string content in, recorded response
        // body out — the reply carries a tool call whose id the LATER
        // turn's tool result correlates (the cross-block name index).
        request_id: REQUEST_1,
        created_at: '2026-09-28T08:00:00Z',
        model_name: 'model-one',
        status_class: 'success',
        is_stream: false,
        input_tokens: 10,
        output_tokens: 20,
        duration_ms: 123,
        provider_name: 'demo-provider',
        added: [{ role: 'user', content: '第一轮问题' }],
        assistant: {
          source: 'inline',
          truncated: false,
          raw: JSON.stringify({
            content: [
              { type: 'text', text: '第一轮回复' },
              { type: 'tool_use', id: 'tu-1', name: 'Bash', input: { command: 'ls -la' } },
            ],
          }),
        },
        added_truncated: false,
        system: { seen_at_request: REQUEST_1, truncated: false, raw_length: 28 },
      },
      {
        // The failed turn: Anthropic blocks in (text + image), the reply
        // only exists as the SSE capture.
        request_id: REQUEST_2,
        created_at: '2026-09-28T08:30:00Z',
        model_name: 'model-two',
        status_class: 'failed',
        is_stream: true,
        input_tokens: 30,
        output_tokens: 40,
        duration_ms: 2500,
        provider_name: 'demo-provider',
        added: [{ role: 'user', content: [{ type: 'text', text: '看看这张图' }, { type: 'image' }] }],
        assistant: { source: 'stream', truncated: false, raw: STREAM_CAPTURE },
        added_truncated: false,
        // Same value as the first request: inherits its timeline entry.
        system: { seen_at_request: REQUEST_1, truncated: false, raw_length: 28 },
      },
      {
        // Unrecorded reply lifted from the next request's resent copy; the
        // turn itself contributed a tool result.
        request_id: REQUEST_3,
        created_at: '2026-09-28T09:00:00Z',
        model_name: 'model-three',
        status_class: 'success',
        is_stream: false,
        input_tokens: 50,
        output_tokens: 60,
        duration_ms: 999,
        provider_name: 'demo-provider',
        added: [{ role: 'user', content: [{ type: 'tool_result', tool_use_id: 'tu-1', content: 'file list output' }] }],
        assistant: { source: 'resend', truncated: false, raw: [{ role: 'assistant', content: '重发的回复' }] },
        added_truncated: false,
        // The changed (truncated) value's own entry.
        system: { seen_at_request: REQUEST_3, truncated: true, raw_length: 34 },
      },
      {
        // Capped request body: added degrades to the empty array and the
        // block keeps its metadata plus the truncation notice; nothing was
        // recorded for the reply either.
        request_id: REQUEST_4,
        created_at: '2026-09-28T09:30:00Z',
        model_name: 'model-four',
        status_class: 'success',
        is_stream: false,
        input_tokens: 102,
        output_tokens: 178,
        duration_ms: 61000,
        provider_name: 'demo-provider',
        added: [],
        assistant: { source: 'none', truncated: false, raw: null },
        added_truncated: true,
        system: { seen_at_request: REQUEST_3, truncated: true, raw_length: 34 },
      },
    ],
    has_more_before: false,
    next_before: '',
    has_more_after: false,
    next_after: '',
  }
}

// The per-request system raw fetch's answer: the timeline entry in effect
// at the named request — the first value (recorded at REQUEST_1) for
// REQUEST_1/2, the changed truncated value (recorded at REQUEST_3) for
// REQUEST_3/4.
function systemRawFor(requestId: string): AgentSessionTranscriptSystem {
  if (requestId === REQUEST_1 || requestId === REQUEST_2) {
    return { system: [{ seen_at_request: REQUEST_1, truncated: false, raw: 'You are a helpful assistant.' }] }
  }
  return { system: [{ seen_at_request: REQUEST_3, truncated: true, raw: [{ type: 'text', text: 'Revised system prompt.' }] }] }
}

beforeEach(() => {
  stubDesktopMatchMedia()
  listMock.mockResolvedValue(listFixture())
  transcriptMock.mockResolvedValue(transcriptFixture())
  transcriptSystemMock.mockImplementation(async (_sid: string, opts?: { request?: string }) => systemRawFor(opts?.request ?? ''))
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  currentRouter = null
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

// ---------------------------------------------------------------------------
// List page (unchanged surface — regression pins)
// ---------------------------------------------------------------------------

describe.each<Locale>(['zh-CN', 'en'])('AgentSessionListPage aggregates and filter (%s)', (locale) => {
  it('renders one row per session with the aggregate columns and the unknown-cost marker only where it applies', async () => {
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

  it('offers the recognizer enum, sends agent_client on select, drops it on clear', async () => {
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
  it('states the three-tool support scope and the two routes in the description, with no banner above the filters', async () => {
    await mountHost(ListHost, locale, '/agent-sessions')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(1))
    await nextTick()

    const text = document.body.textContent ?? ''
    // Support scope: names the three supported tools verbatim — folded
    // into the one-sentence page description (the page-top shape every
    // other list page uses).
    expect(text, 'description names the three tools').toContain('Claude Code / Codex / OpenCode')
    // Two-route sourcing: the copy must keep stating BOTH ways a session
    // ID arrives — live header capture and the startup backfill (the
    // Codex route; the pre-backfill copy claimed header-only collection,
    // which the backfill pass made false).
    const scopeFragments: Record<Locale, string[]> = {
      'zh-CN': ['实时请求头', '启动回填'],
      en: ['session-header', 'backfill'],
    }
    for (const fragment of scopeFragments[locale]) {
      expect(text, `description keeps the two-route sourcing (${fragment})`).toContain(fragment)
    }
    // The old support-scope banner is gone for good — the page top must
    // read like every other list page: header, then filter panel, no
    // alert strip in between.
    expect(document.body.querySelector('.n-alert, .scope-note'), 'no alert banner above the filters').toBeNull()
  })
})

// ---------------------------------------------------------------------------
// Detail page — the session replay
// ---------------------------------------------------------------------------

// Mount the replay page and settle its one fetch. Returns once the
// transcript mock has been called AND a render pass has flushed.
async function mountReplay(locale: Locale, startPath = `/agent-sessions/${SESSION_A}`) {
  await mountHost(DetailHost, locale, startPath)
  await vi.waitFor(() => expect(transcriptMock).toHaveBeenCalledTimes(1))
  await nextTick()
}

// One replay turn, located by its anchor id (the block root element).
function blockEl(requestId: string): HTMLElement | null {
  return document.getElementById(`req-${requestId}`)
}

describe.each<Locale>(['zh-CN', 'en'])('AgentSessionDetailPage replay assembly (%s)', (locale) => {
  it('renders the two-line info bar with the session aggregate (list-row semantics)', async () => {
    await mountReplay(locale)

    const lines = [...document.body.querySelectorAll('.replay-meta__line')]
    expect(lines.length, 'info bar is exactly two lines').toBe(2)

    // Line 1: tool display name, the verbatim session id, the request
    // count, and the from/to clock doublet with the elapsed span
    // (08:00 -> 09:30 = 5,400,000ms = "1h 30m 0s" through formatSpan).
    const line1 = lines[0].textContent ?? ''
    expect(line1, 'tool display name').toContain('Claude Code')
    expect(line1, 'verbatim session id').toContain(SESSION_A)
    expect(line1, 'request count with its unit').toContain(`4 ${copy[locale].detailRequestsUnit}`)
    expect(line1, 'first seen clock under the from label').toContain(copy[locale].firstSeenLabel + ' ' + shortTime('2026-09-28T08:00:00Z'))
    expect(line1, 'last seen clock under the to label').toContain(copy[locale].lastSeenLabel + ' ' + shortTime('2026-09-28T09:30:00Z'))
    const elapsed = copy[locale].replayElapsed.replace('{span}', '1h 30m 0s')
    expect(line1, 'elapsed span between first and last request').toContain(elapsed)

    // Line 2: token sums, the known-cost total plus the unknown marker,
    // and the success ratio — the same figures the list row shows.
    const line2 = lines[1].textContent ?? ''
    expect(line2, 'session token in/out sums').toContain(`${logsCopy[locale].tokenRowIn} 192 / ${logsCopy[locale].tokenRowOut} 298`)
    expect(line2, 'known-cost total (micros -> major unit, 6dp)').toContain('0.001660')
    expect(line2, 'unknown-cost marker with its row count').toContain(copy[locale].costUnknownNote.replace('{n}', '2'))
    expect(line2, 'success ratio').toContain(copy[locale].replaySuccessRatio.replace('{success}', '3').replace('{total}', '4'))
  })

  it("renders one system-prompt row per request block — collapsed by default, expanding fetches that request's raw text once", async () => {
    await mountReplay(locale)

    // One quiet row per block, directly under the block header (the
    // metadata sign), labeled with the plain "System Prompt" — no
    // "first seen + N changes" phrasing anywhere, no per-request
    // annotations, no prompt text. Collapsed also means the raw-text
    // endpoint has not been touched at all.
    const rows = [...document.body.querySelectorAll<HTMLElement>('[data-testid="replay-system-row"]')]
    expect(rows.length, 'one system row per request block').toBe(4)
    for (const row of rows) {
      expect(row.textContent ?? '', 'the row label is the plain "System Prompt"').toContain(copy[locale].systemPromptLabel)
    }
    // Each row sits right under its block's own metadata sign (the block
    // header), before the conversation axis starts.
    const turn1 = blockEl(REQUEST_1)!
    const meta1 = turn1.querySelector('.replay-block__meta')
    const row1 = turn1.querySelector('[data-testid="replay-system-row"]')
    expect(meta1 && row1, 'the turn carries both header and system row').toBeTruthy()
    expect(row1!.previousElementSibling, 'the system row follows the header line').toBe(meta1)
    // The retired phrasings are gone ENTIRELY — not rendered with another
    // number, not in another locale's leftover (regex-escaped patterns;
    // the zh copy carried '·' and '+', the en copy a literal '+').
    const pageText = document.body.textContent ?? ''
    expect(pageText, 'no "first seen" phrasing survives').not.toMatch(/首次出现|first seen/)
    expect(pageText, 'no change-count phrasing survives').not.toMatch(/变化\s*\d+\s*次|\+\s*\d+\s+changes?/)
    expect(pageText, 'system prompt hidden while collapsed').not.toContain('You are a helpful assistant.')
    expect(transcriptSystemMock, 'no raw-text fetch while the rows are collapsed').not.toHaveBeenCalled()

    // Expanding one request's row fetches exactly that request's raw text
    // (the second request inherits the first's value; the endpoint answers
    // the value in effect at the named request) and renders it through the
    // shared bubble renderer.
    const toggleOf = (id: string) =>
      blockEl(id)!.querySelector<HTMLButtonElement>('[data-testid="replay-system-row"] button')!
    toggleOf(REQUEST_2).dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(transcriptSystemMock, "expanding fetches the request's raw text once").toHaveBeenCalledTimes(1))
    expect(transcriptSystemMock.mock.calls[0][1], 'the fetch names the expanded request').toEqual({ request: REQUEST_2 })
    await vi.waitFor(() =>
      expect(blockEl(REQUEST_2)?.textContent ?? '', 'the inherited value renders').toContain('You are a helpful assistant.'),
    )

    // Expanding the FIRST request's row — same timeline entry, unchanged
    // value — reuses the same fetch: one entry, one fetch, however many
    // blocks resolve to it.
    toggleOf(REQUEST_1).dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() =>
      expect(blockEl(REQUEST_1)?.textContent ?? '', 'the same value renders on its own block').toContain('You are a helpful assistant.'),
    )
    expect(transcriptSystemMock, 'blocks sharing one timeline entry share one fetch').toHaveBeenCalledTimes(1)

    // The changed value fetches under its own request and carries the
    // truncation mark (the fixture records that entry as truncated).
    toggleOf(REQUEST_3).dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(transcriptSystemMock).toHaveBeenCalledTimes(2))
    expect(transcriptSystemMock.mock.calls[1][1], 'the changed value fetches under its own request').toEqual({ request: REQUEST_3 })
    await vi.waitFor(() => {
      const turn3 = blockEl(REQUEST_3)!
      expect(turn3.textContent ?? '', 'the revised prompt renders').toContain('Revised system prompt.')
      expect(turn3.textContent ?? '', 'the truncated entry carries the truncation mark').toContain(copy[locale].flowTruncated)
    })
    expect(blockEl(REQUEST_1)?.textContent ?? '', 'the untruncated entry carries no truncation mark').not.toContain(copy[locale].flowTruncated)
  })

  it('keeps the plain row label whatever the timeline shape — a never-changed prompt renders the same collapsed line on every block', async () => {
    // Same fixture with every block resolving to the FIRST timeline entry
    // (the change never happened): every row is the same collapsed line,
    // and however many rows the reader opens, one entry means one fetch.
    const constant = transcriptFixture()
    constant.system = [constant.system[0]]
    for (const b of constant.blocks) {
      b.system = { seen_at_request: REQUEST_1, truncated: false, raw_length: 28 }
    }
    transcriptMock.mockResolvedValueOnce(constant)
    await mountReplay(locale)

    const rows = document.body.querySelectorAll('[data-testid="replay-system-row"]')
    expect(rows.length, 'one row per block still').toBe(4)
    for (const row of rows) {
      expect(row.textContent ?? '', 'the same plain label').toContain(copy[locale].systemPromptLabel)
    }
    const toggles = [...document.body.querySelectorAll<HTMLButtonElement>('[data-testid="replay-system-row"] button')]
    toggles[0].dispatchEvent(new MouseEvent('click', { bubbles: true }))
    toggles[3].dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(transcriptSystemMock, 'one timeline entry, one fetch — however many rows open').toHaveBeenCalledTimes(1))
    expect(document.body.textContent ?? '', 'still no change-count phrasing').not.toMatch(/变化|changes/)
  })

  it('renders the turns in chronological order with metadata signs, failure tags, and request-detail links', async () => {
    await mountReplay(locale)

    // One anchored block per request, in fixture (wire) order: each
    // turn's own text appears, and earlier turns precede later ones in
    // the DOM.
    for (const id of [REQUEST_1, REQUEST_2, REQUEST_3, REQUEST_4]) {
      expect(blockEl(id), `block root carries the req-<id> anchor`).toBeTruthy()
    }
    const text = document.body.textContent ?? ''
    const order = ['第一轮问题', '看看这张图', 'file list output'].map((s) => text.indexOf(s))
    expect(order.every((i) => i >= 0), 'each turn contributed its own text').toBe(true)
    expect([...order].sort((a, b) => a - b), 'turns render in chronological order').toEqual(order)

    // Metadata sign per turn: model, duration, in/out tokens.
    const turn1 = blockEl(REQUEST_1)!
    const meta1 = turn1.querySelector('.replay-block__meta')?.textContent ?? ''
    expect(meta1, 'model name on the sign').toContain('model-one')
    expect(meta1, 'sub-second duration').toContain('123ms')
    expect(meta1, 'per-turn token in/out').toContain(`${logsCopy[locale].tokenRowIn} 10 / ${logsCopy[locale].tokenRowOut} 20`)

    // The failed turn carries its five-class status tag AND its content —
    // a failure must read as failed, never silently disappear; the
    // success turns carry no tag.
    const turn2 = blockEl(REQUEST_2)!
    expect(turn2.querySelector('.n-tag')?.textContent, 'failed turn shows the failed status tag').toContain(
      logsCopy[locale].status_failed,
    )
    expect(turn2.textContent ?? '', 'failed turn keeps its contributed messages').toContain('看看这张图')
    expect(blockEl(REQUEST_1)!.querySelector('.n-tag'), 'success turn carries no status tag').toBeNull()

    // Every turn offers the request-details hop, and it lands on that
    // request's log page.
    const link1 = turn1.querySelector('.replay-block__meta button')
    expect(link1?.textContent, 'request-details link copy').toContain(copy[locale].replayRequestLink)
    link1!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(currentRouter!.currentRoute.value.path, 'the hop targets this request log').toBe(`/request-logs/${REQUEST_1}`))
  })

  it('routes the assistant halves by source: inline bubbles, stream merges into one bubble with the merged-note, resend bubbles, none renders the placeholder', async () => {
    await mountReplay(locale)

    // Inline: the recorded response body becomes the assistant bubble.
    expect(blockEl(REQUEST_1)?.textContent ?? '', 'inline reply text').toContain('第一轮回复')

    // Stream: the SSE capture merges into exactly ONE assistant bubble
    // carrying the full text, plus the merged-from-chunks note.
    const turn2 = blockEl(REQUEST_2)!
    const rightRows = turn2.querySelectorAll('.msg-flow__row--right')
    expect(rightRows.length, 'stream capture merges into one assistant bubble').toBe(1)
    expect(rightRows[0].textContent ?? '', 'merged full reply text').toContain('Hello streamed reply')
    expect(turn2.querySelector('.replay-block__mergednote')?.textContent, 'the merged-from-chunks note').toContain(
      copy[locale].streamMergedNote,
    )

    // Resend: the resent assistant copy renders as the reply.
    expect(blockEl(REQUEST_3)?.textContent ?? '', 'resend copy renders as the reply').toContain('重发的回复')

    // None: the explicit "not recorded" placeholder — the shared copy the
    // request-log pages use — and NO conversation bubble, plus the
    // capped-input notice for this turn.
    const turn4 = blockEl(REQUEST_4)!
    expect(turn4.querySelector('.msg-flow__note')?.textContent, 'not-recorded placeholder copy').toContain(
      logsCopy[locale].bodyNotRecorded,
    )
    expect(turn4.querySelectorAll('.msg-flow__row').length, 'no fake empty bubble for an unrecorded reply').toBe(0)
    expect(turn4.textContent ?? '', 'capped request body shows the truncation notice').toContain(copy[locale].flowTruncated)
  })
})

// ---------------------------------------------------------------------------
// Detail page — the graded conversation model (owner 2026-10-10 rework)
// ---------------------------------------------------------------------------

// A miniature of the REAL all-machine-product turn the rework targets
// (the replay fixtures' thinking-then-tool_use stream captures, no text
// delta at all — the shape that used to merge to a bare empty bubble):
// thinking fragments plus a tool_use reassembled from its input_json_delta
// pieces.
const THINKING_TOOL_CAPTURE = [
  'event: message_start',
  'data: {"type":"message_start","message":{"role":"assistant","content":[]}}',
  'event: content_block_start',
  'data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}',
  'data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Check the listing first."}}',
  'event: content_block_start',
  'data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}}}',
  'data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\\"comm"}}',
  'data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"and\\": \\"ls -la\\"}"}}',
  'data: {"type":"content_block_stop","index":0}',
  'data: {"type":"content_block_stop","index":1}',
  'data: {"type":"message_stop"}',
  '',
].join('\n')

function conversationModelFixture(): AgentSessionTranscript {
  return {
    session: {
      agent_session_id: SESSION_A,
      agent_client: 'claude-code',
      request_count: 2,
      success_count: 2,
      input_tokens: 40,
      output_tokens: 80,
      known_cost_micros: 0,
      unknown_cost_count: 0,
      first_seen_at: '2026-09-28T08:00:00Z',
      last_seen_at: '2026-09-28T08:10:00Z',
    },
    system: [],
    blocks: [
      {
        // The assistant turn that THINKS and CALLS — stream capture with
        // no text delta at all (the four regression blocks' shape).
        request_id: 'req-conv-1',
        created_at: '2026-09-28T08:00:00Z',
        model_name: 'model-conv',
        status_class: 'success',
        is_stream: true,
        input_tokens: 20,
        output_tokens: 40,
        duration_ms: 500,
        provider_name: 'demo-provider',
        added: [{ role: 'user', content: '列一下目录' }],
        assistant: { source: 'stream', truncated: false, raw: THINKING_TOOL_CAPTURE },
        added_truncated: false,
      },
      {
        // The follow-up turn: the tool's result, two machine injections,
        // then the human's actual follow-up question.
        request_id: 'req-conv-2',
        created_at: '2026-09-28T08:10:00Z',
        model_name: 'model-conv',
        status_class: 'success',
        is_stream: false,
        input_tokens: 20,
        output_tokens: 40,
        duration_ms: 300,
        provider_name: 'demo-provider',
        added: [
          { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'toolu_01', content: [{ type: 'text', text: 'total 0\nexit 0' }] }] },
          { role: 'user', content: '<system-reminder>\nCalled the Bash tool with the listing above.' },
          { role: 'user', content: '<command-name>/compact</command-name>' },
          { role: 'user', content: '人类追问：下一步呢' },
        ],
        assistant: { source: 'none', truncated: false, raw: null },
        added_truncated: false,
      },
    ],
    has_more_before: false,
    next_before: '',
    has_more_after: false,
    next_after: '',
  }
}

describe.each<Locale>(['zh-CN', 'en'])('AgentSessionDetailPage graded conversation model (%s)', (locale) => {
  beforeEach(() => {
    transcriptMock.mockResolvedValue(conversationModelFixture())
  })

  it('an all-thinking+tool_use stream turn renders a NON-empty reply: collapsed thinking + a call card, never a bare bubble', async () => {
    await mountReplay(locale)
    const turn = blockEl('req-conv-1')!

    // Exactly one assistant bubble, and it is NOT empty: its content is
    // the thinking disclosure plus the call card (the old merge dropped
    // both and painted a bare bubble).
    const rightRows = turn.querySelectorAll('.msg-flow__row--right')
    expect(rightRows.length, 'one assistant bubble for the streamed reply').toBe(1)
    const bubble = rightRows[0]
    expect(bubble.querySelector('.msg-flow__thinking-toggle'), 'thinking disclosure inside the reply').toBeTruthy()
    expect(bubble.querySelector('.msg-flow__thinking-label')?.textContent, 'thinking label copy').toContain(
      copy[locale].thinkingBlock,
    )
    expect(bubble.textContent ?? '', 'thinking text hidden while collapsed').not.toContain('Check the listing first.')

    // Expanding the disclosure reveals the assembled reasoning text.
    bubble.querySelector('.msg-flow__thinking-toggle')?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() =>
      expect(bubble.querySelector('.msg-flow__thinking-body')?.textContent, 'expanded thinking text').toContain(
        'Check the listing first.',
      ),
    )

    // The tool call: a compact card with the tool name, args collapsed.
    const callCard = bubble.querySelector('.toolcard')
    expect(callCard, 'the streamed tool_use renders as a call card').toBeTruthy()
    expect(callCard?.querySelector('.toolcard__name')?.textContent, 'card names the tool').toContain('Bash')
    expect(callCard?.querySelector('.toolcard__detail'), 'arguments collapsed by default').toBeNull()
    callCard?.querySelector('.toolcard__head')?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() =>
      expect(callCard?.querySelector('.toolcard__detail')?.textContent, 'expanded arguments carry the reassembled JSON').toContain(
        '"ls -la"',
      ),
    )
  })

  it('tool results render as compact cards with the correlated tool name — never user bubbles; injected context collapses off the axis', async () => {
    await mountReplay(locale)
    const turn = blockEl('req-conv-2')!

    // The conversation axis of this turn carries exactly ONE user bubble:
    // the human's follow-up. The tool result and the injected context
    // never appear as user bubbles.
    const leftRows = turn.querySelectorAll('.msg-flow__row--left')
    expect(leftRows.length, 'only the human input renders a user bubble').toBe(1)
    expect(leftRows[0].textContent, 'the bubble is the human follow-up').toContain('人类追问：下一步呢')

    // The tool result: a compact card, named by the CROSS-TURN correlation
    // (the previous turn's streamed tool_use id → name), with the output's
    // first line as the preview and the full output behind the toggle.
    const cards = turn.querySelectorAll('.toolcard')
    expect(cards.length, 'one result card').toBe(1)
    const card = cards[0]
    expect(card.querySelector('.toolcard__kind')?.textContent, 'result card kind label').toContain(copy[locale].toolResult)
    expect(card.querySelector('.toolcard__name')?.textContent, 'result card carries the correlated tool name').toContain('Bash')
    expect(card.querySelector('.toolcard__preview')?.textContent, 'first-line preview on the header').toContain('total 0')
    expect(card.querySelector('.toolcard__detail'), 'output collapsed by default').toBeNull()
    card.querySelector('.toolcard__head')?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(card.querySelector('.toolcard__detail')?.textContent, 'expanded output').toContain('exit 0'))

    // The injected run: one quiet line naming BOTH sections, their text
    // hidden until the line expands. (The line's exact copy is spelled out
    // per locale — the zh copy counts sections, the en copy carries the
    // bare count.)
    const injectedLine = turn.querySelector('.replay-injected')
    expect(injectedLine, 'the injected run renders its collapse line').toBeTruthy()
    const expectedInjectedLine = locale === 'zh-CN' ? '已注入上下文（2 段）' : 'Injected context (2)'
    expect(injectedLine?.querySelector('.replay-injected__label')?.textContent, 'the line counts its sections').toContain(
      expectedInjectedLine,
    )
    const turnText = () => turn.textContent ?? ''
    expect(turnText(), 'injected content hidden while collapsed').not.toContain('Called the Bash tool')
    injectedLine?.querySelector('.replay-injected__toggle')?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => {
      expect(turnText(), 'expanded run shows the first section').toContain('Called the Bash tool')
      expect(turnText(), 'expanded run shows the second section').toContain('/compact')
    })
  })
})

describe('AgentSessionDetailPage fetch discipline', () => {
  it('calls the transcript API exactly once — no polling, no per-request follow-up', async () => {
    await mountReplay('zh-CN')
    // Settle every pending microtask/tick the mount could have queued;
    // a poller or per-block fetch would have fired by now.
    for (let i = 0; i < 5; i++) await nextTick()
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(transcriptMock, 'exactly one transcript fetch for the whole page').toHaveBeenCalledTimes(1)
    expect(transcriptMock.mock.calls[0][0], 'the fetch targeted the URL session id').toBe(SESSION_A)
  })
})

describe.each<Locale>(['zh-CN', 'en'])('AgentSessionDetailPage states (%s)', (locale) => {
  it('14006 renders the not-found state with the back button', async () => {
    transcriptMock.mockRejectedValueOnce(new APIError(14006))
    await mountHost(DetailHost, locale, `/agent-sessions/${SESSION_A}`)
    await vi.waitFor(() => expect(transcriptMock).toHaveBeenCalledTimes(1))
    await nextTick()

    const text = document.body.textContent ?? ''
    expect(text, 'localized not-found copy').toContain(copy[locale].notFound)
    // The page survives: the back button is offered and returns to the
    // session list.
    const back = [...document.body.querySelectorAll('button')].find((b) => b.textContent?.includes(copy[locale].backToList))
    expect(back, 'back button present on the not-found state').toBeTruthy()
    back!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(currentRouter!.currentRoute.value.path, 'back returns to the session list').toBe('/agent-sessions'))
  })

  it('a transport failure keeps the page and shows the localized copy inline — never the raw error string', async () => {
    transcriptMock.mockRejectedValueOnce(new NetworkError())
    await mountHost(DetailHost, locale, `/agent-sessions/${SESSION_A}`)
    await vi.waitFor(() => expect(transcriptMock).toHaveBeenCalledTimes(1))
    await nextTick()

    const text = document.body.textContent ?? ''
    expect(text, 'localized load-failed title').toContain(copy[locale].loadFailed)
    expect(text, 'localized network-error fallback').toContain(locale === 'zh-CN' ? zhCN.common.networkError : en.common.networkError)
    expect(text, 'no raw technical error string leaks').not.toContain('NetworkError')
    expect(
      [...document.body.querySelectorAll('button')].find((b) => b.textContent?.includes(copy[locale].backToList)),
      'back button present on the error state',
    ).toBeTruthy()
  })
})

describe('AgentSessionDetailPage anchor scrolling', () => {
  // happy-dom defines no scrollIntoView to spy on, so the tests install a
  // recording stub on the prototype and take it back afterwards — restore
  // must delete rather than assign, or it would shadow the chain with
  // undefined and poison later tests.
  function stubScrollIntoView(): { spy: ReturnType<typeof vi.fn>; restore: () => void } {
    const proto = HTMLElement.prototype as { scrollIntoView?: (...args: unknown[]) => void }
    const hadOwn = Object.prototype.hasOwnProperty.call(proto, 'scrollIntoView')
    const original = proto.scrollIntoView
    const spy = vi.fn()
    proto.scrollIntoView = spy
    return {
      spy,
      restore: () => {
        if (hadOwn) proto.scrollIntoView = original
        else delete proto.scrollIntoView
      },
    }
  }

  it('a #req-<id> hash anchors the first fetch at that turn and scrolls it into view via scrollIntoView on the block element — not the window', async () => {
    const { spy, restore } = stubScrollIntoView()
    const windowScroll = vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
    try {
      await mountReplay('zh-CN', `/agent-sessions/${SESSION_A}#req-${REQUEST_2}`)
      await vi.waitFor(() => expect(spy, 'the target turn was scrolled to').toHaveBeenCalled())

      // The first fetch anchored AT the deep-linked turn: the inclusive
      // after cursor, never a bare latest-page fetch.
      expect(transcriptMock, 'exactly one fetch for the anchored entry').toHaveBeenCalledTimes(1)
      expect(transcriptMock.mock.calls[0][0], 'the fetch targeted the URL session id').toBe(SESSION_A)
      expect(transcriptMock.mock.calls[0][1], 'the fetch anchored at the hashed turn (inclusive after cursor)').toEqual({ after: REQUEST_2 })

      // The call rides the block element itself — scrollIntoView walks the
      // real scrollable ancestors (the app shell's layout container);
      // window.scrollTo never happens.
      expect(spy.mock.contexts[0], 'scrollIntoView was called ON the req-<id> block element').toBe(blockEl(REQUEST_2))
      expect(spy.mock.calls[0][0], 'scroll alignment option').toEqual({ block: 'start' })
      expect(windowScroll, 'the window is never scrolled directly').not.toHaveBeenCalled()
    } finally {
      windowScroll.mockRestore()
      restore()
    }
  })

  it('no hash opens on the latest page and lands the viewport on the newest turn', async () => {
    const { spy, restore } = stubScrollIntoView()
    try {
      await mountReplay('zh-CN')
      await vi.waitFor(() => expect(spy, 'the newest turn was scrolled to').toHaveBeenCalled())
      await new Promise((resolve) => setTimeout(resolve, 20))

      // The latest-page opening: a bare fetch (no cursor params at all)…
      expect(transcriptMock, 'exactly one fetch').toHaveBeenCalledTimes(1)
      expect(transcriptMock.mock.calls[0][1], 'no cursor params on the latest-page opening').toBeUndefined()
      // …and the viewport lands on the LAST turn, bottom-aligned — the
      // resume-a-session reading position.
      expect(spy.mock.contexts[0], 'scrollIntoView was called ON the newest turn').toBe(blockEl(REQUEST_4))
      expect(spy.mock.calls[0][0], 'bottom alignment for the latest-page landing').toEqual({ block: 'end' })
      expect(spy, 'the landing scrolls exactly once').toHaveBeenCalledTimes(1)
    } finally {
      restore()
    }
  })
})

describe('AgentSessionDetailPage text rendering', () => {
  it('markdown, the long-part clamp, image placeholders, and tool_use labels+argument detail ride the shared bubble renderer', async () => {
    const long = Array.from({ length: 500 }, (_, k) => `第 ${k} 段：这是一段用于超过折叠阈值的长文本。`).join('\n\n')
    expect(long.length, 'fixture actually crosses the threshold').toBeGreaterThan(3500)
    transcriptMock.mockResolvedValueOnce({
      session: {
        agent_session_id: SESSION_B,
        agent_client: 'codex',
        request_count: 2,
        success_count: 2,
        input_tokens: 10,
        output_tokens: 20,
        known_cost_micros: 0,
        unknown_cost_count: 0,
        first_seen_at: '2026-09-27T10:00:00Z',
        last_seen_at: '2026-09-27T10:05:00Z',
      },
      system: [],
      blocks: [
        {
          request_id: 'req-text-1',
          created_at: '2026-09-27T10:00:00Z',
          model_name: 'model-text',
          status_class: 'success',
          is_stream: false,
          input_tokens: 5,
          output_tokens: 5,
          duration_ms: 100,
          provider_name: 'demo-provider',
          // Markdown source + an image part + the long wall in the added
          // half (user side).
          added: [
            { role: 'user', content: [{ type: 'text', text: '# 回放标题\n\n**加粗**与 `code` 及围栏：\n\n```go\nfunc main() {}\n```' }, { type: 'image' }] },
            { role: 'user', content: [{ type: 'text', text: long }] },
          ],
          assistant: {
            // A resent assistant copy carrying a tool_use block exercises
            // the reply half's non-text arm.
            source: 'resend',
            truncated: false,
            raw: [{ role: 'assistant', content: [{ type: 'tool_use', id: 'tu-9', name: 'Bash', input: { command: 'ls' } }] }],
          },
          added_truncated: false,
        },
      ],
      has_more_before: false,
      next_before: '',
      has_more_after: false,
      next_after: '',
    })
    await mountReplay('zh-CN')

    const turn = blockEl('req-text-1')!
    // Markdown went through the pipeline: strong/code elements exist, the
    // fenced block is highlighted, and NO verbatim markdown marker
    // survives anywhere in the turn. (happy-dom's DOMPurify fragment
    // parsing unwraps ATX headings to bare text, so no h1 assertion is
    // possible here — but inline/blocked structure and the hljs markup
    // pin the same pipeline.)
    const mdEl = turn.querySelector('.msg-flow__text')
    expect(mdEl?.querySelector('strong')?.textContent, 'markdown bold rendered').toContain('加粗')
    expect(mdEl?.querySelector('p code')?.textContent, 'markdown inline code rendered').toContain('code')
    const fenced = mdEl?.querySelector('pre code')
    expect(fenced?.getAttribute('class'), 'fenced go block kept its language').toContain('language-go')
    expect(fenced?.innerHTML, 'fenced block went through highlight.js').toContain('hljs-keyword')
    const turnText = turn.textContent ?? ''
    expect(turnText, 'no verbatim bold marker left').not.toContain('**加粗**')
    expect(turnText, 'no verbatim fence left').not.toContain('```go')
    expect(turnText, 'the heading text itself rendered').toContain('回放标题')
    // Image part: the placeholder block, never the payload.
    expect(turn.querySelector('.msg-flow__image')?.textContent, 'image placeholder block').toContain(zhCN.agentSessions.partImage)
    // tool_use part: a compact CALL card — the kind label + the tool
    // name on the header, payload hidden until expanded...
    const callCard = turn.querySelector('.toolcard')
    expect(callCard, 'tool_use renders as a call card').toBeTruthy()
    expect(callCard?.querySelector('.toolcard__kind')?.textContent, 'call card kind label').toContain(zhCN.agentSessions.toolCall)
    expect(callCard?.querySelector('.toolcard__name')?.textContent, 'call card carries the tool name').toContain('Bash')
    expect(callCard?.querySelector('.toolcard__detail'), 'call payload collapsed by default').toBeNull()
    // ...and its parameter detail on expand: the translator lifts
    // input:{command:'ls'} into part.detail as pretty-printed JSON with
    // detailLang 'json', and the card renders it as the mono detail block
    // — highlighted through the same shared hljs core the fenced blocks
    // use (attr token = the parameter name, string token = the argument
    // value), so a broken detail arm fails here, visibly.
    callCard?.querySelector('.toolcard__head')?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(callCard?.querySelector('.toolcard__detail'), 'expanded call card shows its payload').toBeTruthy())
    const toolDetailEl = callCard?.querySelector('.toolcard__detail') ?? null
    expect(toolDetailEl?.textContent, 'tool_use detail carries the parameter name').toContain('"command"')
    expect(toolDetailEl?.textContent, 'tool_use detail carries the argument value').toContain('"ls"')
    const toolDetailCode = toolDetailEl?.querySelector('code.toolcard__detail-code')
    expect(toolDetailCode?.innerHTML, 'json arguments went through highlight.js (attr token)').toContain('hljs-attr')
    expect(toolDetailCode?.innerHTML, 'json arguments went through highlight.js (string token)').toContain('hljs-string')
    // Long part: clamped by default behind the disclosure row; clicking
    // expands it and flips the toggle to collapse.
    const textEls = [...turn.querySelectorAll('.msg-flow__text')]
    const longEl = textEls.find((el) => el.textContent?.includes('第 499 段')) ?? textEls[textEls.length - 1]
    expect(longEl?.classList.contains('msg-flow__text--clamped'), 'the wall starts clamped').toBe(true)
    const btn = turn.querySelector('.msg-flow__disclose-btn') as HTMLButtonElement | null
    expect(btn?.textContent, 'disclosure row offers expand').toContain(zhCN.agentSessions.expandFull)
    btn?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() =>
      expect(longEl?.classList.contains('msg-flow__text--clamped'), 'expanded clears the clamp').toBe(false),
    )
    expect(turn.querySelector('.msg-flow__disclose-btn')?.textContent, 'toggle flips to collapse').toContain(
      zhCN.agentSessions.collapse,
    )
  })
})
