import { apiFetch, APIError } from './client'
import { ACCOUNT_SESSION_INVALID } from './errcodes'
import { router } from '../router'
import { useAuthStore } from '../store/auth'

export interface AuthState {
  initialized: boolean
}

export interface AdminInfo {
  username: string
  /** Minutes east of UTC for the server's timezone. Only /auth/me populates
   *  it; the analytics time-range picker uses it so "Today"/"Yesterday"
   *  presets match the server's natural day instead of the browser's. */
  server_timezone_offset?: number
}

export interface LoginLockedData {
  locked_until: number
}

/**
 * Design doc §7 "unified 401 handling": every call in this module that hits an
 * admin-session-gated route funnels through here, so an AccountSessionInvalid
 * response (session expired, or the cookie was never valid) always clears
 * local auth state and sends the user back to /login — no matter which
 * screen/action triggered the request. Deliberately kept local to this
 * module rather than folded into apiFetch/client.ts: other API modules
 * (e.g. the future /v1 gateway proxy) must not inherit this behavior.
 * The error is still rethrown so callers can show their own toast/UI state
 * for every other error case.
 */
async function withSessionInvalidHandling<T>(request: Promise<T>): Promise<T> {
  try {
    return await request
  } catch (err) {
    if (err instanceof APIError && err.code === ACCOUNT_SESSION_INVALID) {
      useAuthStore().handleSessionExpired()
      if (router.currentRoute.value.path !== '/login') {
        // Best-effort: if a navigation is already converging on /login
        // (e.g. the route guard's own redirect), vue-router may reject
        // this as a redundant/duplicated navigation — that's fine, the
        // end state is the same either way.
        void router.push('/login').catch(() => {})
      }
    }
    throw err
  }
}

export function getAuthState(): Promise<AuthState> {
  return apiFetch<AuthState>('/api/admin/auth/state')
}

export function setup(username: string, password: string): Promise<AdminInfo> {
  return apiFetch<AdminInfo>('/api/admin/auth/setup', {
    method: 'POST',
    body: JSON.stringify({ username, password }),
  })
}

export function login(username: string, password: string): Promise<AdminInfo> {
  return apiFetch<AdminInfo>('/api/admin/auth/login', {
    method: 'POST',
    body: JSON.stringify({ username, password }),
  })
}

export function logout(): Promise<void> {
  return withSessionInvalidHandling(apiFetch<void>('/api/admin/auth/logout', { method: 'POST' }))
}

// getMe deliberately does NOT go through withSessionInvalidHandling: its
// only caller, authStore.checkState() (the app-boot check), already has
// its own complete handling of ACCOUNT_SESSION_INVALID — reusing the
// wrapper here would set sessionExpiredNotice=true and push to /login as
// a side effect of THIS call, before checkState()'s own catch even runs,
// defeating its documented "never-logged-in visitors must not see a
// session-expired message" behavior. If a genuinely mid-use caller needs
// this endpoint later, wrap that specific call site instead of this
// shared function.
export function getMe(): Promise<AdminInfo> {
  return apiFetch<AdminInfo>('/api/admin/auth/me')
}

export function changePassword(currentPassword: string, newPassword: string): Promise<void> {
  return withSessionInvalidHandling(
    apiFetch<void>('/api/admin/auth/password', {
      method: 'PUT',
      body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }),
    }),
  )
}
