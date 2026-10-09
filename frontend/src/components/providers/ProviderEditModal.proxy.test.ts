// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the provider edit dialog's outbound-proxy
// override fields (providers.proxy_mode / proxy_url):
//
//   - prefill: a provider with a custom stance opens the dialog with the
//     custom option selected and its URL in the box; an inherit provider
//     opens with the inherit option and an empty box — the read half of
//     the dialog round trip
//   - save: submitting carries proxy_mode/proxy_url in the PATCH payload
//     next to the unchanged name/base_url/protocol fields — the write half
//   - validation: a custom stance with an illegal URL blocks the submit
//     with the localized message and never PATCHes
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import naive, { NSelect } from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import { listProviders, updateProvider, type Provider } from '../../api/providers'

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return { ...actual, updateProvider: vi.fn(), listProviders: vi.fn() }
})

const updateProviderMock = vi.mocked(updateProvider)
const listProvidersMock = vi.mocked(listProviders)

// Must be imported after the vi.mock factory is registered.
import ProviderEditModal from './ProviderEditModal.vue'
import en from '../../locales/en'

const updateProviderInputOf = (input: unknown) => input as Record<string, unknown>

function baseProvider(overrides: Partial<Provider>): Provider {
  return {
    id: 3,
    name: 'p1',
    provider_type: 'openai',
    base_url: 'https://a.example.com',
    proxy_mode: '',
    proxy_url: '',
    note: '',
    management_status: 1,
    running_status: 'available',
    keys: [],
    created_at: '2026-01-01T00:00:00Z',
    protocol_endpoints: '',
    ...overrides,
  }
}

const Host = defineComponent({
  components: { ProviderEditModal },
  template:
    '<NConfigProvider><NMessageProvider><ProviderEditModal :show="show" :provider="provider" /></NMessageProvider></NConfigProvider>',
  props: {
    provider: { type: Object as () => Provider, required: true },
    show: { type: Boolean, default: false },
  },
})

let wrapper: VueWrapper | null = null

async function mountModal(provider: Provider) {
  const i18n = createI18n({ legacy: false, locale: 'en', fallbackLocale: 'en', messages: { en } })
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, {
    attachTo: document.body,
    props: { provider },
    global: { plugins: [pinia, i18n, naive] },
  })
  // The prefill watch keys on show/provider; in the real app the dialog
  // opens AFTER mount (show false → true), which is exactly what drives
  // the prefill. Reproduce that transition rather than mounting mid-open.
  await wrapper.setProps({ show: true })
  await nextTick()
  await nextTick()
}

function proxyModeSelect() {
  const el = document.body.querySelector<HTMLElement>('.provider-proxy__mode')
  expect(el, 'proxy mode select rendered').toBeTruthy()
  return el!
}

function proxyUrlInput(): HTMLInputElement {
  const input = document.body.querySelector<HTMLInputElement>('.provider-proxy__url input')
  expect(input, 'proxy URL input rendered').toBeTruthy()
  return input!
}

function setPlainInput(input: HTMLInputElement, value: string) {
  input.value = value
  input.dispatchEvent(new Event('input'))
}

function pickProxyMode(value: '' | 'off' | 'custom') {
  const modal = wrapper!.findComponent(ProviderEditModal)
  const select = modal.findAllComponents(NSelect).find((c) => c.classes().includes('provider-proxy__mode'))
  expect(select, 'proxy mode select component found').toBeTruthy()
  select!.vm.$emit('update:value', value)
}

function submit() {
  // The modal's footer teleports to body, so the confirm button is looked
  // up in the document, inside ModalDrawer's actions container.
  const buttons = Array.from(document.body.querySelectorAll<HTMLButtonElement>('.modal-drawer__actions button'))
  const confirm = buttons.filter((b) => (b.textContent ?? '').includes('Save'))
  expect(confirm.length, 'confirm button rendered').toBeGreaterThan(0)
  confirm[0]!.dispatchEvent(new Event('click'))
}

beforeEach(() => {
  updateProviderMock.mockReset()
  // The store's update() refreshes the list best-effort after a PATCH.
  listProvidersMock.mockResolvedValue({ list: [] } as never)
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('ProviderEditModal proxy override fields', () => {
  it('prefills a custom stance (mode + URL) from the provider row', async () => {
    await mountModal(
      baseProvider({ proxy_mode: 'custom', proxy_url: 'socks5://192.168.1.5:1080' }),
    )
    expect(proxyModeSelect().textContent).toContain(en.providers.proxyModeCustom)
    expect(proxyUrlInput().value).toBe('socks5://192.168.1.5:1080')
    expect(proxyUrlInput().disabled).toBe(false)
  })

  it('prefills the inherit stance (empty mode, empty URL) for a fresh provider', async () => {
    await mountModal(baseProvider({}))
    expect(proxyModeSelect().textContent).toContain(en.providers.proxyModeInherit)
    expect(proxyUrlInput().value).toBe('')
    // Outside custom mode the URL box is visible but disabled — a parked
    // URL stays readable instead of vanishing.
    expect(proxyUrlInput().disabled).toBe(true)
  })

  it('submits the proxy pair with the PATCH payload when the stance is custom', async () => {
    updateProviderMock.mockResolvedValue(baseProvider({}) as never)
    await mountModal(baseProvider({}))
    pickProxyMode('custom')
    await nextTick()
    setPlainInput(proxyUrlInput(), 'socks5h://10.0.0.9:1080')
    await nextTick()
    submit()
    await vi.waitFor(() => expect(updateProviderMock).toHaveBeenCalledTimes(1))
    const [id, input] = updateProviderMock.mock.calls[0]!
    expect(id).toBe(3)
    const payload = updateProviderInputOf(input)
    expect(payload.proxy_mode).toBe('custom')
    expect(payload.proxy_url).toBe('socks5h://10.0.0.9:1080')
    // The unchanged basics ride along, same as before the proxy fields
    // existed.
    expect(payload.name).toBe('p1')
    expect(payload.base_url).toBe('https://a.example.com')
  })

  it('submits the inherit stance when the mode is put back to inherit', async () => {
    updateProviderMock.mockResolvedValue(baseProvider({}) as never)
    await mountModal(baseProvider({ proxy_mode: 'custom', proxy_url: 'socks5://192.168.1.5:1080' }))
    pickProxyMode('')
    await nextTick()
    // The old URL stays in the (now disabled) box and rides along — the
    // server stores the pair as given; clearing the URL is the admin's
    // explicit choice in the box, not a side effect of the mode.
    submit()
    await vi.waitFor(() => expect(updateProviderMock).toHaveBeenCalledTimes(1))
    const [, input] = updateProviderMock.mock.calls[0]!
    const payload = updateProviderInputOf(input)
    expect(payload.proxy_mode).toBe('')
    expect(payload.proxy_url).toBe('socks5://192.168.1.5:1080')
  })

  it.each([
    { input: 'ftp://proxy.example.com:21', message: en.providers.proxyUrlScheme },
    { input: 'proxy.example.com:7890', message: en.providers.proxyUrlScheme },
    { input: '', message: en.providers.proxyUrlRequired },
  ])('an illegal URL "$input" under custom mode blocks the submit with its own message', async (draft) => {
    await mountModal(baseProvider({}))
    pickProxyMode('custom')
    await nextTick()
    setPlainInput(proxyUrlInput(), draft.input)
    await nextTick()
    submit()
    await nextTick()
    await nextTick()
    expect(document.body.textContent ?? '', 'the form shows the issue message').toContain(draft.message)
    expect(updateProviderMock, 'an illegal proxy URL never reaches the wire').not.toHaveBeenCalled()
  })
})
