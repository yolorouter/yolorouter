// @vitest-environment happy-dom
//
// Mounted-DOM tests for the disclosure tag: they click the real rendered
// trigger and assert the panel actually appears. This is the one behavior no
// pure-function test can see — the popover wiring silently breaks when a
// wrapper node lands between the popover and its tooltip trigger, while
// every type check and unit test stays green.
import { afterEach, describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import { createI18n } from 'vue-i18n'
import KeyTestTargetsTag from './KeyTestTargetsTag.vue'
import en from '../../locales/en'

const targets = [
  { proto: 'openai', outcome: 0, duration_ms: 12, detail: '' },
  { proto: 'anthropic', outcome: 7, duration_ms: 34, detail: 'HTTP 400: unsupported endpoint' },
]

function mountTag(hint: string) {
  const i18n = createI18n({ legacy: false, locale: 'en', messages: { en } })
  return mount(KeyTestTargetsTag, {
    props: { text: 'Upstream error', type: 'error', hint, targets },
    global: { plugins: [i18n] },
    attachTo: document.body,
  })
}

// The popover body teleports out of the component subtree, so the panel is
// asserted against the whole document.
function panelVisible(): boolean {
  return (document.body.textContent ?? '').includes(en.providers.keyTestTargetsTitle)
}

afterEach(() => {
  document.body.innerHTML = ''
})

describe('KeyTestTargetsTag', () => {
  it('opens the per-protocol panel on click when the tag carries a hint', async () => {
    // The hinted path wraps the tag in a tooltip — historically the fragile
    // one, since the popover must hand its click through the tooltip.
    const wrapper = mountTag('expand for details')
    const trigger = wrapper.get('[role="button"]')
    expect(trigger.attributes('aria-expanded')).toBe('false')
    expect(panelVisible()).toBe(false)

    await trigger.trigger('click')
    await nextTick()

    expect(panelVisible()).toBe(true)
    expect(document.body.textContent).toContain('HTTP 400: unsupported endpoint')
    expect(trigger.attributes('aria-expanded')).toBe('true')
    wrapper.unmount()
  })

  it('opens the panel on click when the tag has no hint', async () => {
    const wrapper = mountTag('')
    const trigger = wrapper.get('[role="button"]')
    await trigger.trigger('click')
    await nextTick()
    expect(panelVisible()).toBe(true)
    wrapper.unmount()
  })

  it('opens the panel from the keyboard', async () => {
    const wrapper = mountTag('expand for details')
    const trigger = wrapper.get('[role="button"]')
    await trigger.trigger('keydown', { key: 'Enter' })
    await nextTick()
    expect(panelVisible()).toBe(true)
    wrapper.unmount()
  })
})
