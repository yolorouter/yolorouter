// @vitest-environment happy-dom
//
// Mounted-DOM tests for the access-info panel and its examples modal: they
// click the real rendered buttons and assert the teleported dialog actually
// appears with the right group, that language tabs switch, and that copy is
// wired to the clipboard util. This is the wiring no pure-function test can
// see — the endpoint composable, the modal landing tab, and the copy
// plumbing can all silently break while every unit test stays green.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent, h, nextTick } from 'vue'
import { createI18n } from 'vue-i18n'
import { NMessageProvider } from 'naive-ui'

// Resolved before any component imports the composable, so the catalog
// builds against a deterministic address instead of the origin fallback.
vi.mock('../../api/system', () => ({
  getSystemEndpoint: () => Promise.resolve({ endpoint: 'https://gateway.example.com' }),
}))

const copyToClipboard = vi.fn(async (_text: string) => true)
vi.mock('../../utils/clipboard', () => ({
  copyToClipboard: (...args: unknown[]) => copyToClipboard(...(args as [string])),
}))

import ApiAccessPanel from './ApiAccessPanel.vue'
import en from '../../locales/en'

const GATEWAY = 'https://gateway.example.com'

const i18n = createI18n({ legacy: false, locale: 'en', messages: { en } })

// useMessage needs a provider above the panel; a render-function wrapper
// avoids the runtime template compiler.
const Host = defineComponent({
  render() {
    return h(NMessageProvider, () => h(ApiAccessPanel))
  },
})

function mountPanel() {
  return mount(Host, {
    global: { plugins: [i18n] },
    attachTo: document.body,
  })
}

// The modal teleports to body, so its content is asserted against the whole
// document — same recipe as the KeyTestTargetsTag tests.
function bodyText(): string {
  return document.body.textContent ?? ''
}

async function clickByExampleLabel(label: string) {
  const buttons = Array.from(document.querySelectorAll('button')).filter(
    (b) => b.textContent?.trim() === label,
  )
  expect(buttons.length).toBeGreaterThan(0)
  buttons[0].click()
  await nextTick()
  await nextTick()
}

beforeEach(() => {
  copyToClipboard.mockClear()
})

afterEach(() => {
  document.body.innerHTML = ''
})

describe('ApiAccessPanel', () => {
  it('shows one base-URL row per entry protocol', async () => {
    const wrapper = mountPanel()
    // The endpoint resolves asynchronously; wait for it before asserting the
    // rows, or the origin fallback is still on screen.
    await vi.waitFor(() => expect(wrapper.text()).toContain(GATEWAY))
    const text = wrapper.text()
    expect(text).toContain('OpenAI-compatible')
    expect(text).toContain('Anthropic-compatible')
    expect(text).toContain('Gemini-compatible')
    expect(text).toContain(`${GATEWAY}/v1`)
    expect(text).toContain(`${GATEWAY}/v1beta`)
    wrapper.unmount()
  })

  it('opens the examples modal on the clicked protocol with highlighted, copyable samples', async () => {
    const wrapper = mountPanel()
    await vi.waitFor(() => expect(wrapper.text()).toContain(GATEWAY))
    await clickByExampleLabel('Examples')
    expect(bodyText()).toContain(en.apiKeys.examplesTitle)
    expect(bodyText()).toContain(en.apiKeys.exampleGroupChat)
    // Landing tab is curl: the full request URL is on screen.
    expect(bodyText()).toContain(`curl ${GATEWAY}/v1/chat/completions`)

    // Language tab switch brings up the Python SDK sample.
    const pythonTab = Array.from(document.querySelectorAll('.n-tabs-tab')).find(
      (el) => el.textContent?.trim() === 'Python',
    )
    expect(pythonTab).toBeTruthy()
    pythonTab!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await nextTick()
    await nextTick()
    expect(bodyText()).toContain(`base_url="${GATEWAY}/v1"`)

    // The per-block copy button routes the snippet through the clipboard util.
    const copyButton = document.querySelector<HTMLButtonElement>(
      '.example-block__head button',
    )
    expect(copyButton).toBeTruthy()
    copyButton!.click()
    await nextTick()
    expect(copyToClipboard).toHaveBeenCalled()
    expect(copyToClipboard.mock.calls[0][0]).toContain(GATEWAY)

    // Switching the outer group tab lands on that capability's samples.
    const imageTab = Array.from(document.querySelectorAll('.n-tabs-tab')).find(
      (el) => el.textContent?.trim() === en.apiKeys.exampleGroupImageGeneration,
    )
    expect(imageTab).toBeTruthy()
    imageTab!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await nextTick()
    await nextTick()
    expect(bodyText()).toContain(`${GATEWAY}/v1/images/generations`)
    wrapper.unmount()
  })

  it('lands the modal on the protocol of the clicked row', async () => {
    const wrapper = mountPanel()
    await vi.waitFor(() => expect(wrapper.text()).toContain(GATEWAY))
    const buttons = Array.from(
      document.querySelectorAll<HTMLButtonElement>('.endpoint-row__example'),
    )
    expect(buttons).toHaveLength(3)

    buttons[1]!.click()
    await nextTick()
    await nextTick()
    expect(bodyText()).toContain(`curl ${GATEWAY}/v1/messages`)
    expect(bodyText()).toContain('x-api-key: <API Key>')

    // Reopen via the Gemini row without closing first: the landing tab
    // follows the click, not the previous open.
    buttons[2]!.click()
    await nextTick()
    await nextTick()
    expect(bodyText()).toContain(`curl ${GATEWAY}/v1beta/models/<model>:generateContent`)
    wrapper.unmount()
  })
})
