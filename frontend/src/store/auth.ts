import { defineStore } from 'pinia'
import * as authApi from '../api/auth'
import { APIError } from '../api/client'
import { ACCOUNT_SESSION_INVALID } from '../api/errcodes'

interface AuthStoreState {
  /** null = /auth/state hasn't been queried yet; cached after the first query so we don't hit it on every route change. */
  initialized: boolean | null
  username: string | null
  /** Minutes east of UTC for the server's timezone, or null before /auth/me
   *  resolves. The analytics time-range picker uses it to align preset
   *  windows with the server's natural day. */
  serverTimezoneOffset: number | null
  /**
   * Set whenever handleSessionExpired fires (a genuinely mid-use session
   * expiry, caught by withSessionInvalidHandling on some later
   * authenticated call — never by checkState() itself, see its comment
   * below) — LoginPage reads and consumes this via
   * consumeSessionExpiredNotice to show the required
   * session-expired message (errcode ACCOUNT_SESSION_INVALID,
   * locales/zh-CN/errcodes.ts). Without this, the most common path to
   * /login (the 24h session TTL simply expiring) silently drops the user
   * on a blank login form with no explanation.
   */
  sessionExpiredNotice: boolean
}

export const useAuthStore = defineStore('auth', {
  state: (): AuthStoreState => ({
    initialized: null,
    username: null,
    sessionExpiredNotice: false,
    serverTimezoneOffset: null,
  }),
  getters: {
    // Derived rather than a separately-tracked field: username and
    // isLoggedIn were previously two independent state fields that every
    // action had to assign in lockstep, which only stays consistent for
    // as long as every future edit remembers to touch both.
    isLoggedIn: (state): boolean => state.username !== null,
  },
  actions: {
    /**
     * Called once at app boot: checks whether the system is initialized
     * and tries to restore login state from an existing cookie.
     *
     * getMe() only swallows AccountSessionInvalid; every other error
     * (network failure, backend 500, other dependency outages) is
     * rethrown as-is for the caller (the router guard) to decide whether
     * to retry — silently treating those as "not logged in" here would be
     * wrong: a dependency outage isn't the same as actually being logged
     * out, and disguising it as the latter sends the user to the login
     * page and never auto-retries for the rest of this app lifecycle.
     *
     * On AccountSessionInvalid, this clears local state silently WITHOUT
     * setting sessionExpiredNotice: the backend returns the exact same
     * error code for "no cookie at all" and "cookie present but
     * expired/unknown" (internal/middleware/auth.go), and at app boot
     * there is no prior local evidence this browser was ever
     * authenticated — showing "session expired" here would mislead a
     * visitor who simply never logged in. A genuine mid-use expiry (the
     * user WAS just interacting with the app) is instead caught by
     * withSessionInvalidHandling on whichever later authenticated call
     * fails, which does set the notice via handleSessionExpired().
     */
    async checkState() {
      const state = await authApi.getAuthState()
      this.initialized = state.initialized
      if (state.initialized) {
        try {
          const me = await authApi.getMe()
          this.username = me.username
          this.serverTimezoneOffset = me.server_timezone_offset ?? null
        } catch (err) {
          if (err instanceof APIError && err.code === ACCOUNT_SESSION_INVALID) {
            this.username = null
            return
          }
          throw err
        }
      }
    },
    async setup(username: string, password: string) {
      const admin = await authApi.setup(username, password)
      this.initialized = true
      this.username = admin.username
      this.serverTimezoneOffset = admin.server_timezone_offset ?? null
    },
    async login(username: string, password: string) {
      const admin = await authApi.login(username, password)
      this.username = admin.username
      this.serverTimezoneOffset = admin.server_timezone_offset ?? null
    },
    async logout() {
      await authApi.logout()
      this.username = null
    },
    async changePassword(currentPassword: string, newPassword: string) {
      await authApi.changePassword(currentPassword, newPassword)
      // The backend already deleted every session issued before this
      // change (including the current one).
      this.username = null
    },
    /** Called whenever any request catches AccountSessionInvalid — clears local state; the caller navigates to /login. */
    handleSessionExpired() {
      this.username = null
      this.sessionExpiredNotice = true
    },
    /** Reads and clears the pending notice in one step, so it's shown at most once per expiry. */
    consumeSessionExpiredNotice(): boolean {
      const had = this.sessionExpiredNotice
      this.sessionExpiredNotice = false
      return had
    },
    /**
     * Called by SetupPage.vue when a concurrent setup attempt already won
     * (ACCOUNT_SETUP_ALREADY_DONE): that response is itself authoritative
     * proof setup is done, so this updates the flag directly rather than
     * round-tripping through checkState() (which could fail independently
     * on an unrelated network error and leave the caller stuck). Kept as
     * an action — not a direct external assignment — so every write to
     * this store's state goes through one place.
     */
    markInitialized() {
      this.initialized = true
    },
  },
})
