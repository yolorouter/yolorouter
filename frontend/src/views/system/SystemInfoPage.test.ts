// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the About page's update-wait experience, in BOTH
// locales (zh-CN and en ship different copy, so each is asserted on the copy
// itself): the ~60s mid-wait reassurance line, the 5-minute timeout's
// diagnostic guide (two common causes + the journalctl command), the
// unchanged success criterion (a different version answering), and the
// dismissed timeout guide staying out of a reopened confirm view. Time is
// driven with vi.useFakeTimers through the extracted restartWait machine —
// no real minute ever elapses.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import { getSystemVersion, postSystemUpdate, type SystemVersion } from '../../api/system'

vi.mock('../../api/system', () => ({
  getSystemVersion: vi.fn(),
  postSystemUpdate: vi.fn(),
}))

const versionMock = vi.mocked(getSystemVersion)
const updateMock = vi.mocked(postSystemUpdate)

// Must be imported after the vi.mock factory is registered.
import SystemInfoPage from './SystemInfoPage.vue'
import en from '../../locales/en'
import zhCN from '../../locales/zh-CN'

// The deployment shape that offers the one-click button: in-place updates,
// v0.2.5 running, v0.2.6 published. `answer` is what the version endpoint
// reports; tests flip it when the "restarted service" comes back.
let answer = 'v0.2.5'
function versionInfo(version: string): SystemVersion {
  return {
    version,
    commit: 'c0ffee',
    build_time: '2026-09-24T00:00:00Z',
    go_version: 'go1.24',
    goos: 'linux',
    goarch: 'amd64',
    db_driver: 'sqlite',
    update_mode: 'in_place',
    uptime_seconds: 42,
    latest: 'v0.2.6',
    has_update: true,
    release_url: '',
    check_failed: false,
  }
}

// App.vue nests pages under n-config-provider > n-message-provider; the
// page's useMessage() throws without that ancestry.
const Host = defineComponent({
  components: { SystemInfoPage },
  template: '<NConfigProvider><NMessageProvider><SystemInfoPage /></NMessageProvider></NConfigProvider>',
})

type Locale = 'en' | 'zh-CN'
const copy = { en: en.system, 'zh-CN': zhCN.system } as const
// The modal's Cancel button renders the shared common.copy, not system.copy.
const commonCopy = { en: en.common, 'zh-CN': zhCN.common } as const

let wrapper: VueWrapper | null = null

// Desktop viewport: ModalDrawer must take the centered-modal path, matching
// how an admin actually waits out a restart (same stub shape as the
// DefaultLayout tests, minus the change events these tests never fire).
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

async function mountPage(locale: Locale) {
  const i18n = createI18n({ legacy: false, locale, fallbackLocale: 'zh-CN', messages: { en, 'zh-CN': zhCN } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/login', component: { template: '<div />' } },
    ],
  })
  await router.push('/')
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  // Flush the mount-time update check (mocked, instant) and paint.
  await vi.advanceTimersByTimeAsync(0)
  await nextTick()
}

// Buttons live in the shared body (the modal teleports), so text search over
// document.body — the CCSwitchImportModal tests' approach.
function buttonByText(text: string): HTMLButtonElement {
  const btn = [...document.body.querySelectorAll('button')].find((b) => (b.textContent ?? '').includes(text))
  expect(btn, `button containing "${text}" should be rendered`).toBeTruthy()
  return btn!
}

// Walks the real flow: "Update now" opens the confirm modal, "Start update"
// POSTs (mocked to succeed) and the page enters the restart wait.
async function enterRestartWait(locale: Locale) {
  buttonByText(copy[locale].updateNow).dispatchEvent(new Event('click'))
  await nextTick()
  await nextTick()
  buttonByText(copy[locale].updateConfirmOk).dispatchEvent(new Event('click'))
  await vi.advanceTimersByTimeAsync(0)
  await nextTick()
  expect(document.body.textContent).toContain(copy[locale].restarting)
}

beforeEach(() => {
  vi.useFakeTimers()
  stubDesktopMatchMedia()
  answer = 'v0.2.5'
  versionMock.mockImplementation(() => Promise.resolve(versionInfo(answer)))
  updateMock.mockResolvedValue({ status: 'updated', target: 'v0.2.6' })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.useRealTimers()
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe.each<Locale>(['zh-CN', 'en'])('SystemInfoPage update wait (%s)', (locale) => {
  it('stays quiet before 60s, then shows the slow-migration reassurance', async () => {
    await mountPage(locale)
    await enterRestartWait(locale)
    await vi.advanceTimersByTimeAsync(59_999)
    expect(document.body.textContent, 'one tick short of 60s: no reassurance yet').not.toContain(copy[locale].restartReassurance)
    await vi.advanceTimersByTimeAsync(1)
    await nextTick()
    expect(document.body.textContent, '60s of silence: the reassurance line appears').toContain(copy[locale].restartReassurance)
  })

  it('at the 5-minute timeout the modal swaps to the diagnostic guide', async () => {
    await mountPage(locale)
    await enterRestartWait(locale)
    await vi.advanceTimersByTimeAsync(5 * 60_000)
    await nextTick()
    const text = document.body.textContent ?? ''
    expect(text).toContain(copy[locale].updateTimeoutTitle)
    expect(text, 'cause 1: not enough disk space').toContain(copy[locale].updateTimeoutCauseDisk)
    expect(text, 'cause 2: failed migration').toContain(copy[locale].updateTimeoutCauseMigration)
    expect(text, 'the log-reading command').toContain('journalctl -u yolorouter -e')
    // The wait is over without a comeback: no success copy anywhere.
    expect(text).not.toContain(copy[locale].updateDone.replace('{version}', 'v0.2.6'))
  })

  it('a different version answering finishes the wait with the success toast', async () => {
    await mountPage(locale)
    await enterRestartWait(locale)
    // One poll on the old version, then the restarted service answers.
    await vi.advanceTimersByTimeAsync(2_000)
    answer = 'v0.2.6'
    await vi.advanceTimersByTimeAsync(2_000)
    await nextTick()
    const text = document.body.textContent ?? ''
    expect(text, 'success toast with the new version').toContain(copy[locale].updateDone.replace('{version}', 'v0.2.6'))
    // No timeout copy on the success path. (The modal's leave transition
    // never fires under happy-dom + fake timers, so "modal removed from DOM"
    // is not assertable here — the success toast plus the absent timeout
    // guide are the observable outcome.)
    expect(text, 'no timeout guide on the success path').not.toContain(copy[locale].updateTimeoutTitle)
    expect(text).not.toContain(copy[locale].updateTimeoutLogHint)
  })

  it('cancelling the timed-out modal and reopening shows a fresh confirm view, not the stale guide', async () => {
    await mountPage(locale)
    await enterRestartWait(locale)
    await vi.advanceTimersByTimeAsync(5 * 60_000)
    await nextTick()
    // Premise: the wait gave up with the diagnostic guide in the modal, and
    // phase is idle again, so the modal offers its Cancel button.
    expect(document.body.textContent).toContain(copy[locale].updateTimeoutTitle)
    // Dismiss the timed-out modal, then start over via "Update now".
    buttonByText(commonCopy[locale].cancel).dispatchEvent(new Event('click'))
    await nextTick()
    await nextTick()
    buttonByText(copy[locale].updateNow).dispatchEvent(new Event('click'))
    await nextTick()
    await nextTick()
    const text = document.body.textContent ?? ''
    // The reopened view is the plain confirm dialog of a NEW attempt…
    expect(text, 'confirm title of the new attempt').toContain(copy[locale].updateConfirmTitle.replace('{version}', 'v0.2.6'))
    expect(text, 'restart warning of the confirm view').toContain(copy[locale].updateWarnRestart)
    // …without any of the previous attempt's diagnostic guide bleeding in.
    expect(text, 'no stale timeout title').not.toContain(copy[locale].updateTimeoutTitle)
    expect(text, 'no stale cause 1').not.toContain(copy[locale].updateTimeoutCauseDisk)
    expect(text, 'no stale cause 2').not.toContain(copy[locale].updateTimeoutCauseMigration)
    expect(text, 'no stale log hint').not.toContain(copy[locale].updateTimeoutLogHint)
  })
})
