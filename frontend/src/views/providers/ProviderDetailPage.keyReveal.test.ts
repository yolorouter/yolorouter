// @vitest-environment happy-dom
//
// The reveal-and-copy wiring for a provider key, on BOTH surfaces the batch
// added it to: the key row's actions menu and the edit modal's stored-key
// copy row. The contract under test is copy-ONLY — the plaintext travels
// from the reveal response through one local variable into the clipboard
// util and must never surface anywhere else. Assertions run in both
// directions:
//
//   forward  — the clipboard util receives the FULL plaintext (the api
//              module is mocked, so this pins the response-to-clipboard
//              plumbing, same recipe as ApiAccessPanel.test.ts), and the
//              「已复制」 toast renders (tier pinned, not just copy)
//   reverse  — after the copy settles and the edit modal has been open and
//              closed, the plaintext substring appears nowhere in the page's
//              HTML (wrapper + teleported body) nor in ANY Pinia store's
//              $state (enumerated off the active pinia, so a future store
//              is covered by construction)
//
// Covered acceptance cases:
//
//   TC-03 the copy chain, forward + the three-layer reverse
//   TC-08 close-and-reopen the edit modal after a copy: no residue, the
//         modal's plaintext field restarts empty, the list keeps rendering
//   TC-09 the copy copies exist in BOTH locales (tree parity itself is the
//         parity suite, which the same vitest run executes)
//
// The failure branches the handlers grew are pinned just as hard (repo rule:
// a new branch ships with the test that goes red without it): a refused
// clipboard write toasts copyFailed on BOTH surfaces, a refused reveal
// surfaces its APIError message on both surfaces without reaching the
// clipboard, and a session-expired reveal redirects to /login instead of
// toasting.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, getActivePinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import { getProvider, revealProviderKey, type Provider, type ProviderKey } from '../../api/providers'
import { listProviderCandidates, type ProviderCandidate } from '../../api/models'
import { APIError } from '../../api/client'
import { ACCOUNT_SESSION_INVALID } from '../../api/errcodes'
import { useAuthStore } from '../../store/auth'

vi.mock('../../api/providers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/providers')>()
  return { ...actual, getProvider: vi.fn(), revealProviderKey: vi.fn() }
})

// The page header's proxy-warning icon resolves its stance from the global
// outbound-proxy setting, fetched on every mount — mock it to the direct
// (off) stance so the icon stays bare and no suite here reaches the wire.
import { getOutboundProxy } from '../../api/systemSettings'

vi.mock('../../api/systemSettings', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/systemSettings')>()
  return { ...actual, getOutboundProxy: vi.fn() }
})
vi.mocked(getOutboundProxy).mockResolvedValue({ mode: 'off', url: '', version: 0 })

vi.mock('../../api/models', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/models')>()
  return { ...actual, listProviderCandidates: vi.fn() }
})

// The clipboard util is the seam the plaintext must reach — and the ONLY
// place it may go. Mocked (ApiAccessPanel.test.ts precedent) so the tests
// assert what was handed to it rather than a browser clipboard.
const copyToClipboard = vi.fn(async (_text: string) => true)
vi.mock('../../utils/clipboard', () => ({
  copyToClipboard: (...args: unknown[]) => copyToClipboard(...(args as [string])),
}))

const getProviderMock = vi.mocked(getProvider)
const revealMock = vi.mocked(revealProviderKey)
const listCandidatesMock = vi.mocked(listProviderCandidates)

// Must be imported after the vi.mock factories are registered.
import ProviderDetailPage from './ProviderDetailPage.vue'
import zhCN from '../../locales/zh-CN'
import en from '../../locales/en'
import { CANDIDATE_STATUS_ENABLED, VERIFICATION_PASSED } from '../../api/candidateStatus'

const PROVIDER_ID = 5
// Distinctive on purpose: every zero-plaintext assertion below searches for
// this exact substring, so it must not collide with fixture labels. The tail
// matches the fixture's key_suffix — the suffix IS the plaintext's last 4,
// which is exactly why the row can show "sk-abcd***wxyz" while the full
// string must appear nowhere.
const PLAINTEXT = 'sk-live-reveal-6Kq9kR2mV8xw3Nf7Yt5Zp4Qd1Jh6Ls0Ue7Ob2Wi9Cxwxyz'
const SUFFIX = 'wxyz'

// App.vue nests pages under n-config-provider > n-message-provider >
// n-dialog-provider; the page's useMessage()/useDialog() throw without that
// ancestry.
const Host = defineComponent({
  components: { ProviderDetailPage },
  template:
    '<NConfigProvider><NMessageProvider><NDialogProvider><ProviderDetailPage /></NDialogProvider></NMessageProvider></NConfigProvider>',
})

let wrapper: VueWrapper | null = null

// Desktop viewport (same stub shape as the other page tests): the desktop
// table branches are the ones asserted.
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

function providerFixture(keys: ProviderKey[] = [keyFixture()]): Provider {
  return {
    id: PROVIDER_ID,
    name: 'demo-provider',
    provider_type: 'openai',
    base_url: 'https://api.example.com',
    note: '',
    management_status: CANDIDATE_STATUS_ENABLED,
    running_status: 'available',
    keys,
    created_at: '2026-09-01T00:00:00Z',
    protocol_endpoints: '',
    proxy_mode: '',
    proxy_url: '',
  }
}

// A settled, non-queued mapping row (nothing the detail page would adopt for
// background polling — these tests stay deterministic in their fetch counts).
function candidateRow(): ProviderCandidate {
  return {
    candidate_id: 1,
    model_id: 100,
    model_name: 'model-one',
    provider_model_name: 'upstream-one',
    input_price: 1.5,
    output_price: 2.5,
    cache_write_price: null,
    cache_read_price: null,
    billing_mode: 'token',
    image_pricing_tiers: null,
    video_pricing_tiers: null,
    audio_unit_price: null,
    max_output: 4096,
    management_status: CANDIDATE_STATUS_ENABLED,
    verification_status: VERIFICATION_PASSED,
    last_test_result: 0,
    last_tested_at: '2026-09-01T00:00:00Z',
    last_test_error: null,
    queue_state: '',
    auto_enable_on_pass: false,
  }
}

// Returns the router the page was mounted under, so tests can assert where
// navigation ended up (the session-expiry branch pushes /login).
async function mountPage() {
  getProviderMock.mockResolvedValue(providerFixture())
  listCandidatesMock.mockResolvedValue({ list: [candidateRow()] })
  revealMock.mockResolvedValue({ plaintext_key: PLAINTEXT })
  const i18n = createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { 'zh-CN': zhCN } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/login', component: { template: '<div />' } },
      { path: '/providers', component: { template: '<div />' } },
      { path: '/providers/:id', component: { template: '<div />' } },
    ],
  })
  await router.push(`/providers/${PROVIDER_ID}`)
  await router.isReady()
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  // Wait for the RENDERED page, not just the fetches: the keys table flushes
  // a few ticks after the API call resolves.
  await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(zhCN.providers.tabKeys))
  await nextTick()
  return router
}

// The keys-table body row rendering a key, located by its label (unique in
// the fixtures here).
function keyRowTr(label: string): HTMLTableRowElement {
  const tr = [...document.body.querySelectorAll('tr')].find((el) => (el.textContent ?? '').includes(label))
  expect(tr, `key row for ${label} rendered`).toBeTruthy()
  return tr!
}

// Opens a key row's '⋯' actions menu the way a user does. The actions column
// is the LAST cell of the row, so its trigger is the last cell's button —
// the reorder column ahead of it carries two buttons of its own.
async function openKeyRowMenu(label: string): Promise<string[]> {
  const trigger = keyRowTr(label).querySelector('td:last-of-type button')
  expect(trigger, `the actions trigger on key ${label}'s row rendered`).toBeTruthy()
  trigger!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await nextTick()
  await nextTick()
  const menus = [...document.body.querySelectorAll('.n-dropdown-menu')]
  const menu = menus[menus.length - 1]
  expect(menu, `the actions menu for key ${label} opened`).toBeTruthy()
  return [...menu!.querySelectorAll('.n-dropdown-option')]
    .filter((opt) => !opt.classList.contains('n-dropdown-option--divider'))
    .map((opt) => (opt.textContent ?? '').trim())
}

// Clicks the currently open menu's item by its visible label. naive-ui binds
// the click handler on the option's inner body element, so the event must
// originate there (same recipe as candidateActions).
async function clickMenuItem(label: string) {
  const menus = [...document.body.querySelectorAll('.n-dropdown-menu')]
  const menu = menus[menus.length - 1]
  const item = [...menu!.querySelectorAll('.n-dropdown-option')].find(
    (opt) => (opt.textContent ?? '').trim() === label,
  )
  expect(item, `menu item "${label}" rendered`).toBeTruthy()
  const target = item!.querySelector('.n-dropdown-option-body') ?? item!
  target.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await nextTick()
  await nextTick()
}

// The most recently opened naive modal (the key edit modal teleports to
// body).
function openModal(): Element {
  const modals = [...document.body.querySelectorAll('.n-modal')]
  const modal = modals[modals.length - 1]
  expect(modal, 'the key edit modal rendered').toBeTruthy()
  return modal!
}

// A button inside the given container by its visible label.
function buttonByLabel(container: Element, label: string): HTMLButtonElement {
  const btn = [...container.querySelectorAll('button')].find((b) => (b.textContent ?? '').trim() === label)
  expect(btn, `button "${label}" rendered`).toBeTruthy()
  return btn!
}

// The three-layer reverse assertion's helpers — a toast's TIER is pinned, not
// just its copy (same discipline as the batch-delete tests).
function successToastSaying(text: string) {
  const el = [...document.body.querySelectorAll('.n-message--success-type')].find((e) =>
    (e.textContent ?? '').includes(text),
  )
  expect(el, `a success toast saying "${text}"`).toBeTruthy()
}

// The failure-toast twin: a copy that wrongly toasts SUCCESS on a failed
// write would keep a bare text assertion green if the copies ever drifted
// together, so the tier is pinned here too.
function errorToastSaying(text: string) {
  const el = [...document.body.querySelectorAll('.n-message--error-type')].find((e) =>
    (e.textContent ?? '').includes(text),
  )
  expect(el, `an error toast saying "${text}"`).toBeTruthy()
}

function noSuccessToast() {
  expect(document.body.querySelector('.n-message--success-type'), 'no success toast').toBeNull()
}

function bodyText(): string {
  return document.body.textContent ?? ''
}

// The reverse three layers: the plaintext substring must appear in neither
// the page's own HTML (wrapper) nor the teleported DOM (toasts/modals land
// in body) nor ANY store's $state — stores are enumerated off the ACTIVE
// pinia so a store registered by a future child component is covered without
// this test knowing its name.
function assertZeroPlaintextEverywhere(plaintext: string) {
  expect(wrapper!.html(), 'layer 1: the page subtree HTML').not.toContain(plaintext)
  expect(document.body.innerHTML, 'layer 2: the full DOM incl. teleports').not.toContain(plaintext)
  const pinia = getActivePinia()
  expect(pinia, 'an active pinia exists').toBeTruthy()
  for (const [id, store] of pinia!._s) {
    expect(JSON.stringify(store.$state), `layer 3: store "${id}" $state`).not.toContain(plaintext)
  }
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

describe('ProviderDetailPage key reveal copy (TC-03/TC-08/TC-09)', () => {
  it('TC-03 forward: the row-menu copy reveals and hands the FULL plaintext to the clipboard util, toasting 已复制', async () => {
    await mountPage()

    // The row menu carries the copy entry between test and delete.
    expect(await openKeyRowMenu('primary')).toEqual([
      zhCN.providers.editKey,
      zhCN.providers.testConnection,
      zhCN.common.copy,
      zhCN.providers.deleteKey,
    ])
    await clickMenuItem(zhCN.common.copy)

    // The reveal fired for THIS key, and the clipboard util received the
    // complete plaintext — exactly once, exactly the string.
    await vi.waitFor(() => expect(copyToClipboard).toHaveBeenCalledTimes(1))
    expect(revealMock).toHaveBeenCalledWith(PROVIDER_ID, 1)
    expect(copyToClipboard.mock.calls[0][0]).toBe(PLAINTEXT)
    // ② the toast (tier + copy) rendered…
    successToastSaying(zhCN.common.copied)
    // …and nothing about the plaintext leaked into the DOM on the way.
    expect(bodyText()).not.toContain(PLAINTEXT)

    // ③ copy settled → open the edit modal (the one reactive surface that
    // could have cached something) → close it → the reverse three layers.
    await openKeyRowMenu('primary')
    await clickMenuItem(zhCN.providers.editKey)
    expect((openModal().textContent ?? '')).toContain(zhCN.providers.editKey)
    buttonByLabel(openModal(), zhCN.providers.cancel).click()
    await nextTick()
    await nextTick()
    assertZeroPlaintextEverywhere(PLAINTEXT)
  })

  it('TC-03 reverse stands for the modal surface too: copying the stored key from the edit modal leaves nothing behind', async () => {
    await mountPage()

    await openKeyRowMenu('primary')
    await clickMenuItem(zhCN.providers.editKey)
    const modal = openModal()
    // The stored-key copy row exists only when editing an existing key.
    expect(modal.textContent ?? '', 'the stored-key copy row rendered').toContain(zhCN.providers.copyCurrentKey)
    buttonByLabel(modal, zhCN.common.copy).click()

    await vi.waitFor(() => expect(copyToClipboard).toHaveBeenCalledWith(PLAINTEXT))
    expect(revealMock).toHaveBeenCalledWith(PROVIDER_ID, 1)
    successToastSaying(zhCN.common.copied)

    buttonByLabel(modal, zhCN.providers.cancel).click()
    await nextTick()
    await nextTick()
    assertZeroPlaintextEverywhere(PLAINTEXT)
  })

  it('TC-08: after a modal copy, closing and reopening the modal shows no residue and the list still renders', async () => {
    await mountPage()

    // Copy inside the edit modal…
    await openKeyRowMenu('primary')
    await clickMenuItem(zhCN.providers.editKey)
    buttonByLabel(openModal(), zhCN.common.copy).click()
    await vi.waitFor(() => expect(copyToClipboard).toHaveBeenCalledTimes(1))

    // …then close it and open it again for a different look at the same key.
    buttonByLabel(openModal(), zhCN.providers.cancel).click()
    await nextTick()
    await nextTick()
    await openKeyRowMenu('primary')
    await clickMenuItem(zhCN.providers.editKey)

    const reopened = openModal()
    // The plaintext field restarts EMPTY (blank = keep the current key) — a
    // leftover value would also be silently submitted on the next save.
    const passwordInput = [...reopened.querySelectorAll('input')].find((i) => i.type === 'password')
    expect(passwordInput, 'the modal rendered its plaintext field').toBeTruthy()
    expect(passwordInput!.value).toBe('')
    // No trace anywhere: DOM layers + every store $state.
    assertZeroPlaintextEverywhere(PLAINTEXT)
    // The reopen was a fresh view: one reveal for one copy, nothing cached
    // that refetches or replays on open.
    expect(revealMock).toHaveBeenCalledTimes(1)
    // The list stays healthy: the key list behind the modal still renders
    // its row, display pair and all.
    const listText = keyRowTr('primary').textContent ?? ''
    expect(listText).toContain('sk-abcd***wxyz')
    expect(listText).toContain(SUFFIX)
  })

  it('TC-09: the copy copies exist in both locales (zh-CN and en)', async () => {
    // Both locales carry the words the two surfaces render, with the zh
    // copies being the exact strings asserted above. Tree-level parity
    // (every key present in BOTH directions, placeholders matched) is the
    // parity suite, executed by the same vitest run as this file.
    expect(zhCN.common.copy).toBe('复制')
    expect(zhCN.common.copied).toBe('已复制')
    expect(zhCN.common.copyFailed).toBeTruthy()
    expect(zhCN.providers.copyCurrentKey).toBeTruthy()
    expect(zhCN.providers.copyCurrentKey_tip).toBeTruthy()
    expect(en.common.copy).toBeTruthy()
    expect(en.common.copied).toBeTruthy()
    expect(en.common.copyFailed).toBeTruthy()
    expect(en.providers.copyCurrentKey).toBeTruthy()
    expect(en.providers.copyCurrentKey_tip).toBeTruthy()
  })
})

// Every branch the copy handlers grew on their error side, one test each:
// deleting the branch (or inverting its condition) makes the matching test
// below go red. The APIError codes are the reveal endpoint's real refusal
// shapes: 429 spends the aggregated budget → errcode.ServiceUnavailable
// (50005), a vanished key → 12009, a lapsed session → ACCOUNT_SESSION_INVALID.
describe('ProviderDetailPage key reveal copy failure branches', () => {
  it('row-menu copy: a refused clipboard write toasts 复制失败 and never a success toast', async () => {
    await mountPage()
    // The util still received the FULL plaintext — the write itself failed
    // (non-secure context / permission denial is exactly what the boolean
    // return exists for), so the reveal-to-clipboard plumbing is intact.
    copyToClipboard.mockResolvedValueOnce(false)

    await openKeyRowMenu('primary')
    await clickMenuItem(zhCN.common.copy)

    await vi.waitFor(() => errorToastSaying(zhCN.common.copyFailed))
    noSuccessToast()
    expect(revealMock).toHaveBeenCalledWith(PROVIDER_ID, 1)
    expect(copyToClipboard.mock.calls[0]?.[0]).toBe(PLAINTEXT)
  })

  it('modal stored-key copy: a refused clipboard write toasts 复制失败 and never a success toast', async () => {
    await mountPage()
    await openKeyRowMenu('primary')
    await clickMenuItem(zhCN.providers.editKey)
    const modal = openModal()
    copyToClipboard.mockResolvedValueOnce(false)

    buttonByLabel(modal, zhCN.common.copy).click()

    await vi.waitFor(() => errorToastSaying(zhCN.common.copyFailed))
    noSuccessToast()
    expect(revealMock).toHaveBeenCalledWith(PROVIDER_ID, 1)
    expect(copyToClipboard.mock.calls[0]?.[0]).toBe(PLAINTEXT)
  })

  it('row-menu copy: a refused reveal surfaces the API error toast without leaving the page', async () => {
    const router = await mountPage()
    const refusal = new APIError(50005)
    revealMock.mockRejectedValueOnce(refusal)

    await openKeyRowMenu('primary')
    await clickMenuItem(zhCN.common.copy)

    // The refusal reached the admin through displayMessage — the error's OWN
    // already-localized message, tier pinned — and the chain stopped before
    // any clipboard write, with the page exactly where it was.
    await vi.waitFor(() => errorToastSaying(refusal.message))
    noSuccessToast()
    expect(copyToClipboard).not.toHaveBeenCalled()
    expect(router.currentRoute.value.path).toBe(`/providers/${PROVIDER_ID}`)
  })

  it('row-menu copy: a session-expired reveal redirects to /login instead of toasting', async () => {
    const router = await mountPage()
    revealMock.mockRejectedValueOnce(new APIError(ACCOUNT_SESSION_INVALID))

    await openKeyRowMenu('primary')
    await clickMenuItem(zhCN.common.copy)

    // The expiry routed the admin to reauth: navigation fired, the auth
    // store noticed (the notice LoginPage consumes was set)…
    await vi.waitFor(() => expect(router.currentRoute.value.path).toBe('/login'))
    expect(useAuthStore().sessionExpiredNotice).toBe(true)
    // …and the early return means NO error toast dumped the expiry message
    // on top of the redirect, and no clipboard write happened.
    expect(document.body.textContent ?? '').not.toContain(new APIError(ACCOUNT_SESSION_INVALID).message)
    expect(copyToClipboard).not.toHaveBeenCalled()
  })

  it('modal stored-key copy: a refused reveal surfaces the API error toast and keeps the modal open', async () => {
    await mountPage()
    await openKeyRowMenu('primary')
    await clickMenuItem(zhCN.providers.editKey)
    const modal = openModal()
    const refusal = new APIError(12009)
    revealMock.mockRejectedValueOnce(refusal)

    buttonByLabel(modal, zhCN.common.copy).click()

    await vi.waitFor(() => errorToastSaying(refusal.message))
    noSuccessToast()
    expect(copyToClipboard).not.toHaveBeenCalled()
    // The modal survives the failure — the admin can retry or cancel — and
    // the stored-key copy row is still there to retry with.
    expect((openModal().textContent ?? '')).toContain(zhCN.providers.copyCurrentKey)
  })
})
