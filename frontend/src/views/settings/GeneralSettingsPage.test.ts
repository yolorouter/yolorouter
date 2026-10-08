// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the General settings page (/settings/general) in
// BOTH locales (zh-CN and en ship different copy, so each is asserted on the
// copy itself):
//
//   - render: all three persistent-form items appear with the committed
//     values (retention days input, language picker, key-recovery switch +
//     interval input) — TC-06
//   - save: changing retention and saving PUTs the retention endpoint with
//     the CAS version; toggling the recovery switch and saving PUTs the
//     key-auto-recovery endpoint — TC-06 (both arms), TC-10
//   - the retention illegal-value matrix (-1 / 1.5 / 3651 / cleared) each
//     blocks the save with its own localized message and never PUTs —
//     TC-02's frontend half (matrix shape aligned with the key-recovery
//     modal's, whose pure logic lives in utils/keyAutoRecovery.test.ts)
//   - one representative illegal interval blocks the recovery save the same
//     way — TC-10's "modal behavior preserved in form shape" arm
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
  getKeyAutoRecovery,
  getRequestLogRetention,
  updateAdminAuditRetention,
  updateKeyAutoRecovery,
  updateRequestLogRetention,
} from '../../api/systemSettings'

vi.mock('../../api/systemSettings', () => ({
  getKeyAutoRecovery: vi.fn(),
  getRequestLogRetention: vi.fn(),
  updateKeyAutoRecovery: vi.fn(),
  updateRequestLogRetention: vi.fn(),
  getAdminAuditRetention: vi.fn(),
  updateAdminAuditRetention: vi.fn(),
}))

const getRetentionMock = vi.mocked(getRequestLogRetention)
const putRetentionMock = vi.mocked(updateRequestLogRetention)
const getAuditRetentionMock = vi.mocked(getAdminAuditRetention)
const putAuditRetentionMock = vi.mocked(updateAdminAuditRetention)
const getRecoveryMock = vi.mocked(getKeyAutoRecovery)
const putRecoveryMock = vi.mocked(updateKeyAutoRecovery)

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

let wrapper: VueWrapper | null = null

// The committed rows the two GETs resolve with. Retention starts at the
// fail-open default 0 (keep forever); recovery starts disabled.
function primeSettings(retentionDays = 0, retentionVersion = 5) {
  getRetentionMock.mockResolvedValue({ retention_days: retentionDays, version: retentionVersion })
  // Audit retention starts at the seeded default 180 — the seeded row the
  // migration writes on a fresh install (unlike the request-log default 0).
  getAuditRetentionMock.mockResolvedValue({ retention_days: 180, version: 3 })
  getRecoveryMock.mockResolvedValue({ enabled: false, interval_minutes: 15, version: 9 })
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

function saveButton(section: 'retention' | 'audit-retention' | 'recovery'): HTMLButtonElement {
  const btn = document.body.querySelector<HTMLButtonElement>(`.gs-${section}__save`)
  expect(btn, `${section} save button rendered`).toBeTruthy()
  return btn!
}

function auditRetentionInput(): HTMLInputElement {
  const input = document.body.querySelector<HTMLInputElement>('.gs-audit-retention__days input')
  expect(input, 'audit retention days input rendered').toBeTruthy()
  return input!
}

function recoverySwitch(): HTMLElement {
  const el = document.body.querySelector<HTMLElement>('.gs-recovery__switch')
  expect(el, 'recovery switch rendered').toBeTruthy()
  return el!
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
  putRecoveryMock.mockReset()
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe.each<Locale>(['zh-CN', 'en'])('GeneralSettingsPage render (%s)', (locale) => {
  it('renders all four persistent-form items with the committed values', async () => {
    await mountPage(locale)
    const text = document.body.textContent ?? ''
    // The page header and all four section cards.
    expect(text).toContain(navCopy[locale].generalSettings)
    expect(text).toContain(gsCopy[locale].retention.title)
    expect(text).toContain(gsCopy[locale].auditRetention.title)
    expect(text).toContain(gsCopy[locale].language.title)
    expect(text).toContain(karCopy[locale].title)
    expect(text).toContain(gsCopy[locale].retention.days)
    expect(text).toContain(gsCopy[locale].auditRetention.days)
    expect(text).toContain(gsCopy[locale].language.label)
    expect(text).toContain(karCopy[locale].enabled)
    expect(text).toContain(karCopy[locale].interval)
    // Committed values: retention shows the seeded 0, audit retention the
    // seeded 180, interval 15.
    expect(retentionInput().value).toBe('0')
    expect(auditRetentionInput().value).toBe('180')
    expect(intervalInput().value).toBe('15')
    expect(recoverySwitch().getAttribute('aria-checked')).toBe('false')
    // All three save buttons are present and enabled (all GETs resolved).
    expect(saveButton('retention').disabled).toBe(false)
    expect(saveButton('audit-retention').disabled).toBe(false)
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
    wrapper!.findComponent(GeneralSettingsPage).findComponent(NSelect).vm.$emit('update:value', value)
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
