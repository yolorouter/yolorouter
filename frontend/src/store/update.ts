import { defineStore } from 'pinia'
import { getSystemVersion, type SystemVersion } from '../api/system'
import { APIError } from '../api/client'
import { ACCOUNT_SESSION_INVALID } from '../api/errcodes'
import { useAuthStore } from './auth'
import { router } from '../router'

// useUpdateStore holds the system-version response (build metadata + update
// status) that the sidebar badge (DefaultLayout) and the System Info page both
// read. It is a shared Pinia singleton — DefaultLayout's onMounted fires the
// one background check on app boot, and SystemInfoPage reloads on mount — so
// both read one race-guarded source instead of each issuing its own request
// and racing (frontend-conventions: any store action reachable from more than
// one component carries the fetch-token guard).
interface UpdateStoreState {
  // Build/runtime metadata (mirrors SystemVersion's build fields).
  version: string
  commit: string
  buildTime: string
  goVersion: string
  goos: string
  goarch: string
  dbDriver: string
  uptimeSeconds: number
  // Update status.
  latest: string
  hasUpdate: boolean
  releaseUrl: string
  checkFailed: boolean
  // Monotonic fetch token: an in-flight check captures the current value and
  // only writes state if it is still the latest when the response lands, so a
  // slow stale response can't overwrite a newer one.
  lastFetchId: number
}

export const useUpdateStore = defineStore('update', {
  state: (): UpdateStoreState => ({
    version: '',
    commit: '',
    buildTime: '',
    goVersion: '',
    goos: '',
    goarch: '',
    dbDriver: '',
    uptimeSeconds: 0,
    latest: '',
    hasUpdate: false,
    releaseUrl: '',
    checkFailed: false,
    lastFetchId: 0,
  }),
  actions: {
    /**
     * Refresh the system-version state from the backend. NEVER throws: a
     * failed check (network error, backend 500) just sets checkFailed and
     * leaves hasUpdate false, so DefaultLayout's onMounted can
     * fire-and-forget this without a try/catch wrapper and without surfacing
     * a misleading toast. The lastFetchId guard prevents a stale response
     * (e.g. from a /system page load racing a DefaultLayout mount) from
     * overwriting a newer one.
     */
    async checkForUpdates() {
      const fetchId = ++this.lastFetchId
      try {
        const info: SystemVersion = await getSystemVersion()
        // A newer checkForUpdates() started while this one was in flight —
        // its result is authoritative, leave state untouched.
        if (fetchId !== this.lastFetchId) return
        this.version = info.version
        this.commit = info.commit
        this.buildTime = info.build_time
        this.goVersion = info.go_version
        this.goos = info.goos
        this.goarch = info.goarch
        this.dbDriver = info.db_driver
        this.uptimeSeconds = info.uptime_seconds
        this.latest = info.latest
        this.hasUpdate = info.has_update
        this.releaseUrl = info.release_url
        this.checkFailed = info.check_failed
      } catch (err) {
        // Session expiry must be handled regardless of whether this request
        // is now stale: an older overlapping check that receives
        // ACCOUNT_SESSION_INVALID after a newer one started still means the
        // admin's session lapsed, and must trigger reauth — the fetch-token
        // guard suppresses update-state writes, not session-expiry handling
        // (Codex review P2).
        if (err instanceof APIError && err.code === ACCOUNT_SESSION_INVALID) {
          useAuthStore().handleSessionExpired()
          // handleSessionExpired only clears Pinia state + sets the notice;
          // the caller must navigate to /login (route guards don't rerun on
          // store change), otherwise the protected shell + stale admin data
          // stay visible (Codex review P2).
          router.push('/login')
          return
        }
        if (fetchId !== this.lastFetchId) return
        // A real network/parse failure (not the backend's "check_failed"
        // flag, which is a normal 200 response): keep the badge off, clear
        // stale latest/releaseUrl so a failed check doesn't leave the page
        // showing a previous release's URL as if current, and mark the check
        // as failed so the page can say so (Codex review P2).
        this.latest = ''
        this.releaseUrl = ''
        this.hasUpdate = false
        this.checkFailed = true
      }
    },
  },
})
