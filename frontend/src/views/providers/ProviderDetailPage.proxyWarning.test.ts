// @vitest-environment happy-dom
//
// The provider detail page's proxy-warning suite (TC-16, component leg):
// the header carries the warning icon exactly when this provider's stance
// resolves to proxied — explicit custom decides its own fate, off stays
// bare, and an inheriting stance resolves against the global setting the
// page fetches on mount. Same fixture shape as the page's other suites;
// assertions read the icon through its stable testid and the locale
// module's own tooltip copy.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import { getProvider, type Provider, type ProviderKey } from '../../api/providers'
import { listProviderCandidates } from '../../api/models'
import { getOutboundProxy } from '../../api/systemSettings'

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return { ...actual, getProvider: vi.fn() }
})

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return { ...actual, listProviderCandidates: vi.fn(), retestCandidate: vi.fn() }
})

vi.mock('../../api/systemSettings', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/systemSettings')>()
  return { ...actual, getOutboundProxy: vi.fn() }
})

const getProviderMock = vi.mocked(getProvider)
const listCandidatesMock = vi.mocked(listProviderCandidates)
const getProxyMock = vi.mocked(getOutboundProxy)

// Must be imported after the vi.mock factories are registered.
import ProviderDetailPage from './ProviderDetailPage.vue'
import zhCN from '../../locales/zh-CN'
import { CANDIDATE_STATUS_ENABLED, VERIFICATION_PASSED } from '../../api/candidateStatus'

const PROVIDER_ID = 5

// App.vue nests pages under n-config-provider > n-message-provider >
// n-dialog-provider; the page's useMessage()/useDialog() throw without that
// ancestry.
const Host = defineComponent({
  components: { ProviderDetailPage },
  template:
    '<NConfigProvider><NMessageProvider><NDialogProvider><ProviderDetailPage /></NDialogProvider></NMessageProvider></NConfigProvider>',
})

let wrapper: VueWrapper | null = null

// Desktop viewport: the header's desktop action row is the branch under
// test (the icon rides the header title either way).
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

function keyFixture(): ProviderKey {
  return {
    id: 1,
    label: 'primary',
    key_prefix: 'sk-abcd',
    key_suffix: 'wxyz',
    sort_order: 1,
    test_model: 'gpt-x',
    management_status: CANDIDATE_STATUS_ENABLED,
    verification_status: VERIFICATION_PASSED,
    needs_reentry: false,
    last_test_result: 0,
    last_test_model: 'gpt-x',
    last_test_duration_ms: 120,
    last_tested_at: '2026-09-01T00:00:00Z',
    last_test_targets: null,
  }
}

function providerFixture(proxyMode: string): Provider {
  return {
    id: PROVIDER_ID,
    name: 'demo-provider',
    provider_type: 'openai',
    base_url: 'https://api.example.com',
    note: '',
    management_status: CANDIDATE_STATUS_ENABLED,
    running_status: 'available',
    keys: [keyFixture()],
    created_at: '2026-09-01T00:00:00Z',
    protocol_endpoints: '',
    proxy_mode: proxyMode,
    proxy_url: proxyMode === 'custom' ? 'socks5://10.0.0.9:1080' : '',
  }
}

async function mountDetail(proxyMode: string, globalMode: 'off' | 'custom' | 'system') {
  getProviderMock.mockResolvedValue(providerFixture(proxyMode))
  listCandidatesMock.mockResolvedValue({ list: [] })
  getProxyMock.mockResolvedValue({ mode: globalMode, url: '', version: 1 })
  const i18n = createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { 'zh-CN': zhCN } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/providers', component: { template: '<div />' } },
      { path: '/providers/:id', component: { template: '<div />' } },
    ],
  })
  await router.push(`/providers/${PROVIDER_ID}`)
  await router.isReady()
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(zhCN.providers.tabKeys))
  await nextTick()
}

function warningIcons(): HTMLElement[] {
  return [...document.body.querySelectorAll<HTMLElement>('[data-testid="provider-proxy-warning"]')]
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

describe('ProviderDetailPage proxy warning icon', () => {
  it('carries the icon in the header for an explicit custom stance, whatever the global', async () => {
    await mountDetail('custom', 'off')
    expect(warningIcons(), 'custom resolves proxied even under a direct global').toHaveLength(1)
    expect(warningIcons()[0]!.getAttribute('aria-label')).toBe(zhCN.providers.proxyWarningTooltip)
  })

  it('stays bare for an explicit off stance under a proxied global', async () => {
    await mountDetail('off', 'custom')
    expect(warningIcons()).toHaveLength(0)
  })

  it('resolves an inheriting stance against the global setting', async () => {
    await mountDetail('', 'custom')
    expect(warningIcons(), 'inherit under custom global is proxied').toHaveLength(1)
  })

  it('stays bare for an inheriting stance when the global is off', async () => {
    await mountDetail('', 'off')
    expect(warningIcons()).toHaveLength(0)
  })
})
