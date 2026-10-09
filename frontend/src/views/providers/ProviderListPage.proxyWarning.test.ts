// @vitest-environment happy-dom
//
// The provider list page's proxy-warning suite (TC-16, component leg): the
// rows whose outbound traffic resolves to proxied — an explicit custom
// stance, or an inheriting stance under a non-off global — carry the
// warning icon with its tooltip copy; explicit off and inherit-under-off
// rows carry none. The icon count and its accessible text are asserted on
// the mounted DOM, in both locales, with the global setting served by the
// real GET the page fires (so the inherit arm is exercised end to end
// through useGlobalProxyMode, not through a stubbed resolver).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import { listProviders, type Provider } from '../../api/providers'
import { listModels } from '../../api/models'
import { getOutboundProxy } from '../../api/systemSettings'

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return { ...actual, listProviders: vi.fn() }
})

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return { ...actual, listModels: vi.fn() }
})

vi.mock('../../api/systemSettings', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/systemSettings')>()
  return { ...actual, getOutboundProxy: vi.fn() }
})

const listMock = vi.mocked(listProviders)
const listModelsMock = vi.mocked(listModels)
const getProxyMock = vi.mocked(getOutboundProxy)

// Must be imported after the vi.mock factories are registered.
import ProviderListPage from './ProviderListPage.vue'
import zhCN from '../../locales/zh-CN'
import en from '../../locales/en'

// The list page renders inside the app's provider ancestry (message +
// dialog), which useMessage()/useDialog() require.
const Host = defineComponent({
  components: { ProviderListPage },
  template:
    '<NConfigProvider><NMessageProvider><NDialogProvider><ProviderListPage /></NDialogProvider></NMessageProvider></NConfigProvider>',
})

let wrapper: VueWrapper | null = null

// Desktop viewport: the table branches (whose name cells host the icon)
// render when matchMedia reports wide.
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

function providerFixture(id: number, name: string, proxyMode: string): Provider {
  return {
    id,
    name,
    provider_type: 'openai',
    base_url: `https://${name}.example.com`,
    note: '',
    management_status: 1,
    running_status: 'available',
    keys: [],
    created_at: '2026-09-01T00:00:00Z',
    protocol_endpoints: '',
    proxy_mode: proxyMode,
    proxy_url: proxyMode === 'custom' ? 'socks5://10.0.0.9:1080' : '',
  }
}

// One row per stance: custom decides its own fate, off decides its own, and
// inherit resolves against whatever the global GET serves.
const ROWS = () => [
  providerFixture(1, 'explicit-custom', 'custom'),
  providerFixture(2, 'explicit-off', 'off'),
  providerFixture(3, 'inheriting', ''),
]

async function mountList(
  locale: 'zh-CN' | 'en',
  globalMode: 'off' | 'custom' | 'system',
  rows: Provider[] = ROWS(),
) {
  listMock.mockResolvedValue({ list: rows })
  listModelsMock.mockResolvedValue({ list: [] })
  getProxyMock.mockResolvedValue({ mode: globalMode, url: '', version: 1 })
  const i18n = createI18n({ legacy: false, locale, fallbackLocale: 'zh-CN', messages: { 'zh-CN': zhCN, en } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/providers/:id', component: { template: '<div />' } },
    ],
  })
  await router.push('/')
  await router.isReady()
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  // Wait for the RENDERED rows (the table flushes a few ticks after the
  // list resolves) plus the icon's verdict on the global setting.
  await vi.waitFor(() => {
    const text = document.body.textContent ?? ''
    for (const row of rows) {
      expect(text).toContain(row.name)
    }
  })
  await nextTick()
  await nextTick()
}

function warningIcons(): HTMLElement[] {
  return [...document.body.querySelectorAll<HTMLElement>('[data-testid="provider-proxy-warning"]')]
}

function rowOf(name: string): HTMLTableRowElement | null {
  return (
    [...document.body.querySelectorAll('tr')].find((el) => (el.textContent ?? '').includes(name)) ?? null
  )
}

beforeEach(() => {
  stubDesktopMatchMedia()
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  document.body.innerHTML = ''
  vi.unstubAllGlobals()
  vi.clearAllMocks()
})

describe('ProviderListPage proxy warning icon', () => {
  it('marks custom and inherit-under-proxied rows, not off or inherit-under-off (zh-CN)', async () => {
    await mountList('zh-CN', 'custom')
    // Global custom: explicit-custom and the inheriting row are proxied;
    // explicit-off rides direct.
    expect(warningIcons(), 'custom + inherit rows carry the icon').toHaveLength(2)
    expect(rowOf('explicit-custom')!.querySelector('[data-testid="provider-proxy-warning"]')).not.toBeNull()
    expect(rowOf('explicit-off')!.querySelector('[data-testid="provider-proxy-warning"]')).toBeNull()
    expect(rowOf('inheriting')!.querySelector('[data-testid="provider-proxy-warning"]')).not.toBeNull()
  })

  it('hides the icon for off and inherit rows when the global is off (zh-CN)', async () => {
    // The off-global leg drops the explicit-custom row: what remains is
    // exactly the population the case is about (off and inherit), so
    // "no icon anywhere" is the assertion this fixture can honestly make.
    await mountList('zh-CN', 'off', [providerFixture(2, 'explicit-off', 'off'), providerFixture(3, 'inheriting', '')])
    expect(warningIcons(), 'off/inherit-off rows carry no icon').toHaveLength(0)
    expect(rowOf('explicit-off')!.querySelector('[data-testid="provider-proxy-warning"]')).toBeNull()
    expect(rowOf('inheriting')!.querySelector('[data-testid="provider-proxy-warning"]')).toBeNull()
  })

  it('marks the inheriting row under the system global too (zh-CN)', async () => {
    await mountList('zh-CN', 'system')
    expect(warningIcons()).toHaveLength(2)
    expect(rowOf('inheriting')!.querySelector('[data-testid="provider-proxy-warning"]')).not.toBeNull()
  })

  it.each([['zh-CN'], ['en']] as const)('carries the localized tooltip copy (%s)', async (locale) => {
    await mountList(locale, 'custom')
    const icon = warningIcons()[0]
    expect(icon, 'at least one icon rendered').toBeTruthy()
    // The accessible name is the tooltip copy verbatim — the same string the
    // hover tooltip renders, asserted through the locale module so a copy
    // edit cannot fake a regression.
    const messages = locale === 'zh-CN' ? zhCN : en
    const expected = (messages.providers as Record<string, string>).proxyWarningTooltip
    expect(icon!.getAttribute('aria-label')).toBe(expected)
    expect(expected, 'the locale carries the entry').toBeTruthy()
  })
})
