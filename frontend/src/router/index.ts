import { createRouter, createWebHistory } from 'vue-router'
// The layout shell stays in the entry chunk — it renders on every
// authenticated navigation. The two unauthenticated boot pages (/setup,
// /login) are static imports too: together they're ~4 kB, and inlining
// them shaves a serial chunk round trip off the unauthenticated first
// paint. Every OTHER page is lazy (`() => import(...)`) so Vite
// code-splits one chunk per route and the entry bundle stays small;
// heavy shared libraries (naive-ui, echarts) land in shared chunks that
// load on demand with the first page that needs them.
import DefaultLayout from '../layouts/DefaultLayout.vue'
import SetupPage from '../views/auth/SetupPage.vue'
import LoginPage from '../views/auth/LoginPage.vue'
import { useAuthStore } from '../store/auth'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/setup', component: SetupPage },
    { path: '/login', component: LoginPage },
    {
      path: '/',
      component: DefaultLayout,
      // Routes without `meta.memberAllowed` are admin-only; the guard below
      // bounces a member session back to the home page. Members keep exactly
      // the self-service surface: overview, usage analytics, cost stats
      // (including their own key/model drill-downs), and their API keys.
      children: [
        { path: '', component: () => import('../views/dashboard/DashboardPage.vue'), meta: { memberAllowed: true } },
        { path: 'analytics', component: () => import('../views/analytics/AnalyticsPage.vue'), meta: { memberAllowed: true } },
        // The model comparison bench runs over the operator's own API key
        // (the backend pins both the key picker and the run to the session
        // account), so members reach it like the usage surfaces above —
        // hence the explicit memberAllowed opt-in.
        { path: 'playground', component: () => import('../views/playground/PlaygroundPage.vue'), meta: { memberAllowed: true } },
        { path: 'costs', component: () => import('../views/costs/CostStatsPage.vue'), meta: { memberAllowed: true } },
        { path: 'cost-optimization', component: () => import('../views/costs/CostOptimizationPage.vue') },
        { path: 'costs/keys/:id(\\d+)', component: () => import('../views/costs/KeyCostDetailPage.vue'), meta: { memberAllowed: true } },
        // Bare `costs/models` must come before the catch-all so the `?name=`
        // dot-segment fallback still matches here instead of being swallowed
        // by `:name(.*)`.
        { path: 'costs/models', component: () => import('../views/costs/ModelCostDetailPage.vue'), meta: { memberAllowed: true } },
        { path: 'costs/models/:name(.*)', component: () => import('../views/costs/ModelCostDetailPage.vue'), meta: { memberAllowed: true } },
        { path: 'costs/providers/:id(\\d+)', component: () => import('../views/costs/ProviderCostDetailPage.vue') },
        { path: 'request-logs', component: () => import('../views/request-logs/RequestLogListPage.vue') },
        { path: 'request-logs/:requestId', component: () => import('../views/request-logs/RequestLogDetailPage.vue') },
        // The console's own change trail (who changed what, field-level
        // diffs, denied/error attempts). Admin-only like the request-log
        // routes above it (no meta.memberAllowed), matching the backend's
        // protected-group registration for /api/admin/audit-logs.
        { path: 'audit-logs', component: () => import('../views/audit/AuditLogListPage.vue') },
        // Tool-session aggregate views — admin-only like the request-log
        // routes above them (no meta.memberAllowed), matching the backend's
        // protected-group registration for /api/admin/agent-sessions.
        { path: 'agent-sessions', component: () => import('../views/agent-sessions/AgentSessionListPage.vue') },
        { path: 'agent-sessions/:sessionId', component: () => import('../views/agent-sessions/AgentSessionDetailPage.vue') },
        { path: 'providers', component: () => import('../views/providers/ProviderListPage.vue') },
        { path: 'providers/:id', component: () => import('../views/providers/ProviderDetailPage.vue') },
        { path: 'models', component: () => import('../views/models/ModelListPage.vue') },
        { path: 'models/:id', component: () => import('../views/models/ModelDetailPage.vue') },
        // Per-model price comparison across every provider mapping. Admin-only
        // like the routes around it — no meta.memberAllowed, so the guard
        // below keeps member sessions on their self-service surface.
        { path: 'model-pricing', component: () => import('../views/models/ModelPricingPage.vue') },
        { path: 'rate-limits', component: () => import('../views/ratelimits/RateLimitListPage.vue') },
        { path: 'api-keys', component: () => import('../views/apikeys/ApiKeyListPage.vue'), meta: { memberAllowed: true } },
        { path: 'oauth-providers', component: () => import('../views/oauth/OAuthProviderListPage.vue') },
        { path: 'users', component: () => import('../views/users/UserListPage.vue') },
        // The System Settings group's persistent-form page (retention, key
        // auto recovery, ...). Admin-only like /about — no memberAllowed.
        { path: 'settings/general', component: () => import('../views/settings/GeneralSettingsPage.vue') },
        { path: 'about', component: () => import('../views/system/SystemInfoPage.vue') },
      ],
    },
    // The catch-all: an unknown path used to render the layout with an
    // empty router-view — a blank page that looks broken. Every real
    // surface lives under an authenticated layout, so sending unknown
    // paths to the home page (and letting the auth guard take it from
    // there) matches what the operator meant by the URL.
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

// Chunk-load failures: the console ships inside the Go binary, so a binary
// upgrade swaps every asset hash at once. A session booted from the old
// build still holds the old lazy-chunk URLs in memory — its next navigation
// would 404 the page chunk and die as a blank route. Both of Vite's chunk
// failure wordings ("Failed to fetch dynamically imported module" on
// Chromium, "Importing a module script failed" on Firefox/WebKit) mean the
// on-disk build changed under us, so fall back to one full page load — it
// re-fetches the fresh index.html and every new hash. ONE-SHOT PER TAB
// SESSION via a sessionStorage marker: `location.reload()` destroys this JS
// module, so a module-level flag would reset on every reload and a genuinely
// broken deploy (fresh build missing a chunk) would reload-loop forever.
// sessionStorage survives the reload yet dies with the tab, so each tab gets
// exactly one recovery attempt per session and a broken deploy settles into
// a dead page instead of a loop. SSR absence of `window` just skips the
// recovery.
const CHUNK_RELOAD_KEY = 'yolorouter-chunk-reload'
router.onError((err) => {
  const isChunkLoad =
    err instanceof Error &&
    /Failed to fetch dynamically imported module|Importing a module script failed/.test(err.message)
  if (
    typeof window === 'undefined' ||
    !isChunkLoad ||
    window.sessionStorage.getItem(CHUNK_RELOAD_KEY) !== null
  ) {
    return
  }
  window.sessionStorage.setItem(CHUNK_RELOAD_KEY, '1')
  window.location.reload()
})

let stateChecked = false
// Memoizes the in-flight checkState() call: if a second navigation enters
// this guard before the first checkState() call resolves (e.g. two rapid
// navigations racing at app boot), it awaits the SAME promise instead of
// firing a second, fully redundant /auth/state + /auth/me round trip.
let stateCheckPromise: Promise<void> | null = null

router.beforeEach(async (to) => {
  const authStore = useAuthStore()

  // Only query /auth/state + try to restore login state once, on the
  // first navigation after app boot — not on every route change. Any
  // later login/logout/change-password action keeps authStore's state in
  // sync directly, so there's never a need to re-query.
  if (!stateChecked) {
    if (!stateCheckPromise) {
      stateCheckPromise = authStore.checkState()
    }
    try {
      await stateCheckPromise
      stateChecked = true
    } catch (err) {
      // /auth/state itself is unreachable (network failure, backend 500,
      // ...) — that's a dependency outage, not "not logged in". This must
      // still fail CLOSED, not open: letting the navigation through
      // unconditionally would render DefaultLayout and whatever protected
      // page was requested without ever having confirmed an admin
      // identity, breaking the "anonymous users can't reach admin pages"
      // invariant at the frontend layer (the backend's own
      // RequireSession middleware still protects the actual data,
      // but every future admin page would silently inherit this same
      // fail-open shell). Send unconfirmed requests to /login instead —
      // don't set stateChecked, so the next navigation (including a
      // retry from the login page itself) re-attempts the check.
      stateCheckPromise = null
      console.error('failed to resolve auth state', err)
      return to.path === '/login' ? true : '/login'
    }
  }

  // System not initialized yet: send everything except /setup there.
  if (authStore.initialized === false && to.path !== '/setup') {
    return '/setup'
  }
  // Initialized but not logged in: /setup no longer makes sense (folded
  // into the same /login redirect as the branch below) — send everything
  // except /login there.
  if (authStore.initialized === true && !authStore.isLoggedIn && to.path !== '/login') {
    return '/login'
  }
  // Already logged in and trying to visit /login or /setup: pointless,
  // just send them back to the home page.
  if (authStore.isLoggedIn && (to.path === '/login' || to.path === '/setup')) {
    return '/'
  }
  // A member session on an admin-only page: bounce to the overview. The
  // backend's RequireAdmin middleware protects the actual data either way;
  // this guard just keeps members from ever rendering a shell full of
  // forbidden-request toasts. Gated on the meta.memberAllowed flag (not a
  // path allowlist) so a newly added route is admin-only until it
  // explicitly opts in.
  if (authStore.isLoggedIn && !authStore.isAdmin && !to.meta.memberAllowed && to.path !== '/') {
    return '/'
  }
  return true
})
