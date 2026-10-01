// @vitest-environment jsdom
//
// frontend/src/views/request-messages/RequestMessagePage.test.ts
//
// Mounted-DOM coverage for the request message page — the full-page
// replacement of the old session-detail request drawer:
//
//   TC-02 rendering      both wire formats (an Anthropic request with a
//                        system prompt, an OpenAI response), every text
//                        bubble through the markdown pipeline (system and
//                        tool roles included), a highlighted fenced code
//                        block, the image/tool part arms, and all eight
//                        key facts
//   TC-03 navigation     the back button's two arms (browser history back
//                        when a previous entry exists; the session-list
//                        fallback on a fresh deep link), the view-full
//                        hop to /request-logs/:requestId, and the deep
//                        link itself rendering fully usable
//   TC-04 regression     the two regression locks the drawer carried: a
//                        failed detail fetch surfaces the localized copy
//                        and never the raw error string; the response
//                        side's three-way branch (inline body / SSE
//                        capture — merged back into one assistant bubble
//                        with the section marked as merged, or handed raw
//                        to the fallback view when unmergeable / neither
//                        recorded)
//
// jsdom, not happy-dom: the bubbles render through renderMarkdown →
// DOMPurify, and happy-dom's Node.prototype.nodeName base getter breaks
// DOMPurify's whitelist (legal block tags unwrapped, scripts surviving).
// jsdom parses nodeName the browser way, so the DOM assertions exercise
// the true sanitized pipeline.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import { getRequestLogDetail, type RequestLogDetail, type RequestLogRow, type StatusClass } from '../../api/requestLogs'
import { NetworkError } from '../../api/client'
import en from '../../locales/en'
import zhCN from '../../locales/zh-CN'

// The page fetches through the api module; mocking it keeps every mount
// hermetic. Must be registered before the page import below resolves its
// dependency graph.
vi.mock('../../api/requestLogs', () => ({
  getRequestLogDetail: vi.fn(),
}))

import RequestMessagePage from './RequestMessagePage.vue'

const logDetailMock = vi.mocked(getRequestLogDetail)

const REQ_ID = 'req-msg-1'

type Locale = 'en' | 'zh-CN'
const logsCopy = { en: en.requestLogs, 'zh-CN': zhCN.requestLogs } as const
const sessionsCopy = { en: en.agentSessions, 'zh-CN': zhCN.agentSessions } as const
const pageCopy = { en: en.requestMessages, 'zh-CN': zhCN.requestMessages } as const

let wrapper: VueWrapper | null = null

// App.vue nests pages under n-config-provider > n-message-provider; the
// page's useMessage() throws without that ancestry.
const Host = defineComponent({
  components: { RequestMessagePage },
  template: '<NConfigProvider><NMessageProvider><RequestMessagePage /></NMessageProvider></NConfigProvider>',
})

// Desktop matchMedia stub (naive-ui consults matchMedia on some branches;
// the desktop answer keeps every mount on one code path).
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

// The mounted page's router (memory history), held module-level so the
// navigation tests can read currentRoute after a click.
let currentRouter: Router | null = null

// Mount the page at the end of the given navigation sequence. A plain
// string push lands with NO history state (the deep-link shape); pushing
// an object with `state: { back }` reproduces the bookkeeping vue-router's
// WEB history maintains automatically on every in-app navigation — memory
// history stores the state verbatim but never invents it, so the test
// hands it exactly what a real browser session would hold.
async function mountPage(locale: Locale, navigations: Array<string | { path: string; state: { back: string } }>) {
  const i18n = createI18n({ legacy: false, locale, fallbackLocale: 'zh-CN', messages: { en, 'zh-CN': zhCN } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/agent-sessions', component: { template: '<div />' } },
      { path: '/agent-sessions/:sessionId', component: { template: '<div />' } },
      { path: '/request-logs/:requestId', component: { template: '<div />' } },
      { path: '/request-messages/:requestId', component: { template: '<div />' } },
    ],
  })
  for (const nav of navigations) await router.push(nav)
  currentRouter = router
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [router, i18n, naive] } })
  await nextTick()
  await nextTick()
}

// The request-log detail the page's fetch resolves to: body fields default
// to "nothing recorded", each case overrides what it needs.
function detailFixture(over: Partial<RequestLogDetail>): RequestLogDetail {
  const row: RequestLogRow = {
    request_id: REQ_ID,
    api_key_id: 1,
    username: 'alice',
    model_name: 'demo-model',
    request_path: '/v1/chat/completions',
    source: '',
    parent_request_id: '',
    provider_id: 1,
    provider_name: 'demo-provider',
    is_stream: false,
    status_code: 200,
    status_class: 'success' as StatusClass,
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
    final_provider_model: 'demo-model-upstream',
    duration_ms: 1234,
    created_at: '2026-09-28T08:00:00Z',
  }
  return {
    ...row,
    w3c_trace_id: '',
    agent_client: 'claude-code',
    agent_session_id: 'sess-x',
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

// A request body in the ANTHROPIC wire shape: a top-level system prompt
// carrying markdown (the arm a user-only OpenAI fixture could never cover),
// one user message whose blocks mix text + image, and a tool-role message
// whose plain-string content is text like any other — the arm pinning that
// EVERY role's text bubbles go through the markdown pipeline.
const ANTHROPIC_REQUEST_BODY = JSON.stringify({
  model: 'demo-model',
  system: '# 指南\n\n回答保持简洁。',
  messages: [
    {
      role: 'user',
      content: [
        { type: 'text', text: '看看这张图' },
        { type: 'image', source: { type: 'base64', media_type: 'image/png' } },
      ],
    },
    {
      role: 'assistant',
      content: [{ type: 'tool_use', id: 't1', name: 'Bash', input: {} }],
    },
    {
      role: 'tool',
      content: '工具**执行完毕**，退出码 0',
    },
  ],
})

// A response body in the OPENAI wire shape: one assistant message whose
// markdown mixes a heading, a table, and a fenced go code block.
const OPENAI_RESPONSE_BODY = JSON.stringify({
  choices: [
    {
      message: {
        role: 'assistant',
        content: '## 结果\n\n| 项 | 值 |\n| --- | --- |\n| 延迟 | 120ms |\n\n```go\nfunc main() {}\n```',
      },
    },
  ],
})

// A user-only request body: it renders one left-side bubble, so ANY
// right-side (assistant) row on the page can only be the response side's.
const USER_ONLY_REQUEST_BODY = JSON.stringify({ messages: [{ role: 'user', content: '一条没有留下响应记录的提问' }] })

// The short-locale timestamp the facts strip renders, computed with the
// same call formatShortClock makes — the expected string tracks this
// process's locale; what the assertion pins is WHICH fixture timestamp
// lands on the page.
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

// A button located by its localized text (naive-ui buttons are plain
// <button> elements).
function buttonByText(text: string): HTMLButtonElement | undefined {
  return [...document.body.querySelectorAll('button')].find((b) => (b.textContent ?? '').includes(text))
}

beforeEach(() => {
  stubDesktopMatchMedia()
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
// TC-02 — rendering: both wire formats, markdown pipeline on every text
// bubble (system included), highlighted code block, the eight key facts
// ---------------------------------------------------------------------------

describe.each<Locale>(['zh-CN', 'en'])('RequestMessagePage rendering (%s)', (locale) => {
  it('TC-02: renders both wire formats as bubbles, every text bubble through markdown, and all eight key facts', async () => {
    logDetailMock.mockResolvedValueOnce(
      detailFixture({ request_body: ANTHROPIC_REQUEST_BODY, response_body: OPENAI_RESPONSE_BODY }),
    )
    // The page lands at the conversation's end: a successful load scrolls
    // the bottom actions row into view (the reply + the full-details hop
    // live there). happy-dom here defines no scrollIntoView to spy on, so
    // the test installs a recording stub on the prototype and takes it
    // back afterwards — restore must delete rather than assign, or it
    // would shadow the chain with undefined and poison later tests.
    const proto = HTMLElement.prototype as { scrollIntoView?: unknown }
    const hadOwn = Object.prototype.hasOwnProperty.call(proto, 'scrollIntoView')
    const original = proto.scrollIntoView
    const scrollIntoView = vi.fn()
    proto.scrollIntoView = scrollIntoView
    try {
      await mountPage(locale, [`/request-messages/${REQ_ID}`])

      // The fetch targeted the URL's request id, and the page body rendered.
      await vi.waitFor(() => expect(logDetailMock).toHaveBeenCalledWith(REQ_ID))
      await vi.waitFor(() => expect(document.body.textContent ?? '', 'facts strip shows the request id').toContain(REQ_ID))
      await vi.waitFor(() =>
        expect(scrollIntoView, 'a successful load scrolls the actions row into view').toHaveBeenCalledWith(
          expect.objectContaining({ block: 'end' }),
        ),
      )

    // ---- Eight key facts ----
    const text = document.body.textContent ?? ''
    expect(text, 'fact: request id').toContain(REQ_ID)
    expect(text, 'fact: model').toContain('demo-model')
    expect(text, 'fact: provider').toContain('demo-provider')
    expect(text, 'fact: status tag').toContain(logsCopy[locale].status_success)
    expect(text, 'fact: stream flag').toContain(logsCopy[locale].stream_false)
    expect(
      text,
      'fact: token in/out pair',
    ).toContain(`${logsCopy[locale].tokenRowIn} 10 / ${logsCopy[locale].tokenRowOut} 20`)
    expect(text, 'fact: duration').toContain('1.23s')
    expect(text, 'fact: created time').toContain(shortTime('2026-09-28T08:00:00Z'))
    // Both body sections carry their titles.
    expect(text, 'request section title').toContain(logsCopy[locale].requestBody)
    expect(text, 'response section title').toContain(logsCopy[locale].responseBody)

    // ---- Role coverage: no role silently dropped ----
    const roles = [...document.body.querySelectorAll('.msg-flow__role')].map((el) => el.textContent ?? '')
    expect(roles, 'system / user / tool / assistant roles all rendered').toEqual(
      expect.arrayContaining(['system', 'user', 'tool', 'assistant']),
    )
    // Sides: system, user, and tool sit left, the assistant reply sits right.
    const leftRows = document.body.querySelectorAll('.msg-flow__row--left')
    const rightRows = document.body.querySelectorAll('.msg-flow__row--right')
    expect(leftRows.length, 'system + user + tool bubbles on the left').toBeGreaterThanOrEqual(3)
    expect(rightRows.length, 'assistant bubbles on the right').toBeGreaterThanOrEqual(2)

    // ---- The markdown pipeline covers EVERY text bubble, system included ----
    // The system prompt's "# 指南" must render as a real heading inside its
    // bubble — not as a verbatim "# 指南" text node.
    const systemRow = [...document.body.querySelectorAll('.msg-flow__row--left')].find(
      (r) => (r.querySelector('.msg-flow__role')?.textContent ?? '') === 'system',
    )
    expect(systemRow, 'the system bubble rendered').toBeTruthy()
    expect(systemRow!.querySelector('.msg-flow__text h1'), 'system prompt renders markdown (h1 from "# 指南")').toBeTruthy()
    expect(systemRow!.textContent ?? '', 'no verbatim markdown markers left in the system bubble').not.toContain('# 指南')

    // ---- …and the tool role's text bubble through the same pipeline ----
    // A tool message's plain-string content is markdown like any other
    // text: the emphasis markers must render as a real <strong>, not
    // survive verbatim in the bubble.
    const toolRow = [...document.body.querySelectorAll('.msg-flow__row--left')].find(
      (r) => (r.querySelector('.msg-flow__role')?.textContent ?? '') === 'tool',
    )
    expect(toolRow, 'the tool bubble rendered').toBeTruthy()
    expect(toolRow!.querySelector('.msg-flow__text strong'), 'tool text renders markdown (strong from "**执行完毕**")').toBeTruthy()
    expect(toolRow!.textContent ?? '', 'no verbatim markdown markers left in the tool bubble').not.toContain('**')

    // The assistant reply's markdown: a highlighted fenced code block…
    const code = document.body.querySelector('.msg-flow__text pre code.language-go')
    expect(code, 'fenced go block rendered with its language class').toBeTruthy()
    expect(code!.querySelector('.hljs-keyword'), 'highlight.js tokens inside the code block').toBeTruthy()
    expect(code!.textContent ?? '', 'code content present').toContain('func main()')
    // …and a table with its header row and cell text.
    const table = document.body.querySelector('.msg-flow__text table')
    expect(table, 'markdown table rendered').toBeTruthy()
    expect(table!.querySelectorAll('th').length, 'table header carries both columns').toBe(2)
    expect(table!.textContent ?? '', 'table cell text').toContain('120ms')
    // And the heading from the reply's own markdown.
    expect(document.body.querySelector('.msg-flow__text h2'), 'reply heading rendered').toBeTruthy()

    // ---- Non-text parts keep their arms ----
    expect(document.body.querySelector('.msg-flow__image')?.textContent, 'image part placeholder').toContain(
      sessionsCopy[locale].partImage,
    )
    expect(document.body.querySelector('.msg-flow__tool')?.textContent, 'tool_use part label').toContain('tool_use: Bash')
    } finally {
      if (hadOwn) proto.scrollIntoView = original
      else delete proto.scrollIntoView
    }
  })
})

// ---------------------------------------------------------------------------
// TC-03 — navigation: back's two arms, the view-full hop, deep link
// ---------------------------------------------------------------------------

describe('RequestMessagePage navigation', () => {
  it('TC-03 arm 1: back follows the browser history to the page the user came from', async () => {
    logDetailMock.mockResolvedValueOnce(detailFixture({ request_body: USER_ONLY_REQUEST_BODY }))
    // A real in-app arrival: session detail first, then the message page —
    // web history records `back` on the second push, mirrored here by the
    // explicit state (see mountPage).
    await mountPage('zh-CN', [
      '/agent-sessions/sess-a',
      { path: `/request-messages/${REQ_ID}`, state: { back: '/agent-sessions/sess-a' } },
    ])
    await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(REQ_ID))

    buttonByText(zhCN.requestMessages.back)!.click()
    await vi.waitFor(() =>
      expect(currentRouter!.currentRoute.value.path, 'back returns to the session detail').toBe('/agent-sessions/sess-a'),
    )
  })

  it('TC-03 arm 2: a fresh deep link falls back to the session list (and the page itself fully rendered first)', async () => {
    logDetailMock.mockResolvedValueOnce(detailFixture({ request_body: ANTHROPIC_REQUEST_BODY, response_body: OPENAI_RESPONSE_BODY }))
    // No prior navigation at all — the deep-link shape (new tab, shared
    // URL). The page must still be fully usable on its own.
    await mountPage('zh-CN', [`/request-messages/${REQ_ID}`])
    await vi.waitFor(() => expect(document.body.textContent ?? '', 'deep link renders the page').toContain(REQ_ID))
    expect(document.body.querySelector('.msg-flow__row--left'), 'deep link renders the conversation').toBeTruthy()

    buttonByText(zhCN.requestMessages.back)!.click()
    await vi.waitFor(() =>
      expect(currentRouter!.currentRoute.value.path, 'no history to go back to: land on the session list').toBe('/agent-sessions'),
    )
  })

  it('TC-03: the view-full-details button hops to the request-log detail with this request id', async () => {
    logDetailMock.mockResolvedValueOnce(detailFixture({ request_body: USER_ONLY_REQUEST_BODY }))
    await mountPage('zh-CN', [`/request-messages/${REQ_ID}`])
    await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(REQ_ID))

    const viewFull = buttonByText(zhCN.requestMessages.viewFull)
    expect(viewFull, 'the view-full-details button rendered').toBeTruthy()
    viewFull!.click()
    await vi.waitFor(() =>
      expect(currentRouter!.currentRoute.value.path, 'hop opens the request-log detail for this request').toBe(
        `/request-logs/${REQ_ID}`,
      ),
    )
  })
})

// ---------------------------------------------------------------------------
// TC-04 — the drawer's two regression locks, carried over to the page
// ---------------------------------------------------------------------------

describe('RequestMessagePage failed fetch (TC-04 lock 1)', () => {
  it('surfaces the localized network fallback, never the raw error message', async () => {
    // A NetworkError carries no business code and its message is technical
    // transport detail, not user copy — displayMessage (api/client.ts) is
    // the repo-wide convention for rendering it. Regression-locks the
    // page's catch against falling back to err.message.
    logDetailMock.mockRejectedValueOnce(new NetworkError('ECONNREFUSED raw-transport-detail'))
    await mountPage('zh-CN', [`/request-messages/${REQ_ID}`])

    await vi.waitFor(() =>
      expect(document.body.textContent ?? '', 'inline error block shows the localized networkError copy').toContain(
        zhCN.common.networkError,
      ),
    )
    const text = document.body.textContent ?? ''
    expect(text, 'error block title').toContain(zhCN.requestMessages.loadFailed)
    expect(text, 'the raw transport error string must not leak into the UI').not.toContain('ECONNREFUSED raw-transport-detail')
    // The page shell (header + back) stays usable around the error.
    expect(buttonByText(zhCN.requestMessages.back), 'back button still rendered alongside the error').toBeTruthy()
  })
})

describe('RequestMessagePage response side (TC-04 lock 2)', () => {
  // Both "nothing was ever recorded" states land on the placeholder note:
  // has_stream_body only separates "no capture" from "capture file
  // unreadable", and the request-log detail page renders both of those as
  // the same "not recorded" note — this page matches that convention.
  it.each([
    { name: 'non-stream request failed before any body capture (has_stream_body=false)', hasStreamBody: false },
    { name: 'stream request whose capture file is unreadable (has_stream_body=true)', hasStreamBody: true },
  ])('renders the not-recorded placeholder when both bodies are empty, never an empty assistant bubble: $name', async (c) => {
    logDetailMock.mockResolvedValueOnce(
      detailFixture({ request_body: USER_ONLY_REQUEST_BODY, has_stream_body: c.hasStreamBody }),
    )
    await mountPage('zh-CN', [`/request-messages/${REQ_ID}`])

    // The response section is the "not recorded" note…
    await vi.waitFor(() =>
      expect(document.body.querySelector('.msg-flow__note')?.textContent, 'placeholder note on the response side').toContain(
        zhCN.requestLogs.bodyNotRecorded,
      ),
    )
    // …and NOT an empty assistant bubble ("the model said nothing on
    // purpose"): the request side's only message is the user prompt, so
    // any right-side row here would be exactly that empty bubble.
    expect(document.body.querySelector('.msg-flow__row--right'), 'no assistant bubble for an uncaptured response').toBeNull()
  })

  it('feeds the SSE capture (not the empty response_body) to the fallback view for an unmergeable stream', async () => {
    // A capture with no SSE frame at all — a plain-text upstream error.
    // The translator reports it unmergeable (fallback), and the
    // collapsible view must show THE CAPTURE: a stream request's
    // response_body is '' by definition, and feeding it would leave the
    // expanded view as a bare hint over an empty <pre>.
    const capture = 'upstream reset mid-stream: no SSE frame was ever sent'
    logDetailMock.mockResolvedValueOnce(
      detailFixture({ request_body: USER_ONLY_REQUEST_BODY, stream_body: capture, has_stream_body: true, is_stream: true }),
    )
    await mountPage('zh-CN', [`/request-messages/${REQ_ID}`])

    // The fallback collapse rendered and starts collapsed — the capture
    // text is not in the DOM yet (naive-ui only mounts expanded content).
    const header = await vi.waitFor(() => {
      const el = document.body.querySelector('.n-collapse-item__header')
      expect(el, 'the fallback collapse rendered for the unmergeable capture').toBeTruthy()
      return el!
    })
    expect(header.textContent, 'fallback collapse titled "raw body"').toContain(zhCN.agentSessions.flowFallbackTitle)
    expect(document.body.textContent ?? '', 'raw capture hidden while collapsed').not.toContain(capture)
    // Expand via the collapse's main trigger area (naive-ui binds the
    // toggle there, not on the outer header wrapper).
    header.querySelector('.n-collapse-item__header-main')?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() =>
      expect(document.body.textContent ?? '', 'expanded fallback shows the raw capture text').toContain(capture),
    )
  })

  // The branch's remaining arm, over both locales — the merge note is
  // localized copy, and the old drawer test asserted it per locale.
  describe.each<Locale>(['zh-CN', 'en'])('merged capture (%s)', (locale) => {
    it('merges a mergeable SSE capture into one assistant bubble and marks the response section', async () => {
      // A stream request whose capture holds real OpenAI SSE frames:
      // response_body is '' by definition there, so the reply can only
      // come from merging the capture — the section title must say so,
      // and neither the raw frames nor the fallback JSON view may appear.
      const capture = [
        'data: {"choices":[{"delta":{"role":"assistant","content":"流式"}}]}',
        '',
        'data: {"choices":[{"delta":{"content":"回复已合并"}}]}',
        '',
        'data: [DONE]',
        '',
      ].join('\n')
      logDetailMock.mockResolvedValueOnce(
        detailFixture({ request_body: USER_ONLY_REQUEST_BODY, stream_body: capture, has_stream_body: true, is_stream: true }),
      )
      await mountPage(locale, [`/request-messages/${REQ_ID}`])

      // The section-title notes include the merge signal when the reply was
      // rebuilt from the capture — the page's own merge signal, per locale.
      // (The title now also carries a message-count note, so this reads all
      // notes and matches by content rather than position.)
      await vi.waitFor(() => {
        const notes = [...document.body.querySelectorAll('.msg-section__note')].map((n) => n.textContent ?? '')
        expect(
          notes.some((n) => n.includes(pageCopy[locale].streamMergedNote)),
          'stream-merge note on the response section title',
        ).toBe(true)
      })

      // The deltas concatenated into ONE readable assistant bubble — the
      // request side is user-only, so every right-side row is the reply.
      const rightRows = document.body.querySelectorAll('.msg-flow__row--right')
      expect(rightRows.length, 'exactly one assistant bubble for the merged reply').toBe(1)
      expect(rightRows[0].textContent ?? '', 'stream reply merged to the full text').toContain('流式回复已合并')
      // The merged rendering, not a dump of the capture: no frame payload
      // survives into the DOM.
      expect(document.body.textContent ?? '', 'no raw SSE frame payload leaks into the DOM').not.toContain('"delta"')
      // …and not the unmergeable arm's fallback view either.
      expect(document.body.querySelector('.n-collapse-item__header'), 'no fallback collapse for a mergeable capture').toBeNull()
    })
  })
})
