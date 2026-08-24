import type { Router } from 'vue-router'
import { errorCodeOf } from '../api/client'
import { ACCOUNT_SESSION_INVALID } from '../api/errcodes'
import { useAuthStore } from '../store/auth'

// redirectIfSessionExpired routes a lapsed admin session to reauth: it clears
// the auth state AND navigates — handleSessionExpired alone does not navigate,
// and route guards don't rerun on a store change, so without the push the
// protected shell stays visible with dead data. Returns true when it fired, so
// polling callers stop retrying against a dead session instead of hammering it
// on every tick. Must run BEFORE any stale-request guard: a superseded fetch
// that hit session expiry is still a session expiry.
export function redirectIfSessionExpired(err: unknown, router: Router): boolean {
  if (errorCodeOf(err) !== ACCOUNT_SESSION_INVALID) return false
  useAuthStore().handleSessionExpired()
  void router.push('/login')
  return true
}
