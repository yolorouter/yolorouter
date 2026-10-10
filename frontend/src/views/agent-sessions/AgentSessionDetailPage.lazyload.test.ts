// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the replay page's lazy loading (the paging
// behavior itself; the rendering assembly has its own file,
// AgentSessionPages.test.ts). IntersectionObserver is stubbed the way the
// anchor tests stub scrollIntoView — happy-dom ships a no-op
// implementation that never fires, so the tests install a controllable
// one and trigger the boundary callbacks by hand.
//
// Covered acceptance cases (zh-CN locale — behavior, not copy breadth):
//
//   TC-09  opening fetches the LATEST page exactly once (no cursor
//          params), lands the viewport on the newest turn via
//          scrollIntoView bottom-aligned, and leaves no busy copy at the
//          top boundary once settled
//   TC-10  the top sentinel fires exactly one before request (an
//          in-flight direction does not re-request), prepends the older
//          page in chronological order, and is removed once the head is
//          reached (no further requests possible)
//   TC-11  a #req-<id> hash for an unloaded turn anchors the FIRST fetch
//          at it (inclusive after); a hash arriving later for an
//          already-loaded turn only scrolls (no refetch); a stale anchor
//          falls back to the latest page instead of an empty replay
//   TC-12  a session that fits one page renders no paging controls at all
//   TC-15  a failed older page turns the boundary into the retry row;
//          retrying prepends and paging continues
//   TC-16  the same session rendered as one full page vs assembled from
//          two pages produces identical block DOM and info bar
//   TC-17  the info bar keeps showing the whole-session aggregate however
//          many older pages load
//   TC-18  the system group fetches its raw text exactly on first expand
//          (zero requests while collapsed, none again on re-expand)
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import {
  getAgentSessionTranscript,
  getAgentSessionTranscriptSystem,
  type AgentSessionReplayBlock,
  type AgentSessionRow,
  type AgentSessionTranscript,
} from '../../api/agentSessions'
import en from '../../locales/en'
import zhCN from '../../locales/zh-CN'

vi.mock('../../api/agentSessions', () => ({
  getAgentSessionTranscript: vi.fn(),
  getAgentSessionTranscriptSystem: vi.fn(),
}))

const transcriptMock = vi.mocked(getAgentSessionTranscript)
const transcriptSystemMock = vi.mocked(getAgentSessionTranscriptSystem)

// Must be imported after the vi.mock factory is registered.
import AgentSessionDetailPage from './AgentSessionDetailPage.vue'

const SESSION = 'sess-lazy'
const zhCopy = zhCN.agentSessions

// App.vue nests pages under n-config-provider > n-message-provider; the
// page's useMessage() throws without that ancestry.
const DetailHost = defineComponent({
  components: { AgentSessionDetailPage },
  template: '<NConfigProvider><NMessageProvider><AgentSessionDetailPage /></NMessageProvider></NConfigProvider>',
})

// ---------- Controllable IntersectionObserver stub ----------

type IOEntry = { isIntersecting: boolean; target: Element }
type IOCallback = (entries: IOEntry[]) => void

class FakeIntersectionObserver {
  static instances: FakeIntersectionObserver[] = []
  private observed = new Set<Element>()
  private cb: IOCallback
  constructor(cb: IOCallback) {
    this.cb = cb
    FakeIntersectionObserver.instances.push(this)
  }
  observe(el: Element): void {
    this.observed.add(el)
  }
  unobserve(el: Element): void {
    this.observed.delete(el)
  }
  disconnect(): void {
    this.observed.clear()
  }
  // The manual trigger the real browser would fire for a scroll into the
  // (margin-expanded) viewport.
  fire(target: Element, isIntersecting = true): void {
    this.cb([{ isIntersecting, target }])
  }
  has(target: Element): boolean {
    return this.observed.has(target)
  }
}

function observerWatching(target: Element): FakeIntersectionObserver | undefined {
  return FakeIntersectionObserver.instances.find((io) => io.has(target))
}

// ---------- Fixtures: one 24-block session, paged ----------

function blk(id: string): AgentSessionReplayBlock {
  return {
    request_id: id,
    created_at: '2026-10-10T00:00:00Z',
    model_name: 'model-x',
    status_class: 'success',
    is_stream: false,
    input_tokens: 1,
    output_tokens: 2,
    duration_ms: 10,
    provider_name: 'demo-provider',
    added: [{ role: 'user', content: `msg-${id}` }],
    assistant: { source: 'none', truncated: false, raw: null },
    added_truncated: false,
  }
}

// The whole-session aggregate — identical on every page by contract, which
// is exactly what TC-17 pins.
function aggregate(): AgentSessionRow {
  return {
    agent_session_id: SESSION,
    agent_client: 'claude-code',
    request_count: 24,
    success_count: 24,
    input_tokens: 240,
    output_tokens: 480,
    known_cost_micros: 1200,
    unknown_cost_count: 0,
    first_seen_at: '2026-10-10T08:00:00Z',
    last_seen_at: '2026-10-10T11:00:00Z',
  }
}

function pageOf(
  ids: string[],
  hasMoreBefore: boolean,
  nextBefore: string,
  hasMoreAfter: boolean,
  nextAfter: string,
  system: AgentSessionTranscript['system'] = [],
): AgentSessionTranscript {
  return {
    session: aggregate(),
    system,
    blocks: ids.map(blk),
    has_more_before: hasMoreBefore,
    next_before: nextBefore,
    has_more_after: hasMoreAfter,
    next_after: nextAfter,
  }
}

const ALL_IDS = Array.from({ length: 24 }, (_, k) => `b${String(k).padStart(2, '0')}`)
const LATEST_PAGE = pageOf(ALL_IDS.slice(14), true, 'b14', false, '')
const OLDER_PAGE_1 = pageOf(ALL_IDS.slice(4, 14), true, 'b04', true, 'b13')
const OLDER_PAGE_2 = pageOf(ALL_IDS.slice(0, 4), false, '', true, 'b03')
const ANCHOR_B05_PAGE = pageOf(ALL_IDS.slice(5, 15), true, 'b05', true, 'b14')
const SHORT_PAGE = pageOf(Array.from({ length: 15 }, (_, k) => `s${String(k).padStart(2, '0')}`), false, '', false, '')
const FULL_PAGE = pageOf(ALL_IDS, false, '', false, '')

// The default paged behavior: latest opening, two older pages, an anchored
// window at b05, an empty page for any cursor the fixture does not know.
function installPagedMock(extra?: Record<string, AgentSessionTranscript>) {
  transcriptMock.mockImplementation(async (_sid: string, params?: { before?: string; after?: string }) => {
    if (!params || (!params.before && !params.after)) return LATEST_PAGE
    if (params.before === 'b14') return OLDER_PAGE_1
    if (params.before === 'b04') return OLDER_PAGE_2
    if (params.after === 'b05') return ANCHOR_B05_PAGE
    if (extra && params.before && params.before in extra) return extra[params.before]
    if (extra && params.after && params.after in extra) return extra[params.after]
    // An unknown cursor: the server's stale-cursor shape — an empty page
    // with both has_more flags false, aggregate still whole.
    return pageOf([], false, '', false, '')
  })
}

// ---------- Mount plumbing ----------

let wrapper: VueWrapper | null = null
let currentRouter: Router | null = null

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
      { path: '/request-logs/:requestId', component: { template: '<div />' } },
    ],
  })
  await router.push(startPath)
  await router.isReady()
  return router
}

async function mountDetail(startPath = `/agent-sessions/${SESSION}`) {
  const i18n = createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { en, 'zh-CN': zhCN } })
  currentRouter = await makeRouter(startPath)
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(DetailHost, { attachTo: document.body, global: { plugins: [pinia, currentRouter, i18n, naive] } })
  await nextTick()
  await nextTick()
}

// Mount and settle the opening fetch.
async function mountReplay(startPath = `/agent-sessions/${SESSION}`) {
  await mountDetail(startPath)
  await vi.waitFor(() => expect(transcriptMock).toHaveBeenCalledTimes(1))
  await nextTick()
}

// The scrollIntoView recording stub (same install/restore discipline as
// the anchor tests in AgentSessionPages.test.ts).
type ScrollIntoViewFn = (...args: unknown[]) => void
let scrollIntoViewSpy: ReturnType<typeof vi.fn<ScrollIntoViewFn>> | null = null
function stubScrollIntoView() {
  const proto = HTMLElement.prototype as { scrollIntoView?: ScrollIntoViewFn }
  const hadOwn = Object.prototype.hasOwnProperty.call(proto, 'scrollIntoView')
  const original = proto.scrollIntoView
  const spy = vi.fn<ScrollIntoViewFn>()
  scrollIntoViewSpy = spy
  proto.scrollIntoView = spy
  return () => {
    if (hadOwn) proto.scrollIntoView = original
    else delete proto.scrollIntoView
    scrollIntoViewSpy = null
  }
}

function blockEl(requestId: string): HTMLElement | null {
  return document.getElementById(`req-${requestId}`)
}

function blockIdsInOrder(): string[] {
  return [...document.body.querySelectorAll('.replay-block')].map((el) => el.id.slice('req-'.length))
}

function topPager(): HTMLElement | null {
  return document.body.querySelector<HTMLElement>('[data-testid="replay-pager-top"]')
}

function bottomPager(): HTMLElement | null {
  return document.body.querySelector<HTMLElement>('[data-testid="replay-pager-bottom"]')
}

function fireTop() {
  const el = topPager()
  expect(el, 'top sentinel exists to fire').toBeTruthy()
  observerWatching(el!)!.fire(el!)
}

beforeEach(() => {
  stubDesktopMatchMedia()
  vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver as unknown as typeof IntersectionObserver)
  FakeIntersectionObserver.instances = []
  installPagedMock()
  transcriptSystemMock.mockResolvedValue({ system: [] })
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
// TC-09 — opening: latest page, exactly once, bottom landing
// ---------------------------------------------------------------------------

describe('AgentSessionDetailPage lazy loading — opening (TC-09)', () => {
  it('fetches the latest page exactly once with no cursor params, lands on the newest turn, leaves no busy copy at the top', async () => {
    const restoreScroll = stubScrollIntoView()
    try {
      await mountReplay()
      for (let i = 0; i < 5; i++) await nextTick()
      await new Promise((resolve) => setTimeout(resolve, 20))

      // Exactly one transcript fetch, no cursor params — the latest-page
      // opening, no polling, no follow-up.
      expect(transcriptMock, 'exactly one fetch on open').toHaveBeenCalledTimes(1)
      expect(transcriptMock.mock.calls[0][0], 'the fetch targeted the URL session id').toBe(SESSION)
      expect(transcriptMock.mock.calls[0][1], 'the opening fetch carries no cursor params').toBeUndefined()

      // The latest page's turns render — newest turn present, older
      // not-loaded turns absent.
      expect(blockEl('b23'), 'newest turn rendered').toBeTruthy()
      expect(blockEl('b13'), 'turn older than the page boundary not rendered').toBeNull()
      expect(blockIdsInOrder()).toEqual(ALL_IDS.slice(14))

      // The viewport lands on the NEWEST turn, bottom-aligned — the
      // resume-a-session reading position.
      expect(scrollIntoViewSpy!, 'the landing scroll happened').toHaveBeenCalledTimes(1)
      expect(scrollIntoViewSpy!.mock.contexts[0], 'scrollIntoView was called ON the newest turn').toBe(blockEl('b23'))
      expect(scrollIntoViewSpy!.mock.calls[0][0], 'bottom alignment').toEqual({ block: 'end' })

      // The top boundary exists (older history remains) but carries no
      // leftover busy copy once settled — idle chrome is empty.
      const pager = topPager()
      expect(pager, 'top boundary exists while older history remains').toBeTruthy()
      expect((pager!.textContent ?? '').trim(), 'no busy copy left at the top after settling').toBe('')
    } finally {
      restoreScroll()
    }
  })
})

// ---------------------------------------------------------------------------
// TC-10 — top sentinel: one request per hit, no re-request in flight,
// prepend order, sentinel removed at the head
// ---------------------------------------------------------------------------

describe('AgentSessionDetailPage lazy loading — upscroll paging (TC-10)', () => {
  it('a sentinel hit fires exactly one before request; an in-flight direction does not re-request; the page prepends chronologically; the sentinel is removed once the head is reached', async () => {
    // The first older page resolves under the test's control, so the
    // in-flight window is observable.
    let releaseOlder1!: (page: AgentSessionTranscript) => void
    const older1 = new Promise<AgentSessionTranscript>((resolve) => {
      releaseOlder1 = resolve
    })
    transcriptMock.mockImplementation(async (_sid: string, params?: { before?: string; after?: string }) => {
      if (!params || (!params.before && !params.after)) return LATEST_PAGE
      if (params.before === 'b14') return older1
      if (params.before === 'b04') return OLDER_PAGE_2
      return pageOf([], false, '', false, '')
    })

    await mountReplay()
    expect(topPager(), 'top boundary present before any upscroll').toBeTruthy()

    // The page wired the sentinel into an observer.
    const io = observerWatching(topPager()!)
    expect(io, 'the top sentinel is observed').toBeTruthy()

    // First hit: exactly one before request, under the page's oldest block
    // id (the exclusive cursor).
    io!.fire(topPager()!)
    await vi.waitFor(() => expect(transcriptMock, 'the hit fired one before request').toHaveBeenCalledTimes(2))
    expect(transcriptMock.mock.calls[1][1], 'the request pages exclusively from the page cursor').toEqual({ before: 'b14' })

    // While that request is in flight, repeated hits must not re-request.
    io!.fire(topPager()!)
    io!.fire(topPager()!)
    await new Promise((resolve) => setTimeout(resolve, 10))
    expect(transcriptMock, 'in-flight direction does not re-request').toHaveBeenCalledTimes(2)

    // The page arrives: blocks prepend in chronological order, no
    // duplicates, and the boundary keeps existing (one more page left).
    releaseOlder1(OLDER_PAGE_1)
    await vi.waitFor(() => expect(blockIdsInOrder()).toEqual(ALL_IDS.slice(4)))
    expect(topPager(), 'boundary remains while older history remains').toBeTruthy()

    // Second page reaches the head: has_more_before=false removes the
    // sentinel from the DOM entirely — no further request is possible.
    io!.fire(topPager()!)
    await vi.waitFor(() => expect(transcriptMock, 'the head page was requested').toHaveBeenCalledTimes(3))
    expect(transcriptMock.mock.calls[2][1]).toEqual({ before: 'b04' })
    await vi.waitFor(() => {
      expect(topPager(), 'sentinel removed once the head is reached').toBeNull()
      expect(blockIdsInOrder()).toEqual(ALL_IDS)
    })
    expect(transcriptMock, 'no request after the sentinel is gone').toHaveBeenCalledTimes(3)
  })

  it('a hash arriving later for an already-loaded turn scrolls without refetching; the bottom sentinel pages toward newer and is removed at the tail', async () => {
    const restoreScroll = stubScrollIntoView()
    try {
      // Open anchored mid-history: both directions continuable.
      await mountReplay(`/agent-sessions/${SESSION}#req-b05`)
      await vi.waitFor(() => expect(blockEl('b05'), 'anchored turn rendered').toBeTruthy())
      expect(transcriptMock.mock.calls[0][1], 'the opening fetch anchored at b05').toEqual({ after: 'b05' })
      expect(scrollIntoViewSpy!.mock.calls[0][0], 'the landing targets the anchor').toEqual({ block: 'start' })

      // The toward-newer boundary exists and pages toward the tail.
      const bottom = bottomPager()
      expect(bottom, 'bottom boundary present under an anchored opening').toBeTruthy()
      const ioBottom = observerWatching(bottom!)
      expect(ioBottom, 'the bottom sentinel is observed').toBeTruthy()

      transcriptMock.mockImplementation(async (_sid: string, params?: { before?: string; after?: string }) => {
        if (!params) return LATEST_PAGE
        if (params.after === 'b05') return ANCHOR_B05_PAGE
        if (params.after === 'b14') return pageOf(ALL_IDS.slice(14), true, 'b14', false, '')
        return pageOf([], false, '', false, '')
      })
      ioBottom!.fire(bottom!)
      await vi.waitFor(() => expect(transcriptMock, 'the toward-newer continuation was requested').toHaveBeenCalledTimes(2))
      expect(transcriptMock.mock.calls[1][1], 'the continuation uses the inclusive after cursor').toEqual({ after: 'b14' })
      await vi.waitFor(() => {
        expect(blockIdsInOrder()).toEqual(ALL_IDS.slice(5))
        expect(bottomPager(), 'bottom boundary removed at the tail').toBeNull()
      })

      // A later hash for a turn now in the loaded window: scroll only.
      await currentRouter!.push(`/agent-sessions/${SESSION}#req-b07`)
      await vi.waitFor(() => expect(scrollIntoViewSpy!.mock.calls.length).toBeGreaterThanOrEqual(2))
      expect(transcriptMock, 'an already-loaded anchor does not refetch').toHaveBeenCalledTimes(2)
      expect(scrollIntoViewSpy!.mock.contexts[1], 'the re-anchor scrolled its own turn').toBe(blockEl('b07'))
      expect(scrollIntoViewSpy!.mock.calls[1][0]).toEqual({ block: 'start' })
    } finally {
      restoreScroll()
    }
  })
})

// ---------------------------------------------------------------------------
// TC-11 — anchor deep link: first fetch anchored, stale anchor recovers
// ---------------------------------------------------------------------------

describe('AgentSessionDetailPage lazy loading — anchor deep link (TC-11)', () => {
  it('a stale #req-<id> anchor falls back to the latest page instead of an empty replay', async () => {
    // Not mountReplay: the fallback makes the second fetch fast enough
    // that "exactly one call" would race.
    await mountDetail(`/agent-sessions/${SESSION}#req-gone`)
    // The anchored fetch answered the stale-cursor shape (empty page, both
    // has_more false, aggregate whole) — the page detects it (a session
    // with requests cannot otherwise answer zero blocks) and refetches
    // without the cursor.
    await vi.waitFor(() => expect(transcriptMock).toHaveBeenCalledTimes(2))
    expect(transcriptMock.mock.calls[0][1], 'first fetch anchored at the stale id').toEqual({ after: 'gone' })
    expect(transcriptMock.mock.calls[1][1], 'fallback fetch is the plain latest opening').toBeUndefined()
    await vi.waitFor(() => expect(blockIdsInOrder()).toEqual(ALL_IDS.slice(14)))
  })
})

// ---------------------------------------------------------------------------
// TC-12 — short session: zero paging chrome
// ---------------------------------------------------------------------------

describe('AgentSessionDetailPage lazy loading — short session (TC-12)', () => {
  it('a session that fits one page renders no paging controls and no loading placeholders', async () => {
    transcriptMock.mockResolvedValue(SHORT_PAGE)
    await mountReplay()
    for (let i = 0; i < 5; i++) await nextTick()
    await new Promise((resolve) => setTimeout(resolve, 20))

    expect(document.body.querySelectorAll('.replay-pager').length, 'no boundary rows at all').toBe(0)
    const text = document.body.textContent ?? ''
    expect(text, 'no older-direction copy').not.toContain(zhCopy.loadOlder)
    expect(text, 'no newer-direction copy').not.toContain(zhCopy.loadNewer)
    expect(blockIdsInOrder()).toEqual(Array.from({ length: 15 }, (_, k) => `s${String(k).padStart(2, '0')}`))
  })
})

// ---------------------------------------------------------------------------
// TC-15 — failed older page: retry row, then continue
// ---------------------------------------------------------------------------

describe('AgentSessionDetailPage lazy loading — failure and retry (TC-15)', () => {
  it('a failed before request turns the boundary into the retry row; retrying prepends and paging continues', async () => {
    let failFirst = true
    transcriptMock.mockImplementation(async (_sid: string, params?: { before?: string; after?: string }) => {
      if (!params || (!params.before && !params.after)) return LATEST_PAGE
      if (params.before === 'b14') {
        if (failFirst) {
          failFirst = false
          throw new Error('boom')
        }
        return OLDER_PAGE_1
      }
      return pageOf([], false, '', false, '')
    })

    await mountReplay()
    fireTop()
    await vi.waitFor(() => expect(topPager()?.textContent ?? '', 'the failure copy shows at the boundary').toContain(zhCopy.turnLoadFailed))

    // The retry action: same cursor, and on success the page prepends and
    // the error row is gone (the boundary keeps its idle look).
    const retry = [...(topPager()?.querySelectorAll('button') ?? [])].find((b) => b.textContent?.includes(zhCopy.retry))
    expect(retry, 'the retry button is offered').toBeTruthy()
    retry!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(blockIdsInOrder()).toEqual(ALL_IDS.slice(4)))
    expect(transcriptMock, 'exactly the failed request plus its retry').toHaveBeenCalledTimes(3)
    expect(topPager()?.textContent ?? '', 'error row cleared after the successful retry').not.toContain(zhCopy.turnLoadFailed)
  })
})

// ---------------------------------------------------------------------------
// TC-16 — paged assembly renders identically to a one-page whole
// ---------------------------------------------------------------------------

describe('AgentSessionDetailPage lazy loading — render parity (TC-16)', () => {
  it('the same session assembled from two pages renders the same block DOM and info bar as a one-page whole', async () => {
    const capture = () => ({
      blocks: [...document.body.querySelectorAll('.replay-block')].map((el) => el.innerHTML),
      meta: (document.body.querySelector('.replay-meta')?.textContent ?? '').replace(/\s+/g, ' ').trim(),
    })

    // Leg 1: the whole session in one page (a session that fits — the
    // pre-paging rendering shape).
    transcriptMock.mockResolvedValue(FULL_PAGE)
    await mountDetail()
    await vi.waitFor(() => expect(blockIdsInOrder()).toEqual(ALL_IDS))
    const whole = capture()
    expect(whole.blocks.length).toBe(24)

    wrapper!.unmount()
    document.body.innerHTML = ''

    // Leg 2: the same blocks assembled from the latest page plus one
    // older page through the sentinel. The paged mock must be back in
    // place before the mount — leg 1 replaced it with the one-page
    // mockResolvedValue.
    transcriptMock.mockImplementation(async (_sid: string, params?: { before?: string; after?: string }) => {
      if (!params || (!params.before && !params.after)) return LATEST_PAGE
      if (params.before === 'b14') return pageOf(ALL_IDS.slice(0, 14), false, '', true, 'b13')
      return pageOf([], false, '', false, '')
    })
    await mountDetail()
    await vi.waitFor(() => expect(blockIdsInOrder()).toEqual(ALL_IDS.slice(14)))
    fireTop()
    await vi.waitFor(() => expect(blockIdsInOrder()).toEqual(ALL_IDS))
    const paged = capture()

    expect(paged.blocks, 'every turn renders identically after paged assembly').toEqual(whole.blocks)
    expect(paged.meta, 'the info bar is unchanged').toBe(whole.meta)
  })
})

// ---------------------------------------------------------------------------
// TC-17 — the info bar keeps the whole-session aggregate
// ---------------------------------------------------------------------------

describe('AgentSessionDetailPage lazy loading — aggregate constancy (TC-17)', () => {
  it('the info bar numbers stay the whole-session aggregate while older pages load', async () => {
    await mountReplay()
    const metaText = () => (document.body.querySelector('.replay-meta')?.textContent ?? '').replace(/\s+/g, ' ')

    // The fixture's aggregate: 24 requests, 240 in / 480 out — NOT the 10
    // blocks the opening actually loaded.
    const opening = metaText()
    expect(opening).toContain(`24 ${zhCopy.detailRequestsUnit}`)
    expect(opening).toContain('240')
    expect(opening).toContain('480')

    fireTop()
    await vi.waitFor(() => expect(blockIdsInOrder()).toEqual(ALL_IDS.slice(4)))
    expect(metaText(), 'after one older page the aggregate is unchanged').toBe(opening)

    fireTop()
    await vi.waitFor(() => expect(blockIdsInOrder()).toEqual(ALL_IDS))
    expect(metaText(), 'after reaching the head the aggregate is unchanged').toBe(opening)
  })
})

// ---------------------------------------------------------------------------
// TC-18 — system raw text: fetched exactly on first expand
// ---------------------------------------------------------------------------

describe('AgentSessionDetailPage lazy loading — system raw on expand (TC-18)', () => {
  const SYSTEM_META: AgentSessionTranscript['system'] = [
    { seen_at_request: 'b00', truncated: false, raw_length: 28 },
    { seen_at_request: 'b10', truncated: true, raw_length: 34 },
  ]

  function installSystemPages() {
    transcriptMock.mockImplementation(async (_sid: string, params?: { before?: string; after?: string }) => {
      if (!params || (!params.before && !params.after)) return pageOf(ALL_IDS.slice(14), true, 'b14', false, '', SYSTEM_META)
      if (params.before === 'b14') return pageOf(ALL_IDS.slice(4, 14), true, 'b04', true, 'b13', SYSTEM_META)
      return pageOf([], false, '', false, '', SYSTEM_META)
    })
    transcriptSystemMock.mockResolvedValue({
      system: [
        { seen_at_request: 'b00', truncated: false, raw: 'You are a helpful assistant.' },
        { seen_at_request: 'b10', truncated: true, raw: [{ type: 'text', text: 'Revised system prompt.' }] },
      ],
    })
  }

  it('zero raw-text requests while collapsed; exactly one on expand, kept on re-expand; the timeline renders complete', async () => {
    installSystemPages()
    await mountReplay()

    // Collapsed: metadata summary line only, and the raw-text endpoint
    // untouched — the first screen carries no system prompt text.
    const group = document.body.querySelector('.replay-system')
    expect(group, 'the system group renders from metadata').toBeTruthy()
    expect(transcriptSystemMock, 'no raw-text fetch while collapsed').not.toHaveBeenCalled()
    expect(document.body.textContent ?? '', 'prompt text absent while collapsed').not.toContain('You are a helpful assistant.')

    // Expand: exactly one raw-text fetch, then the full timeline.
    group!.querySelector('.n-collapse-item__header-main')?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(transcriptSystemMock, 'expanding fetches the raw timeline exactly once').toHaveBeenCalledTimes(1))
    expect(transcriptSystemMock.mock.calls[0][0]).toBe(SESSION)
    await vi.waitFor(() => expect(group!.querySelectorAll('.replay-system__entry').length, 'the timeline renders every entry').toBe(2))
    const entryHeaders = [...group!.querySelectorAll('.replay-system__entry .n-collapse-item__header')]
    expect(entryHeaders[0]?.textContent ?? '', 'first entry annotated').toContain(zhCopy.systemSeenAt.replace('{id}', 'b00'))
    expect(entryHeaders[1]?.textContent ?? '', 'changed entry annotated').toContain(zhCopy.systemSeenAt.replace('{id}', 'b10'))

    // Opening one entry reveals its prompt through the shared renderer.
    group!.querySelector('.replay-system__entry .n-collapse-item__header-main')?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(group!.textContent ?? '', 'the fetched raw prompt renders').toContain('You are a helpful assistant.'))

    // Re-collapse and re-expand: the raw text is kept, no second request.
    group!.querySelector('.n-collapse-item__header-main')?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(group!.querySelectorAll('.replay-system__entry').length).toBe(0))
    group!.querySelector('.n-collapse-item__header-main')?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() => expect(group!.querySelectorAll('.replay-system__entry').length).toBe(2))
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(transcriptSystemMock, 're-expanding does not refetch').toHaveBeenCalledTimes(1)

    // And paging the blocks never touches the raw-text endpoint either.
    fireTop()
    await vi.waitFor(() => expect(blockIdsInOrder()).toEqual(ALL_IDS.slice(4)))
    expect(transcriptSystemMock, 'block paging does not refetch the raw text').toHaveBeenCalledTimes(1)
  })
})
