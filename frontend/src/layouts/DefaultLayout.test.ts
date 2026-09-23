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

async function mountLayout(locale: 'en' | 'zh-CN') {
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
    // the same dummy page).
    routes: ['/', '/analytics', '/costs', '/api-keys'].map((path) => ({
      path,
      component: { template: '<div />' },
    })),
  })
  await router.push('/analytics')

  // A member account keeps the shell light for these tests: no admin-only
  // update check fires on mount, and the sidebar still carries the language
  // entry both fixes exercise.
  const pinia = createPinia()
  setActivePinia(pinia)
  useAuthStore().$patch({ username: 'tester', role: 'member', isLocal: false })

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
