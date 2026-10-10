// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the conversation-bubble renderer (zh-CN
// locale; both-locale parity of every key used here is exercised by the
// locale parity gate). The six bubble-rendering arms:
//
//   role sides       assistant bubbles sit right, every other role left
//   image part       placeholder block (the payload never enters the DOM)
//   tool part        the type label pill (+ highlighted payload block)
//   JSON fallback    collapsible formatted-JSON view for bodies that
//                    cannot be read as chat
//   truncation hint  the inline-cut marker line
//   stream merge     an SSE capture fed through the stream translator
//                    renders as ONE readable assistant bubble
//   long-part clamp  a wall-of-text part renders clamped behind a
//                    disclosure row that expands/collapses on click
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { defineComponent, h, nextTick, type Component } from 'vue'
import naive from 'naive-ui'

import { translateStreamBody, type TranslatedBody } from '../../utils/messageTranslator'
import en from '../../locales/en'
import zhCN from '../../locales/zh-CN'

import ChatMessageFlow from './ChatMessageFlow.vue'

let wrapper: VueWrapper | null = null

// Give every mount the same provider ancestry App.vue provides
// (NConfigProvider > NMessageProvider).
const Host = defineComponent({
  template: '<NConfigProvider><NMessageProvider><slot /></NMessageProvider></NConfigProvider>',
})

// Desktop matchMedia stub for any composable the bubble tree pulls in.
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

// Mount the component with props under the provider host.
async function mountFlow(props: Record<string, unknown>) {
  const i18n = createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { en, 'zh-CN': zhCN } })
  wrapper = mount(Host, {
    attachTo: document.body,
    global: { plugins: [i18n, naive] },
    slots: { default: () => h(ChatMessageFlow as Component, props) },
  })
  await nextTick()
  await nextTick()
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

describe('ChatMessageFlow', () => {
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
    await mountFlow({ body, raw: '' })
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
    await mountFlow({ body, raw })
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
    // An OpenAI SSE capture as the on-disk .stream file holds it; callers
    // feed exactly this string to the same translator before handing the
    // result to this component.
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
    await mountFlow({ body, raw: '' })
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
    await mountFlow({ body, raw: '' })
    const textEl = document.body.querySelector('.msg-flow__text')
    expect(textEl?.classList.contains('msg-flow__text--clamped'), 'long part starts clamped').toBe(true)
    const btn = document.body.querySelector('.msg-flow__disclose-btn') as HTMLButtonElement | null
    expect(btn?.textContent, 'disclose row offers expand in the zh locale').toContain(zhCN.agentSessions.expandFull)
    expect(document.body.querySelector('.msg-flow__disclose-meta')?.textContent, 'size note carries the character count').toContain(
      long.length.toLocaleString(),
    )
    btn?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await vi.waitFor(() =>
      expect(document.body.querySelector('.msg-flow__text')?.classList.contains('msg-flow__text--clamped'), 'expanded clears the clamp').toBe(false),
    )
    expect(document.body.querySelector('.msg-flow__disclose-btn')?.textContent, 'toggle flips to collapse').toContain(zhCN.agentSessions.collapse)
  })

  it('renders the placeholder note for a body that was never recorded inline', async () => {
    const body: TranslatedBody = { kind: 'placeholder', messages: [], truncated: false }
    await mountFlow({ body, raw: '' })
    expect(document.body.querySelector('.msg-flow__note')?.textContent, 'placeholder note').toContain(zhCN.requestLogs.bodyNotRecorded)
  })
})
