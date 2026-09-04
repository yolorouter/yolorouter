// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the apiKey injection contract the create-key
// dialog relies on: with a real credential passed in, every rendered
// sample carries it and none carries the placeholder.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent, h } from 'vue'
import { createI18n } from 'vue-i18n'
import { NMessageProvider } from 'naive-ui'

// Resolved before the modal imports the composable, so samples build
// against a deterministic address.
vi.mock('../../api/system', () => ({
  getSystemEndpoint: () => Promise.resolve({ endpoint: 'https://gateway.example.com' }),
}))

import ApiExamplesModal from './ApiExamplesModal.vue'
import en from '../../locales/en'

const i18n = createI18n({ legacy: false, locale: 'en', messages: { en } })

// useMessage needs a provider above the modal; a render-function wrapper
// avoids the runtime template compiler.
const Host = defineComponent({
  render() {
    return h(NMessageProvider, () =>
      h(ApiExamplesModal, { show: true, apiKey: 'sk-fresh-987' }),
    )
  },
})

afterEach(() => {
  document.body.innerHTML = ''
})

describe('ApiExamplesModal apiKey injection', () => {
  it('renders every sample with the real key and no placeholder', async () => {
    const wrapper = mount(Host, {
      global: { plugins: [i18n] },
      attachTo: document.body,
    })
    await vi.waitFor(() =>
      expect(document.body.textContent ?? '').toContain(en.apiKeys.examplesTitle),
    )
    const text = document.body.textContent ?? ''
    expect(text).not.toContain('<API Key>')
    expect(text).toContain('sk-fresh-987')
    wrapper.unmount()
  })
})
