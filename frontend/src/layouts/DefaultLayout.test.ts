// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the app shell's two mobile-a11y/resize behaviors:
//
//   - the mobile hamburger's aria-label must read nav.menu ("Menu"/
//     "菜单") — it previously read nav.overview, naming the first nav entry
//     instead of what the button does.
//   - crossing the mobile breakpoint with the language sheet open must
//     not strand the desktop modal's click-blocking .n-modal-mask over the
//     restored desktop layout (and must drop the mobile overlays too).
//
// The breakpoint is driven through a stubbed window.matchMedia so the same
// MediaQueryList listeners naive/Vue register in the browser fire here —
// useIsMobile's isMobile ref flips exactly as on a live viewport resize.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import { useAuthStore } from '../store/auth'
import en from '../locales/en'
import zhCN from '../locales/zh-CN'
import DefaultLayout from './DefaultLayout.vue'

// The admin-role test below fires the shell's onMounted update check; mock
// the version endpoint so it resolves instantly instead of attempting a real
// network request from the test worker.
vi.mock('../api/system', () => ({
  getSystemVersion: vi.fn(() =>
    Promise.resolve({
      version: 'test',
      commit: '',
      build_time: '',
      go_version: '',
      goos: '',
      goarch: '',
      db_driver: 'sqlite',
      update_mode: 'disabled',
      uptime_seconds: 1,
      latest: '',
      has_update: false,
      release_url: '',
      check_failed: false,
    }),
  ),
}))

// App.vue nests the router-view under n-config-provider > n-message-provider
// > n-dialog-provider; DefaultLayout's useDialog()/useMessage() throw without
// that ancestry, so tests mount the shell inside the same providers via a
// minimal host.
const Host = defineComponent({
  components: { DefaultLayout },
  template: '<NConfigProvider><NMessageProvider><NDialogProvider><DefaultLayout /></NDialogProvider></NMessageProvider></NConfigProvider>',
})

// A controllable MediaQueryList: `setMobile` updates `matches` and dispatches
// a `change` event to every registered listener, which is exactly the surface
// useIsMobile (and naive-ui) consume. happy-dom's own matchMedia exists but
// offers no deterministic way to fire the change event on demand.
function stubMatchMedia(initialMobile: boolean) {
  const listeners = new Set<(e: { matches: boolean }) => void>()
  const mql = {
    matches: initialMobile,
    media: '(max-width: 768px)',
    onchange: null as ((e: { matches: boolean }) => void) | null,
    addEventListener: (_type: string, cb: (e: { matches: boolean }) => void) => {
      listeners.add(cb)
    },
    removeEventListener: (_type: string, cb: (e: { matches: boolean }) => void) => {
      listeners.delete(cb)
    },
    addListener: (cb: (e: { matches: boolean }) => void) => listeners.add(cb),
    removeListener: (cb: (e: { matches: boolean }) => void) => listeners.delete(cb),
    dispatchEvent: () => true,
  }
  vi.stubGlobal('matchMedia', vi.fn(() => mql))
  return {
    setMobile(mobile: boolean) {
      mql.matches = mobile
      for (const cb of [...listeners]) cb({ matches: mobile })
    },
  }
}

async function mountLayout(locale: 'en' | 'zh-CN', role: 'admin' | 'member' = 'member') {
  const i18n = createI18n({
    legacy: false,
    locale,
    fallbackLocale: 'zh-CN',
    messages: { en, 'zh-CN': zhCN },
  })
  const router = createRouter({
    history: createMemoryHistory(),
    // Every path the shell links to resolves — including /, which both the
    // brand logo RouterLinks and the member sidebar's overview entry target —
    // so RouterLink doesn't warn about unmatched locations (they all render
    // the same dummy page). Admin-role tests need the full admin link set.
    routes: [
      '/',
      '/analytics',
      '/playground',
      '/request-logs',
      '/agent-sessions',
      '/costs',
      '/providers',
      '/models',
      '/api-keys',
      '/rate-limits',
      '/cost-optimization',
      '/users',
      // The admin sidebar's audit entry targets /audit-logs; leaving it out
      // of this fixture made every admin-role test log the vue-router
      // "No match found" warn.
      '/audit-logs',
      '/oauth-providers',
      '/settings/general',
      '/about',
    ].map((path) => ({
      path,
      component: { template: '<div />' },
    })),
  })
  await router.push('/analytics')

  // A member account keeps the shell light for these tests: no admin-only
  // update check fires on mount, and the sidebar still carries the language
  // entry both fixes exercise. Admin-role tests pass role explicitly (their
  // onMounted update check is fail-open: a network failure just sets the
  // store's checkFailed flag, never throws).
  const pinia = createPinia()
  setActivePinia(pinia)
  useAuthStore().$patch({ username: 'tester', role, isLocal: false })

  const wrapper = mount(Host, {
    attachTo: document.body,
    global: { plugins: [pinia, router, i18n, naive] },
  })
  await router.isReady()
  await nextTick()
  return wrapper
}

let wrapper: VueWrapper | null = null
let media: ReturnType<typeof stubMatchMedia>

beforeEach(() => {
  media = stubMatchMedia(true)
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
})

describe('DefaultLayout mobile shell', () => {
  it('labels the hamburger with nav.menu, not the first nav entry (en)', async () => {
    wrapper = await mountLayout('en')
    const hamburger = document.querySelector('.mobile-topbar__menu')
    expect(hamburger, 'mobile top bar renders the hamburger button').not.toBeNull()
    expect(hamburger!.getAttribute('aria-label')).toBe(en.nav.menu)
    // The original mislabel: aria-label must not name the first nav entry.
    expect(hamburger!.getAttribute('aria-label')).not.toBe(en.nav.overview)
  })

  it('labels the hamburger with nav.menu in zh-CN too', async () => {
    wrapper = await mountLayout('zh-CN')
    const hamburger = document.querySelector('.mobile-topbar__menu')
    expect(hamburger).not.toBeNull()
    expect(hamburger!.getAttribute('aria-label')).toBe(zhCN.nav.menu)
  })

  it('drops the open language sheet (and its mask) when the viewport grows past the breakpoint, leaving no desktop modal mask behind', async () => {
    wrapper = await mountLayout('en')

    // Open the nav drawer, then the language bottom sheet from it, so both
    // overlays' open state is live when the breakpoint flips below.
    const hamburger = document.querySelector<HTMLButtonElement>('.mobile-topbar__menu')
    hamburger!.click()
    await nextTick()
    const languageEntry = [...document.querySelectorAll<HTMLButtonElement>('button.sidebar-nav-item')].find((b) =>
      b.textContent?.includes(en.nav.language),
    )
    expect(languageEntry, 'drawer exposes the language entry').toBeTruthy()
    languageEntry!.click()
    await nextTick()

    // Precondition guards: the sheet really is open (its drawer + mask are in
    // the teleported body), so the post-resize assertions cannot pass vacuously.
    expect(document.querySelector('.option-sheet'), 'language bottom sheet is open').not.toBeNull()
    expect(document.querySelector('.n-drawer-mask'), 'sheet mask is present while open').not.toBeNull()

    // Resize to desktop (matchMedia flips past the 768px breakpoint).
    media.setMobile(false)
    await nextTick()
    await nextTick()

    // The mobile overlays must be gone…
    expect(document.querySelector('.option-sheet'), 'bottom sheet unmounted on breakpoint cross').toBeNull()
    expect(document.querySelector('.n-drawer-mask'), 'no drawer mask survives the breakpoint cross').toBeNull()
    // …and the desktop language modal must NOT have mounted with the stale
    // open state — its .n-modal-mask would block every click on the
    // restored sidebar.
    expect(document.querySelector('.n-modal-mask'), 'no orphan modal mask over the desktop layout').toBeNull()
    expect(document.querySelector('.n-modal'), 'desktop language modal did not open on its own').toBeNull()
  })

  it('closes the desktop language modal state when shrinking to mobile, so the sheet does not pop open on its own (reverse direction)', async () => {
    media.setMobile(false)
    wrapper = await mountLayout('en')

    // Desktop language modal is reachable from the sidebar's System group.
    const languageEntry = [...document.querySelectorAll<HTMLButtonElement>('button.sidebar-nav-item')].find((b) =>
      b.textContent?.includes(en.nav.language),
    )
    expect(languageEntry, 'desktop sidebar exposes the language entry').toBeTruthy()
    languageEntry!.click()
    await nextTick()
    expect(document.querySelector('.n-modal-mask'), 'desktop language modal is open').not.toBeNull()

    // Shrink to mobile: the modal state must reset rather than reopen as the
    // bottom sheet, mirroring the leave-mobile direction.
    media.setMobile(true)
    await nextTick()
    await nextTick()

    expect(document.querySelector('.n-modal-mask'), 'modal mask gone after shrinking').toBeNull()
    expect(document.querySelector('.option-sheet'), 'mobile sheet did not pop open on its own').toBeNull()
  })
})

// TC-11's sidebar arms, pinned after the key-auto-recovery modal entry was
// removed (its form now lives on /settings/general): the member branch keeps
// exactly its self-service surface (language entry included — the members'
// sidebar branch is left untouched), and the admin branch links the General
// page while the old modal-launcher entry is gone entirely.
describe('DefaultLayout sidebar after the key-recovery modal removal (TC-11)', () => {
  it('member sidebar keeps the language entry and no admin-only entries appear', async () => {
    // Mobile (the beforeEach default): the member nav lives in the drawer,
    // so open it first — same path the existing shell tests use.
    wrapper = await mountLayout('en')
    document.querySelector<HTMLButtonElement>('.mobile-topbar__menu')!.click()
    await nextTick()
    const navText = document.querySelector('.sidebar-nav')?.textContent ?? ''
    expect(navText, 'member sidebar still carries the language entry').toContain(en.nav.language)
    // The member branch stays the self-service surface: the admin-only
    // pages/entries must not leak into it.
    expect(navText).not.toContain(en.nav.generalSettings)
    expect(navText).not.toContain(en.nav.about)
    expect(navText).not.toContain('Key Auto Recovery')
  })

  it('admin sidebar links General and keeps Language, with no modal-launcher entry left', async () => {
    media.setMobile(false)
    wrapper = await mountLayout('en', 'admin')
    const navText = document.querySelector('.sidebar-nav')?.textContent ?? ''
    expect(navText, 'admin sidebar links the General settings page').toContain(en.nav.generalSettings)
    expect(navText, 'admin sidebar keeps its own language entry').toContain(en.nav.language)
    // The removed entry's label — the nav copy itself is deleted from both
    // locale trees, so this also holds for a zh-CN render.
    expect(navText).not.toContain('Key Auto Recovery')
  })
})

// The Playground entry arms (TC-06): the comparison bench is reachable
// from BOTH sidebar branches — members run it on their own key, so the
// member branch carries the same entry the admin branch does.
//
// Menu-reposition arms (TC-25/26/27): the entry is now a TOP-LEVEL item —
// directly below Overview in document order, ahead of the analytics group
// heading and of Usage — in the admin sidebar (TC-25), the member sidebar
// (TC-26), and the member mobile drawer (TC-27) alike. It used to sit
// inside the analytics group next to Usage; the nav level now reads
// "workbench", not "report".
describe('DefaultLayout sidebar Playground entry (TC-06, TC-25/26/27)', () => {
  const playgroundLink = (): HTMLAnchorElement | null =>
    document.querySelector<HTMLAnchorElement>('a[href="/playground"]')

  // Position oracle for the repositioned entry, shared by every arm below:
  // within the rendered nav, Playground must come after Overview and before
  // both the analytics group heading and Usage. The DOM-order check against
  // the real group-heading node (not just text indexes) is what proves
  // "top-level, outside the analytics group".
  function expectPlaygroundTopLevel(labels: { overview: string; group: string; usage: string; playground: string }) {
    const link = playgroundLink()
    expect(link, 'sidebar links /playground').not.toBeNull()
    expect(link!.textContent).toContain(labels.playground)

    const nav = document.querySelector('.sidebar-nav')
    expect(nav, 'sidebar nav rendered').not.toBeNull()
    const navText = nav!.textContent ?? ''
    expect(
      navText.indexOf(labels.playground),
      'Playground comes after Overview in the nav text order',
    ).toBeGreaterThan(navText.indexOf(labels.overview))
    expect(
      navText.indexOf(labels.playground),
      'Playground comes before Usage in the nav text order',
    ).toBeLessThan(navText.indexOf(labels.usage))

    const groupHeading = [...nav!.querySelectorAll<HTMLElement>('.sidebar-nav__group')].find((g) =>
      g.textContent?.includes(labels.group),
    )
    expect(groupHeading, 'the analytics group heading is rendered').toBeTruthy()
    expect(
      link!.compareDocumentPosition(groupHeading!) & Node.DOCUMENT_POSITION_FOLLOWING,
      'the Playground link renders before the analytics group heading (top-level item, outside the group)',
    ).toBeTruthy()
  }

  it('member sidebar carries the Playground entry as a top-level item below Overview (TC-26)', async () => {
    media.setMobile(false)
    wrapper = await mountLayout('en', 'member')
    expectPlaygroundTopLevel({
      overview: en.nav.overview,
      group: en.nav.groupAnalytics,
      usage: en.nav.usage,
      playground: en.nav.playground,
    })
  })

  it('admin sidebar carries the Playground entry as a top-level item below Overview too (TC-25)', async () => {
    media.setMobile(false)
    wrapper = await mountLayout('en', 'admin')
    expectPlaygroundTopLevel({
      overview: en.nav.overview,
      group: en.nav.groupAnalytics,
      usage: en.nav.usage,
      playground: en.nav.playground,
    })
  })

  it('the member drawer (mobile) exposes the entry as a top-level item as well (TC-27)', async () => {
    // Mobile (the beforeEach default): the member nav lives in the drawer.
    wrapper = await mountLayout('zh-CN', 'member')
    document.querySelector<HTMLButtonElement>('.mobile-topbar__menu')!.click()
    await nextTick()
    expectPlaygroundTopLevel({
      overview: zhCN.nav.overview,
      group: zhCN.nav.groupAnalytics,
      usage: zhCN.nav.usage,
      playground: zhCN.nav.playground,
    })
  })
})
