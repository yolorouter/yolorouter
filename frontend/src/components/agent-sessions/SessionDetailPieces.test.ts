// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the session-detail page's three pieces (zh-CN
// locale; both-locale parity of every key used here is exercised by the
// detail-page test in AgentSessionPages.test.ts, which mounts the whole
// page — summary card, waterfall column, row navigation — in zh-CN and en):
//
//   waterfall bar  bar left/width equal the LINEAR mapping of offset and
//                  duration onto the shared session axis, hand-computed on
//                  a fixture with clean ratios; the precise start time and
//                  duration ride the track's tooltip string
//   summary card   aggregates recomputed from a mixed fixture — statuses,
//                  tokens, a hand-mangled unknown-cost row with a nonzero
//                  cost_micros, and a min/max timestamp pair — equal the
//                  hand-computed figures with the list SQL's semantics
//   message flow   the six bubble-rendering arms: role sides, image
//                  placeholder, tool label, collapsible JSON fallback,
//                  truncation hint, and the stream-merged reply bubble.
//                  (The former drawer pieces — fetch-error and response
//                  three-branch coverage — moved to the request message
//                  page's own test when the drawer was replaced by that
//                  page; the markdown-in-bubble coverage lives there too,
//                  under jsdom.)
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { defineComponent, h, nextTick, type Component } from 'vue'
import naive from 'naive-ui'

import { type RequestLogRow, type StatusClass } from '../../api/requestLogs'
import { translateStreamBody, type TranslatedBody } from '../../utils/messageTranslator'
import en from '../../locales/en'
import zhCN from '../../locales/zh-CN'

import SessionSummaryCard from './SessionSummaryCard.vue'
import SessionWaterfallBar from './SessionWaterfallBar.vue'
import ChatMessageFlow from './ChatMessageFlow.vue'

let wrapper: VueWrapper | null = null

// HelpLabel (summary card) calls useMessage(); give every mount the same
// provider ancestry App.vue provides (NConfigProvider > NMessageProvider).
const Host = defineComponent({
  template: '<NConfigProvider><NMessageProvider><slot /></NMessageProvider></NConfigProvider>',
})

// Desktop matchMedia stub (useIsMobile inside HelpLabel).
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

// Mount one piece with props under the provider host.
async function mountPiece(component: Component, props: Record<string, unknown>) {
  const i18n = createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { en, 'zh-CN': zhCN } })
  wrapper = mount(Host, {
    attachTo: document.body,
    global: { plugins: [i18n, naive] },
    slots: { default: () => h(component, props) },
  })
  await nextTick()
  await nextTick()
}

// A timeline row with only the fields the pieces read.
function row(over: Partial<RequestLogRow>): RequestLogRow {
  return {
    request_id: 'req-x',
    api_key_id: 1,
    username: 'alice',
    model_name: 'model-x',
    request_path: '/v1/chat/completions',
    source: '',
    parent_request_id: '',
    provider_id: 1,
    provider_name: 'demo-provider',
    is_stream: false,
    status_code: 200,
    status_class: 'success' as StatusClass,
    input_tokens: 0,
    output_tokens: 0,
    cache_write_tokens: 0,
    cache_read_tokens: 0,
    image_count: 0,
    usage_seconds: 0,
    usage_characters: 0,
    image_unit_price: null,
    cost_micros: 0,
    cost_known: true,
    fail_reason: null,
    attempts: 1,
    key_switches: 0,
    failovers: 0,
    final_provider_model: 'model-x-upstream',
    duration_ms: 0,
    created_at: '2026-09-28T08:00:00Z',
    ...over,
  }
}

// The same short-locale timestamp the bar's tooltip formats, computed with
// the same call so the expected string tracks this process's locale.
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
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

// ---------------------------------------------------------------------------
// Waterfall bar — linear mapping + tooltip string
// ---------------------------------------------------------------------------

describe('SessionWaterfallBar (session-detail UX TC-04)', () => {
  // Session axis: starts 08:00:00, and the FIRST case's row runs 100s, so
  // the axis spans 100_000ms. Ratios are clean on purpose — the assertions
  // pin exact percents, so a mapping that swaps offset/span or inverts the
  // clamp cannot pass by rounding luck.
  const START = Date.parse('2026-09-28T08:00:00Z')
  const SPAN = 100_000

  const CASES = [
    { name: 'the row that defines the axis end: offset 0%, full width', created: '2026-09-28T08:00:00Z', duration: 100_000, left: '0%', width: '100%', durationText: '100.00s' },
    { name: 'offset 10%, width 20%', created: '2026-09-28T08:00:10Z', duration: 20_000, left: '10%', width: '20%', durationText: '20.00s' },
    { name: 'offset 60%, width 10%', created: '2026-09-28T08:01:00Z', duration: 10_000, left: '60%', width: '10%', durationText: '10.00s' },
  ] as const

  it.each(CASES)('$name', async (c) => {
    await mountPiece(SessionWaterfallBar, {
      row: row({ created_at: c.created, duration_ms: c.duration, status_class: 'success' }),
      startMs: START,
      spanMs: SPAN,
    })

    const bar = document.body.querySelector<HTMLElement>('.waterfall__bar')
    expect(bar, 'bar rendered inside its track').toBeTruthy()
    // The linear mapping, hand-computed: left = offset/span, width =
    // duration/span (percent strings exactly as the component emits them).
    expect(bar!.style.left, 'left = (created_at − session start) / span').toBe(c.left)
    expect(bar!.style.width, 'width = duration / span').toBe(c.width)
    expect(bar!.classList.contains('waterfall__bar--success'), 'bar colour follows the status bucket').toBe(true)

    // The hover tooltip: dispatching mouseenter on the track mounts the
    // NTooltip body with the precise start timestamp and the duration; the
    // same string also rides the track's aria-label/title so touch, keyboard
    // and screen readers never lose it.
    const track = document.body.querySelector<HTMLElement>('.waterfall')
    const expected = zhCN.agentSessions.waterfallTip
      .replace('{time}', shortTime(c.created))
      .replace('{duration}', c.durationText)
    track?.dispatchEvent(new MouseEvent('mouseenter'))
    await vi.waitFor(() =>
      expect(document.body.textContent, 'hover mounts the tooltip body with the exact string').toContain(expected),
    )
    expect(track?.getAttribute('aria-label'), 'tooltip string carries the precise start time').toBe(expected)
    expect(track?.getAttribute('title'), 'native hover fallback carries the same string').toBe(expected)
  })

  it('collapses to a full-width bar at the left edge when the session has no measurable span', async () => {
    await mountPiece(SessionWaterfallBar, {
      row: row({ created_at: '2026-09-28T08:00:00Z', duration_ms: 5 }),
      startMs: Date.parse('2026-09-28T08:00:00Z'),
      spanMs: 0,
    })
    const bar = document.body.querySelector<HTMLElement>('.waterfall__bar')
    expect(bar?.style.left).toBe('0%')
    expect(bar?.style.width).toBe('100%')
  })
})

// ---------------------------------------------------------------------------
// Summary card — hand-computed aggregate
// ---------------------------------------------------------------------------

describe('SessionSummaryCard (session-detail UX TC-05)', () => {
  it('recomputes request/success/token/cost/duration figures with the list SQL semantics', async () => {
    const requests: RequestLogRow[] = [
      row({ request_id: 'r1', status_class: 'success', input_tokens: 100, output_tokens: 200, cost_micros: 1000, cost_known: true, created_at: '2026-09-28T08:00:00Z' }),
      // Hand-mangled unknown row: cost_known=false yet a nonzero
      // cost_micros — the CASE guard in the list SQL keeps it OUT of the
      // known total; so must the frontend recompute.
      row({ request_id: 'r2', status_class: 'failed', input_tokens: 0, output_tokens: 0, cost_micros: 9999, cost_known: false, created_at: '2026-09-28T08:10:00Z' }),
      row({ request_id: 'r3', status_class: 'success', input_tokens: 50, output_tokens: 0, cost_micros: 2500, cost_known: true, created_at: '2026-09-28T08:30:00Z' }),
      row({ request_id: 'r4', status_class: 'partial', input_tokens: 7, output_tokens: 3, cost_micros: 0, cost_known: false, created_at: '2026-09-28T08:45:30Z' }),
    ]
    // Hand computation:
    //   total 4, successes 2 (r1, r3)
    //   input 157, output 203
    //   known cost 1000+2500 = 3500 micros -> 0.003500; unknown rows 2
    //   span 08:00:00 -> 08:45:30 = 45m30s
    await mountPiece(SessionSummaryCard, { requests })
    const text = document.body.textContent ?? ''
    expect(text, 'request count').toContain('4')
    expect(text, 'success sub-line').toContain(zhCN.agentSessions.successCountLine.replace('{n}', '2'))
    expect(text, 'input token sum').toContain('157')
    expect(text, 'output token sum').toContain('203')
    expect(text, 'known-cost total (micros -> 6dp, unknown row excluded)').toContain('0.003500')
    expect(text, 'unknown-cost marker with its row count').toContain(zhCN.agentSessions.costUnknownNote.replace('{n}', '2'))
    expect(text, 'session duration = last - first').toContain('45m 30s')
    // The first/last doublet renders each of its own timestamps.
    expect(text).toContain(shortTime('2026-09-28T08:00:00Z'))
    expect(text).toContain(shortTime('2026-09-28T08:45:30Z'))
  })
})

// ---------------------------------------------------------------------------
// Message flow — the six bubble arms
// ---------------------------------------------------------------------------

describe('ChatMessageFlow (session-detail UX TC-06)', () => {
  // Arms 1-3 + 5: one translated body exercising role sides, an image
  // placeholder, a tool label, and the truncation hint together.
  it('renders role sides, image placeholder, tool label, and the truncation hint', async () => {
    const body: TranslatedBody = {
      kind: 'messages',
      truncated: true,
      messages: [
        { role: 'user', text: '看看这张图', parts: [{ type: 'text', text: '看看这张图' }, { type: 'image' }] },
        { role: 'assistant', text: '', parts: [{ type: 'tool', label: 'tool_use: Bash' }] },
      ],
    }
    await mountPiece(ChatMessageFlow, { body, raw: '' })
    const rowsEls = [...document.body.querySelectorAll('.msg-flow__row')]
    expect(rowsEls.length, 'one row per message').toBe(2)
    expect(rowsEls[0].classList.contains('msg-flow__row--left'), 'user message sits left').toBe(true)
    expect(rowsEls[1].classList.contains('msg-flow__row--right'), 'assistant message sits right').toBe(true)
    expect(rowsEls[0].textContent, 'text part reads verbatim').toContain('看看这张图')
    expect(document.body.querySelector('.msg-flow__image')?.textContent, 'image placeholder block').toContain(zhCN.agentSessions.partImage)
    expect(document.body.querySelector('.msg-flow__tool')?.textContent, 'tool part shows its type label').toContain('tool_use: Bash')
    expect(document.body.textContent, 'truncation hint').toContain(zhCN.agentSessions.flowTruncated)
  })

  it('renders the collapsible JSON fallback and expands it', async () => {
    const raw = '{"error":"not a chat body","code":42}'
    const body: TranslatedBody = { kind: 'fallback', messages: [], truncated: false }
    await mountPiece(ChatMessageFlow, { body, raw })
    // The collapse header is present and starts collapsed; clicking its
    // main trigger area (naive-ui binds the toggle there, not on the outer
    // header wrapper) reveals the formatted JSON tree.
    const header = document.body.querySelector('.n-collapse-item__header')
    expect(header?.textContent, 'fallback collapse titled "raw body"').toContain(zhCN.agentSessions.flowFallbackTitle)
    expect(document.body.querySelector('.vjs-tree'), 'JSON tree hidden while collapsed').toBeNull()
    header?.querySelector('.n-collapse-item__header-main')?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(document.body.querySelector('.vjs-tree'), 'JSON tree expanded on click').not.toBeNull())
    expect(document.body.textContent).toContain('error')
  })

  it('renders the stream-merged reply as one readable assistant bubble', async () => {
    // An OpenAI SSE capture as the on-disk .stream file holds it; the
    // drawer feeds exactly this string to the same translator before
    // handing the result to this component.
    const capture = [
      'data: {"choices":[{"delta":{"role":"assistant","content":"Hello"}}]}',
      '',
      'data: {"choices":[{"delta":{"content":" streamed"}}]}',
      '',
      'data: {"choices":[{"delta":{"content":" reply"}}]}',
      '',
      'data: [DONE]',
      '',
    ].join('\n')
    const body = translateStreamBody(capture)
    expect(body.kind, 'capture merges on the messages path').toBe('messages')
    await mountPiece(ChatMessageFlow, { body, raw: '' })
    const bubble = document.body.querySelector('.msg-flow__row--right .msg-flow__bubble')
    expect(bubble?.textContent, 'merged full reply text in one assistant bubble').toContain('Hello streamed reply')
  })

  it('collapses long text parts behind a disclosure row and expands on click', async () => {
    // Agent-tool payloads routinely carry whole system contexts as ONE text
    // part; past the collapse threshold the part renders clamped (fade
    // preview) with a quiet toggle, and clicking swaps clamp for the full
    // text. Short parts never clamp — the first test's texts cover that.
    const long = Array.from({ length: 500 }, (_, k) => `第 ${k} 段：这是一段用于超过折叠阈值的长文本。`).join('\n\n')
    expect(long.length, 'fixture actually crosses the threshold').toBeGreaterThan(3500)
    const body: TranslatedBody = {
      kind: 'messages',
      truncated: false,
      messages: [{ role: 'user', text: long, parts: [{ type: 'text', text: long }] }],
    }
    await mountPiece(ChatMessageFlow, { body, raw: '' })
    const textEl = document.body.querySelector('.msg-flow__text')
    expect(textEl?.classList.contains('msg-flow__text--clamped'), 'long part starts clamped').toBe(true)
    const btn = document.body.querySelector('.msg-flow__disclose-btn') as HTMLButtonElement | null
    expect(btn?.textContent, 'disclose row offers expand in the zh locale').toContain(zhCN.requestMessages.expandFull)
    expect(document.body.querySelector('.msg-flow__disclose-meta')?.textContent, 'size note carries the character count').toContain(
      long.length.toLocaleString(),
    )
    btn?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() =>
      expect(document.body.querySelector('.msg-flow__text')?.classList.contains('msg-flow__text--clamped'), 'expanded clears the clamp').toBe(false),
    )
    expect(document.body.querySelector('.msg-flow__disclose-btn')?.textContent, 'toggle flips to collapse').toContain(zhCN.requestMessages.collapse)
  })

  it('renders the placeholder note for a body that was never recorded inline', async () => {
    const body: TranslatedBody = { kind: 'placeholder', messages: [], truncated: false }
    await mountPiece(ChatMessageFlow, { body, raw: '' })
    expect(document.body.querySelector('.msg-flow__note')?.textContent, 'placeholder note').toContain(zhCN.requestLogs.bodyNotRecorded)
  })
})
