// @vitest-environment happy-dom
//
// Route-guard coverage for the Playground page's reachability (acceptance
// case TC-24): an anonymous visit to /playground must land on /login, a
// member session must pass (the route's memberAllowed declaration is what
// lets it), and an admin-only route must still bounce the member — proving
// the guard really reads the flag instead of letting everything through.
//
// These drive the REAL router singleton (its own beforeEach guard, the
// real route table), with only the auth API mocked; the guard's state
// check is memoized module-side after the first navigation, so all arms
// share one it() in a fixed order: boot anonymous, then patch the store
// into a member session, then an admin one.
import { describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

import { getAuthState, getMe } from '../api/auth'
import { APIError } from '../api/client'
import { ACCOUNT_SESSION_INVALID } from '../api/errcodes'

vi.mock('../api/auth', () => ({
  getAuthState: vi.fn(),
  getMe: vi.fn(),
  login: vi.fn(),
  setup: vi.fn(),
  logout: vi.fn(),
  changePassword: vi.fn(),
}))

const stateMock = vi.mocked(getAuthState)
const meMock = vi.mocked(getMe)

// Must be imported after the vi.mock factory is registered. Importing the
// router also imports every page component, which is exactly what the real
// app boots — nothing here mounts them, navigation only resolves guards.
import { router } from './index'
import { useAuthStore } from '../store/auth'

describe('router guard: /playground reachability (TC-24)', () => {
  it('bounces anonymous visits to /login, admits members via memberAllowed, and still guards admin-only routes', async () => {
    // Boot: initialized instance, but the session cookie is gone — the
    // same answer the backend gives for both "no cookie" and "expired
    // cookie", which checkState() turns into a logged-out store.
    stateMock.mockResolvedValue({ initialized: true })
    meMock.mockRejectedValue(new APIError(ACCOUNT_SESSION_INVALID))

    const pinia = createPinia()
    setActivePinia(pinia)
    const auth = useAuthStore()

    // The route itself carries the declaration under test.
    expect(router.resolve('/playground').meta.memberAllowed, 'the playground route declares memberAllowed').toBe(true)

    // Anonymous: /playground must not render — straight to /login.
    await router.push('/playground')
    expect(router.currentRoute.value.path, 'anonymous /playground lands on /login').toBe('/login')

    // A member session: the declaration lets the member through.
    auth.$patch({ initialized: true, username: 'member', role: 'member', isLocal: true, isBootstrap: false })
    await router.push('/playground')
    expect(router.currentRoute.value.path, 'member session reaches /playground').toBe('/playground')

    // The same member on an admin-only route (no memberAllowed meta) is
    // still bounced — the flag opt-in is what carries the page, not a
    // guard that forgot how to refuse.
    await router.push('/users')
    expect(router.currentRoute.value.path, 'member session is bounced from an admin-only route').toBe('/')

    // An admin session passes everywhere, /playground included.
    auth.$patch({ role: 'admin' })
    await router.push('/playground')
    expect(router.currentRoute.value.path, 'admin session reaches /playground').toBe('/playground')
  })
})
