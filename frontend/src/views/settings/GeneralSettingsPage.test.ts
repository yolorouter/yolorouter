// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the General settings page (/settings/general) in
// BOTH locales (zh-CN and en ship different copy, so each is asserted on the
// copy itself):
//
//   - render: all five persistent-form items appear with the committed
//     values (retention days input, language picker, key-recovery switch +
//     interval input, and the capability card's six switches + margin
//     input at their seeded off/5 state)
//   - save: changing retention and saving PUTs the retention endpoint with
//     the CAS version; toggling the recovery switch and saving PUTs the
//     key-auto-recovery endpoint — TC-06 (both arms), TC-10
//   - the retention illegal-value matrix (-1 / 1.5 / 3651 / cleared) each
//     blocks the save with its own localized message and never PUTs —
//     TC-02's frontend half (matrix shape aligned with the key-recovery
//     modal's, whose pure logic lives in utils/keyAutoRecovery.test.ts)
//   - one representative illegal interval blocks the recovery save the same
//     way — TC-10's "modal behavior preserved in form shape" arm
//   - the capability card saves as one whole snapshot (two flipped switches
//     + the margin PUT all six switches together with the CAS version),
//     re-projects the committed GET after a save, blocks the illegal
//     margin matrix without a PUT, and surfaces the 409 conflict by
//     reloading the committed row — pure logic in
//     utils/capabilityEnforcement.test.ts
//   - the language item is dual-entry same-source with the sidebar Language
//     entry: a switch driven through the nav entry's store action shows up
//     in the form, and a pick in the form writes the same store (and only
//     the store — no settings API) while the interface re-renders in the
//     new locale — TC-09
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { createPinia, setActivePinia } from 'pinia'
import naive from 'naive-ui'
import { NSelect } from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import {
  getAdminAuditRetention,
  getCapabilityEnforcement,
  getKeyAutoRecovery,
  getOutboundProxy,
  getRequestLogRetention,
  updateAdminAuditRetention,
  updateCapabilityEnforcement,
  updateKeyAutoRecovery,
  updateOutboundProxy,
  updateRequestLogRetention,
} from '../../api/systemSettings'

vi.mock('../../api/systemSettings', () => ({
  getKeyAutoRecovery: vi.fn(),
  getRequestLogRetention: vi.fn(),
  updateKeyAutoRecovery: vi.fn(),
  updateRequestLogRetention: vi.fn(),
  getAdminAuditRetention: vi.fn(),
  updateAdminAuditRetention: vi.fn(),
  getOutboundProxy: vi.fn(),
  updateOutboundProxy: vi.fn(),
  getCapabilityEnforcement: vi.fn(),
  updateCapabilityEnforcement: vi.fn(),
}))

const getRetentionMock = vi.mocked(getRequestLogRetention)
const putRetentionMock = vi.mocked(updateRequestLogRetention)
const getAuditRetentionMock = vi.mocked(getAdminAuditRetention)
const putAuditRetentionMock = vi.mocked(updateAdminAuditRetention)
const getRecoveryMock = vi.mocked(getKeyAutoRecovery)
const putRecoveryMock = vi.mocked(updateKeyAutoRecovery)
const getProxyMock = vi.mocked(getOutboundProxy)
const putProxyMock = vi.mocked(updateOutboundProxy)
const getCapabilityMock = vi.mocked(getCapabilityEnforcement)
const putCapabilityMock = vi.mocked(updateCapabilityEnforcement)

import { APIError } from '../../api/client'
import { CAPABILITY_ENFORCEMENT_CONFLICT, OUTBOUND_PROXY_CONFLICT } from '../../api/errcodes'

// Must be imported after the vi.mock factory is registered.
import GeneralSettingsPage from './GeneralSettingsPage.vue'
import { i18n as appI18n, setLocale as setAppLocale, LOCALES } from '../../i18n'
import { useLocaleStore } from '../../store/locale'
import en from '../../locales/en'
import zhCN from '../../locales/zh-CN'

// LOCALES labels are the languages' own endonyms (the same literals the
// shipped picker shows), so the tests assert on them without duplicating
// the strings.
const labelOf = (value: 'zh-CN' | 'en') => LOCALES.find((l) => l.value === value)!.label

// App.vue nests pages under n-config-provider > n-message-provider; the
// page's useMessage() throws without that ancestry.
const Host = defineComponent({
  components: { GeneralSettingsPage },
  template: '<NConfigProvider><NMessageProvider><GeneralSettingsPage /></NMessageProvider></NConfigProvider>',
})

type Locale = 'en' | 'zh-CN'
const navCopy = { en: en.nav, 'zh-CN': zhCN.nav } as const
const gsCopy = { en: en.generalSettings, 'zh-CN': zhCN.generalSettings } as const
const karCopy = { en: en.keyAutoRecovery, 'zh-CN': zhCN.keyAutoRecovery } as const
const capCopy = { en: en.generalSettings.capability, 'zh-CN': zhCN.generalSettings.capability } as const

let wrapper: VueWrapper | null = null

// The committed rows the GETs resolve with. Retention starts at the
// fail-open default 0 (keep forever); recovery starts disabled.
function primeSettings(retentionDays = 0, retentionVersion = 5) {
  getRetentionMock.mockResolvedValue({ retention_days: retentionDays, version: retentionVersion })
  // Audit retention starts at the seeded default 180 — the seeded row the
  // migration writes on a fresh install (unlike the request-log default 0).
  getAuditRetentionMock.mockResolvedValue({ retention_days: 180, version: 3 })
  getRecoveryMock.mockResolvedValue({ enabled: false, interval_minutes: 15, version: 9 })
  // Outbound proxy starts at the seeded default: mode off, empty URL.
  getProxyMock.mockResolvedValue({ mode: 'off', url: '', version: 7 })
  // Capability enforcement starts at the seeded default: every switch off,
  // margin 5 (the seven rows the seeding migration plants on a fresh
  // install).
  getCapabilityMock.mockResolvedValue({
    context_limit_block: false,
    temperature_default: false,
    temperature_force: false,
    tools_strict: false,
    reasoning_strict: false,
    reasoning_egress_clear: false,
    context_margin_percent: 5,
    version: 11,
  })
}

async function mountPage(locale: Locale) {
  const i18n = createI18n({ legacy: false, locale, fallbackLocale: 'zh-CN', messages: { en, 'zh-CN': zhCN } })
  // The page reads the locale store for the language item, so every mount
  // needs an active pinia.
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, i18n, naive] } })
  // Both mount-time GETs are mocked to resolve instantly; settle them.
  await vi.waitFor(() => expect(getRetentionMock).toHaveBeenCalled())
  await nextTick()
  await nextTick()
}

function retentionInput(): HTMLInputElement {
  const input = document.body.querySelector<HTMLInputElement>('.gs-retention__days input')
  expect(input, 'retention days input rendered').toBeTruthy()
  return input!
}

function intervalInput(): HTMLInputElement {
  const input = document.body.querySelector<HTMLInputElement>('.gs-recovery__interval input')
  expect(input, 'recovery interval input rendered').toBeTruthy()
  return input!
}

function saveButton(
  section: 'retention' | 'audit-retention' | 'proxy' | 'capability' | 'recovery',
): HTMLButtonElement {
  const btn = document.body.querySelector<HTMLButtonElement>(`.gs-${section}__save`)
  expect(btn, `${section} save button rendered`).toBeTruthy()
  return btn!
}

function auditRetentionInput(): HTMLInputElement {
  const input = document.body.querySelector<HTMLInputElement>('.gs-audit-retention__days input')
  expect(input, 'audit retention days input rendered').toBeTruthy()
  return input!
}

// The proxy card's URL input (a plain NInput, unlike the NInputNumber ones).
function proxyUrlInput(): HTMLInputElement {
  const input = document.body.querySelector<HTMLInputElement>('.gs-proxy__url input')
  expect(input, 'proxy URL input rendered').toBeTruthy()
  return input!
}

// NInput keeps its v-model in sync per keystroke, same as NInputNumber.
function setPlainInput(input: HTMLInputElement, value: string) {
  input.value = value
  input.dispatchEvent(new Event('input'))
}

// Drives the proxy mode select exactly like a user pick: the emitted
// update:value is what the v-model / @update:value wiring receives in the
// browser. The select is identified by its passthrough class, not by
// mounting order.
function pickProxyMode(value: 'off' | 'system' | 'custom') {
  const page = wrapper!.findComponent(GeneralSettingsPage)
  const select = page
    .findAllComponents(NSelect)
    .find((c) => (c.element as HTMLElement | undefined)?.classList?.contains('gs-proxy__mode'))
  expect(select, 'proxy mode select rendered').toBeTruthy()
  select!.vm.$emit('update:value', value)
}

function recoverySwitch(): HTMLElement {
  const el = document.body.querySelector<HTMLElement>('.gs-recovery__switch')
  expect(el, 'recovery switch rendered').toBeTruthy()
  return el!
}

// One of the capability card's six switches, addressed by its per-switch
// class so the six stay individually reachable (and individually
// assertable) regardless of card order.
function capabilitySwitch(which: 'context-limit' | 'temperature-default' | 'temperature-force' | 'tools-strict' | 'reasoning-strict' | 'reasoning-clear'): HTMLElement {
  const el = document.body.querySelector<HTMLElement>(`.gs-capability__switch-${which}`)
  expect(el, `capability switch ${which} rendered`).toBeTruthy()
  return el!
}

// The capability card's margin input.
function capabilityMarginInput(): HTMLInputElement {
  const input = document.body.querySelector<HTMLInputElement>('.gs-capability__margin input')
  expect(input, 'capability margin input rendered').toBeTruthy()
  return input!
}

// NInputNumber keeps its v-model in sync per keystroke (updateValueOnInput,
// no min/max/precision props on these fields), so a native input event with
// the new string drives the form state exactly like typing does.
function setInputValue(input: HTMLInputElement, value: string) {
  input.value = value
  input.dispatchEvent(new Event('input'))
}

beforeEach(() => {
  primeSettings()
  putRetentionMock.mockReset()
  putAuditRetentionMock.mockReset()
  putProxyMock.mockReset()
  putCapabilityMock.mockReset()
  putRecoveryMock.mockReset()
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe.each<Locale>(['zh-CN', 'en'])('GeneralSettingsPage render (%s)', (locale) => {
  it('renders all five persistent-form items with the committed values', async () => {
    await mountPage(locale)
    const text = document.body.textContent ?? ''
    // The page header and all section cards.
    expect(text).toContain(navCopy[locale].generalSettings)
    expect(text).toContain(gsCopy[locale].retention.title)
    expect(text).toContain(gsCopy[locale].auditRetention.title)
    expect(text).toContain(gsCopy[locale].language.title)
    expect(text).toContain(karCopy[locale].title)
    expect(text).toContain(gsCopy[locale].outboundProxy.title)
    expect(text).toContain(gsCopy[locale].outboundProxy.mode)
    // The system-mode notice exists in the copy but stays hidden while the
    // committed mode is off — it renders only once system is selected.
    expect(
      document.body.querySelector('[data-testid="proxy-system-notice"]'),
      'system-mode notice hidden while mode is off',
    ).toBeNull()
    expect(text).toContain(gsCopy[locale].retention.days)
    expect(text).toContain(gsCopy[locale].auditRetention.days)
    expect(text).toContain(gsCopy[locale].language.label)
    expect(text).toContain(karCopy[locale].enabled)
    expect(text).toContain(karCopy[locale].interval)
    // The capability card: its title, all six switch labels and the margin
    // label come from the locale's own subtree, so each is asserted on the
    // copy itself (zh-CN and en ship different text).
    expect(text).toContain(capCopy[locale].title)
    expect(text).toContain(capCopy[locale].contextLimitBlock)
    expect(text).toContain(capCopy[locale].temperatureDefault)
    expect(text).toContain(capCopy[locale].temperatureForce)
    expect(text).toContain(capCopy[locale].toolsStrict)
    expect(text).toContain(capCopy[locale].reasoningStrict)
    expect(text).toContain(capCopy[locale].reasoningEgressClear)
    expect(text).toContain(capCopy[locale].margin)
    // Committed values: retention shows the seeded 0, audit retention the
    // seeded 180, interval 15, capability margin the seeded 5.
    expect(retentionInput().value).toBe('0')
    expect(auditRetentionInput().value).toBe('180')
    expect(intervalInput().value).toBe('15')
    expect(capabilityMarginInput().value).toBe('5')
    expect(recoverySwitch().getAttribute('aria-checked')).toBe('false')
    // Every capability switch renders in its committed off state.
    for (const which of [
      'context-limit',
      'temperature-default',
      'temperature-force',
      'tools-strict',
      'reasoning-strict',
      'reasoning-clear',
    ] as const) {
      expect(capabilitySwitch(which).getAttribute('aria-checked')).toBe('false')
    }
    // All the save buttons are present and enabled (all GETs resolved).
    expect(saveButton('retention').disabled).toBe(false)
    expect(saveButton('audit-retention').disabled).toBe(false)
    expect(saveButton('proxy').disabled).toBe(false)
    expect(saveButton('capability').disabled).toBe(false)
    expect(saveButton('recovery').disabled).toBe(false)
  })
})

describe('GeneralSettingsPage save interactions (en)', () => {
  it('PUTs the retention endpoint with the CAS version when the days change (TC-06 retention arm)', async () => {
    await mountPage('en')
    setInputValue(retentionInput(), '30')
    await nextTick()
    saveButton('retention').dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(putRetentionMock).toHaveBeenCalledTimes(1))
    expect(putRetentionMock).toHaveBeenCalledWith({ retention_days: 30, version: 5 })
    // Save succeeds → the committed row is re-read (the GET fires again).
    await vi.waitFor(() => expect(getRetentionMock).toHaveBeenCalledTimes(2))
    expect(document.body.textContent ?? '').toContain(gsCopy.en.retention.saved)
  })

  it('PUTs the admin-audit-retention endpoint with its own CAS version when the days change', async () => {
    await mountPage('en')
    setInputValue(auditRetentionInput(), '90')
    await nextTick()
    saveButton('audit-retention').dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(putAuditRetentionMock).toHaveBeenCalledTimes(1))
    // Its own endpoint, its own version — independent of the request-log
    // retention row above it.
    expect(putAuditRetentionMock).toHaveBeenCalledWith({ retention_days: 90, version: 3 })
    expect(putRetentionMock, 'the sibling retention card stays untouched').not.toHaveBeenCalled()
    // Save succeeds → the committed row is re-read.
    await vi.waitFor(() => expect(getAuditRetentionMock).toHaveBeenCalledTimes(2))
    expect(document.body.textContent ?? '').toContain(gsCopy.en.auditRetention.saved)
  })

  it('PUTs the existing key-auto-recovery endpoint when the switch is toggled (TC-06 / TC-10)', async () => {
    await mountPage('en')
    recoverySwitch().dispatchEvent(new Event('click'))
    await nextTick()
    expect(recoverySwitch().getAttribute('aria-checked')).toBe('true')
    saveButton('recovery').dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(putRecoveryMock).toHaveBeenCalledTimes(1))
    // Existing API, existing payload shape: only the switch changed, the
    // loaded interval and version ride along.
    expect(putRecoveryMock).toHaveBeenCalledWith({ enabled: true, interval_minutes: 15, version: 9 })
    expect(document.body.textContent ?? '').toContain(karCopy.en.saved)
  })
})

describe('GeneralSettingsPage retention illegal-value matrix (en) — TC-02 frontend half', () => {
  // The same drafts the pure validator rejects (utils/requestLogRetention),
  // asserted here at the form level: each one blocks the save with its own
  // message and the PUT never fires.
  const drafts: { input: string; message: string }[] = [
    { input: '-1', message: gsCopy.en.retention.daysMin },
    { input: '1.5', message: gsCopy.en.retention.daysWholeNumber },
    { input: '3651', message: gsCopy.en.retention.daysMax },
    // Clearing the field is "empty", not "zero": NInputNumber reports null.
    { input: '', message: gsCopy.en.retention.daysRequired },
  ]

  it.each(drafts)('input "$input" is rejected with its own message and no PUT', async (draft) => {
    await mountPage('en')
    setInputValue(retentionInput(), draft.input)
    await nextTick()
    saveButton('retention').dispatchEvent(new Event('click'))
    await nextTick()
    await nextTick()
    expect(document.body.textContent ?? '', 'the form shows the issue message').toContain(draft.message)
    expect(putRetentionMock, 'an illegal retention never reaches the wire').not.toHaveBeenCalled()
  })
})

describe('GeneralSettingsPage audit retention stays validated in form shape (en)', () => {
  // Same drafts the pure validator rejects (utils/auditRetention) — one per
  // issue, asserted at the form level with the audit card's own copy.
  const drafts: { input: string; message: string }[] = [
    { input: '-1', message: gsCopy.en.auditRetention.daysMin },
    { input: '1.5', message: gsCopy.en.auditRetention.daysWholeNumber },
    { input: '3651', message: gsCopy.en.auditRetention.daysMax },
    { input: '', message: gsCopy.en.auditRetention.daysRequired },
  ]

  it.each(drafts)('input "$input" is rejected with its own message and no PUT', async (draft) => {
    await mountPage('en')
    setInputValue(auditRetentionInput(), draft.input)
    await nextTick()
    saveButton('audit-retention').dispatchEvent(new Event('click'))
    await nextTick()
    await nextTick()
    expect(document.body.textContent ?? '', 'the form shows the issue message').toContain(draft.message)
    expect(putAuditRetentionMock, 'an illegal retention never reaches the wire').not.toHaveBeenCalled()
  })
})

describe('GeneralSettingsPage recovery interval stays validated in form shape (en) — TC-10', () => {
  it('blocks the save when the interval is cleared, with the modal-era message', async () => {
    await mountPage('en')
    setInputValue(intervalInput(), '')
    await nextTick()
    saveButton('recovery').dispatchEvent(new Event('click'))
    await nextTick()
    await nextTick()
    expect(document.body.textContent ?? '').toContain(karCopy.en.intervalRequired)
    expect(putRecoveryMock).not.toHaveBeenCalled()
  })
})

// TC-09 — the language item and the sidebar Language entry are two views of
// one preference. In production they share two singletons: the locale store
// (pinia) and the i18n instance the app installs (main.ts does `.use(i18n)`
// with the module singleton, and the store reads/writes that same object).
// These tests reproduce exactly that wiring — the module singleton IS the
// plugin instance here — so agreement between the form and the interface is
// the real dual-entry semantics, not a coincidence of two test doubles.
describe('GeneralSettingsPage language dual-entry same source — TC-09', () => {
  function languageSelect(): HTMLElement {
    const el = document.body.querySelector<HTMLElement>('.gs-language__select')
    expect(el, 'language select rendered').toBeTruthy()
    return el!
  }

  // Drives the select exactly like a user pick: the emitted update:value is
  // what the @update:value handler receives in the browser.
  function pickLanguage(value: 'zh-CN' | 'en') {
    // The proxy card added a second NSelect to the page, so the language
    // one is identified by its passthrough class, not by being first.
    const select = wrapper!
      .findComponent(GeneralSettingsPage)
      .findAllComponents(NSelect)
      .find((c) => (c.element as HTMLElement | undefined)?.classList?.contains('gs-language__select'))
    expect(select, 'language select component found').toBeTruthy()
    select!.vm.$emit('update:value', value)
  }

  async function mountPageOnAppI18n() {
    const pinia = createPinia()
    setActivePinia(pinia)
    wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, appI18n, naive] } })
    await vi.waitFor(() => expect(getRetentionMock).toHaveBeenCalled())
    await nextTick()
    await nextTick()
  }

  beforeEach(() => {
    // Deterministic start for every test in this describe: zh-CN, nothing
    // persisted (the module singleton initializes from localStorage at
    // import time, so reset both explicitly).
    localStorage.removeItem('yolorouter-locale')
    setAppLocale('zh-CN')
  })

  afterEach(() => {
    localStorage.removeItem('yolorouter-locale')
    setAppLocale('zh-CN')
  })

  it('a switch made through the nav entry\'s store action shows up in the form (zh → en)', async () => {
    await mountPageOnAppI18n()
    expect(languageSelect().textContent).toContain(labelOf('zh-CN'))

    // The sidebar Language entry's handler is exactly this store call
    // (DefaultLayout's onSelectLanguage / LocaleSwitcher's onLocaleChange),
    // so driving the action is the nav entry's write path without mounting
    // the whole shell.
    useLocaleStore().setLocale('en')
    await nextTick()

    expect(languageSelect().textContent, 'the form reflects the nav-made switch').toContain(labelOf('en'))
  })

  it('picking a language in the form writes the same store and flips the interface back (en → zh)', async () => {
    // Start from en — e.g. the nav arm above just switched there.
    setAppLocale('en')
    await mountPageOnAppI18n()
    expect(languageSelect().textContent).toContain(labelOf('en'))

    pickLanguage('zh-CN')
    await nextTick()
    await nextTick()

    // One source of truth: the store, i18n's global locale, the persisted
    // browser-local value, and <html lang> all moved together…
    expect(useLocaleStore().locale).toBe('zh-CN')
    expect(appI18n.global.locale.value).toBe('zh-CN')
    expect(localStorage.getItem('yolorouter-locale')).toBe('zh-CN')
    expect(document.documentElement.lang).toBe('zh-CN')
    expect(languageSelect().textContent).toContain(labelOf('zh-CN'))
    // …and the mounted interface itself re-rendered in Chinese — the page
    // header copy flipped with the pick, which is "the form switch took
    // effect" end to end.
    expect(document.body.textContent ?? '').toContain(zhCN.nav.generalSettings)
    // Personal-level preference: a language pick never writes to the
    // settings API (the two server-backed PUTs stay untouched).
    expect(putRetentionMock).not.toHaveBeenCalled()
    expect(putRecoveryMock).not.toHaveBeenCalled()
  })
})

// --- Outbound proxy card -----------------------------------------------------

// The system-mode hint. Mounting the settings area and switching
// the mode to system must surface the copy that says what that mode
// actually reads (the process environment variables) and the operational
// caveat that env changes in a container need a container restart. Both
// locales ship their own copy, so both are asserted on the copy itself.
describe.each<Locale>(['zh-CN', 'en'])('GeneralSettingsPage system-mode notice (%s)', (locale) => {
  it('shows the process-environment notice when system mode is selected, and hides it otherwise', async () => {
    await mountPage(locale)
    // Off is the committed default: no notice rendered.
    expect(document.body.querySelector('[data-testid="proxy-system-notice"]')).toBeNull()

    pickProxyMode('system')
    await nextTick()
    await nextTick()

    const notice = document.body.querySelector<HTMLElement>('[data-testid="proxy-system-notice"]')
    expect(notice, 'system-mode notice rendered').toBeTruthy()
    const copy = notice!.textContent ?? ''
    expect(copy).toContain(gsCopy[locale].outboundProxy.systemNotice)
    // The two non-negotiable facts, asserted on the rendered text itself:
    // the mode reads the process environment variables, and container-side
    // changes need a container restart.
    expect(copy).toContain(
      locale === 'zh-CN' ? '读取进程环境变量' : 'reads the process environment variables',
    )
    expect(copy).toContain(locale === 'zh-CN' ? '重启容器' : 'container restart')

    // Switching back to off removes the notice again.
    pickProxyMode('off')
    await nextTick()
    await nextTick()
    expect(document.body.querySelector('[data-testid="proxy-system-notice"]')).toBeNull()
  })
})

describe('GeneralSettingsPage outbound proxy interactions (en)', () => {
  it('PUTs the outbound-proxy endpoint with the CAS version when mode and URL are set', async () => {
    await mountPage('en')
    pickProxyMode('custom')
    await nextTick()
    setPlainInput(proxyUrlInput(), 'socks5h://192.168.1.5:1080')
    await nextTick()
    saveButton('proxy').dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(putProxyMock).toHaveBeenCalledTimes(1))
    expect(putProxyMock).toHaveBeenCalledWith({
      mode: 'custom',
      url: 'socks5h://192.168.1.5:1080',
      version: 7,
    })
    // Save succeeds → the committed row is re-read.
    await vi.waitFor(() => expect(getProxyMock).toHaveBeenCalledTimes(2))
    expect(document.body.textContent ?? '').toContain(gsCopy.en.outboundProxy.saved)
  })

  it('saves mode-only changes (off) with an empty URL', async () => {
    getProxyMock.mockResolvedValue({ mode: 'custom', url: 'http://127.0.0.1:7890', version: 7 })
    await mountPage('en')
    pickProxyMode('off')
    await nextTick()
    // The parked URL rides along unless cleared; clearing it under off is
    // legal (and is what the "turn it off" intent means).
    setPlainInput(proxyUrlInput(), '')
    await nextTick()
    saveButton('proxy').dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(putProxyMock).toHaveBeenCalledTimes(1))
    expect(putProxyMock).toHaveBeenCalledWith({ mode: 'off', url: '', version: 7 })
  })

  it.each([
    { input: 'ftp://proxy.example.com:21', message: gsCopy.en.outboundProxy.proxyUrlScheme },
    { input: 'proxy.example.com:7890', message: gsCopy.en.outboundProxy.proxyUrlScheme },
    { input: '', message: gsCopy.en.outboundProxy.proxyUrlRequired },
  ])('input "$input" under custom mode blocks the save with its own message and no PUT', async (draft) => {
    await mountPage('en')
    pickProxyMode('custom')
    await nextTick()
    setPlainInput(proxyUrlInput(), draft.input)
    await nextTick()
    saveButton('proxy').dispatchEvent(new Event('click'))
    await nextTick()
    await nextTick()
    expect(document.body.textContent ?? '', 'the form shows the issue message').toContain(draft.message)
    expect(putProxyMock, 'an illegal proxy URL never reaches the wire').not.toHaveBeenCalled()
  })

  it('mode flips do not validate eagerly — the message waits for the save click, and a flip clears a shown one', async () => {
    await mountPage('en')
    // Flip to custom while the URL is still empty: no message appears —
    // picking a mode only resets the previous mode's feedback.
    pickProxyMode('custom')
    await nextTick()
    await nextTick()
    expect(document.body.textContent ?? '', 'no eager validation on mode select').not.toContain(
      gsCopy.en.outboundProxy.proxyUrlRequired,
    )
    // The save click is what runs the rule and surfaces the message.
    saveButton('proxy').dispatchEvent(new Event('click'))
    await nextTick()
    await nextTick()
    expect(document.body.textContent ?? '', 'the save surfaces the required message').toContain(
      gsCopy.en.outboundProxy.proxyUrlRequired,
    )
    expect(putProxyMock).not.toHaveBeenCalled()
    // Flipping the mode away clears the shown feedback (restoreValidation).
    pickProxyMode('off')
    await nextTick()
    await nextTick()
    expect(document.body.textContent ?? '', 'mode flip clears the stale message').not.toContain(
      gsCopy.en.outboundProxy.proxyUrlRequired,
    )
  })

  it('surfaces the 409 conflict and reloads the committed row', async () => {
    await mountPage('en')
    pickProxyMode('custom')
    await nextTick()
    setPlainInput(proxyUrlInput(), 'socks5://10.0.0.9:1080')
    await nextTick()
    putProxyMock.mockRejectedValueOnce(new APIError(OUTBOUND_PROXY_CONFLICT))
    saveButton('proxy').dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(putProxyMock).toHaveBeenCalledTimes(1))
    await vi.waitFor(() => expect(getProxyMock).toHaveBeenCalledTimes(2))
    expect(document.body.textContent ?? '').toContain(gsCopy.en.outboundProxy.conflict)
  })
})

// --- Capability enforcement card ---------------------------------------------

// The seven rows behind this card share one version, so a save PUTs the
// whole snapshot: all six switches plus the margin ride along even when
// only one control moved, and the untouched switches keep their committed
// values. The payload arms below pin that shape.
describe('GeneralSettingsPage capability enforcement interactions (en)', () => {
  it('PUTs the whole snapshot — flipped switches on, untouched ones riding along, margin and CAS version included', async () => {
    await mountPage('en')
    // Flip two of the six switches and widen the margin.
    capabilitySwitch('context-limit').dispatchEvent(new Event('click'))
    capabilitySwitch('tools-strict').dispatchEvent(new Event('click'))
    await nextTick()
    expect(capabilitySwitch('context-limit').getAttribute('aria-checked')).toBe('true')
    expect(capabilitySwitch('tools-strict').getAttribute('aria-checked')).toBe('true')
    // The four untouched switches stay off.
    expect(capabilitySwitch('temperature-default').getAttribute('aria-checked')).toBe('false')
    expect(capabilitySwitch('temperature-force').getAttribute('aria-checked')).toBe('false')
    expect(capabilitySwitch('reasoning-strict').getAttribute('aria-checked')).toBe('false')
    expect(capabilitySwitch('reasoning-clear').getAttribute('aria-checked')).toBe('false')
    setInputValue(capabilityMarginInput(), '10')
    await nextTick()
    saveButton('capability').dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(putCapabilityMock).toHaveBeenCalledTimes(1))
    // Whole-snapshot payload: the two flips, the four committed-offs, the
    // new margin, and the loaded version as the CAS token.
    expect(putCapabilityMock).toHaveBeenCalledWith({
      context_limit_block: true,
      temperature_default: false,
      temperature_force: false,
      tools_strict: true,
      reasoning_strict: false,
      reasoning_egress_clear: false,
      context_margin_percent: 10,
      version: 11,
    })
    // Save succeeds → the committed row is re-read.
    await vi.waitFor(() => expect(getCapabilityMock).toHaveBeenCalledTimes(2))
    expect(document.body.textContent ?? '').toContain(capCopy.en.saved)
  })

  it('re-projects the committed snapshot after a save (switches follow the GET, not the PUT echo)', async () => {
    await mountPage('en')
    capabilitySwitch('reasoning-clear').dispatchEvent(new Event('click'))
    await nextTick()
    // The re-read answers with the flip committed.
    getCapabilityMock.mockResolvedValue({
      context_limit_block: false,
      temperature_default: false,
      temperature_force: false,
      tools_strict: false,
      reasoning_strict: false,
      reasoning_egress_clear: true,
      context_margin_percent: 5,
      version: 12,
    })
    saveButton('capability').dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(putCapabilityMock).toHaveBeenCalledTimes(1))
    await vi.waitFor(() => expect(getCapabilityMock).toHaveBeenCalledTimes(2))
    expect(capabilitySwitch('reasoning-clear').getAttribute('aria-checked')).toBe('true')
    expect(capabilityMarginInput().value).toBe('5')
  })
})

describe('GeneralSettingsPage capability margin stays validated in form shape (en)', () => {
  // Same drafts the pure validator rejects (utils/capabilityEnforcement) —
  // one per issue, asserted at the form level with the capability card's
  // own copy.
  const drafts: { input: string; message: string }[] = [
    { input: '-1', message: capCopy.en.marginMin },
    { input: '2.5', message: capCopy.en.marginWholeNumber },
    { input: '1001', message: capCopy.en.marginMax },
    // Clearing the field is "empty", not "zero": NInputNumber reports null.
    { input: '', message: capCopy.en.marginRequired },
  ]

  it.each(drafts)('input "$input" is rejected with its own message and no PUT', async (draft) => {
    await mountPage('en')
    setInputValue(capabilityMarginInput(), draft.input)
    await nextTick()
    saveButton('capability').dispatchEvent(new Event('click'))
    await nextTick()
    await nextTick()
    expect(document.body.textContent ?? '', 'the form shows the issue message').toContain(draft.message)
    expect(putCapabilityMock, 'an illegal margin never reaches the wire').not.toHaveBeenCalled()
  })
})

describe('GeneralSettingsPage capability conflict recovery (en)', () => {
  it('surfaces the 409 conflict and reloads the committed snapshot', async () => {
    await mountPage('en')
    capabilitySwitch('context-limit').dispatchEvent(new Event('click'))
    await nextTick()
    // The concurrent writer committed first: the re-read answers with a
    // newer version and the switch already on.
    getCapabilityMock.mockResolvedValue({
      context_limit_block: true,
      temperature_default: false,
      temperature_force: false,
      tools_strict: false,
      reasoning_strict: false,
      reasoning_egress_clear: false,
      context_margin_percent: 5,
      version: 12,
    })
    putCapabilityMock.mockRejectedValueOnce(new APIError(CAPABILITY_ENFORCEMENT_CONFLICT))
    saveButton('capability').dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(putCapabilityMock).toHaveBeenCalledTimes(1))
    await vi.waitFor(() => expect(getCapabilityMock).toHaveBeenCalledTimes(2))
    expect(document.body.textContent ?? '').toContain(capCopy.en.conflict)
    // The form now shows the committed row: the switch the other admin
    // flipped is on, and the version moved for any subsequent save.
    expect(capabilitySwitch('context-limit').getAttribute('aria-checked')).toBe('true')
  })
})
