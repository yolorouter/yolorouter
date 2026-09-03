import { createRouter, createWebHistory } from 'vue-router'
import DefaultLayout from '../layouts/DefaultLayout.vue'
import SetupPage from '../views/auth/SetupPage.vue'
import LoginPage from '../views/auth/LoginPage.vue'
import DashboardPage from '../views/dashboard/DashboardPage.vue'
import AnalyticsPage from '../views/analytics/AnalyticsPage.vue'
import CostStatsPage from '../views/costs/CostStatsPage.vue'
import CostOptimizationPage from '../views/costs/CostOptimizationPage.vue'
import KeyCostDetailPage from '../views/costs/KeyCostDetailPage.vue'
import ModelCostDetailPage from '../views/costs/ModelCostDetailPage.vue'
import ProviderCostDetailPage from '../views/costs/ProviderCostDetailPage.vue'
import RequestLogListPage from '../views/request-logs/RequestLogListPage.vue'
import RequestLogDetailPage from '../views/request-logs/RequestLogDetailPage.vue'
import ProviderListPage from '../views/providers/ProviderListPage.vue'
import ProviderDetailPage from '../views/providers/ProviderDetailPage.vue'
import ModelListPage from '../views/models/ModelListPage.vue'
import ModelDetailPage from '../views/models/ModelDetailPage.vue'
import ApiKeyListPage from '../views/apikeys/ApiKeyListPage.vue'
import OAuthProviderListPage from '../views/oauth/OAuthProviderListPage.vue'
import UserListPage from '../views/users/UserListPage.vue'
import SystemInfoPage from '../views/system/SystemInfoPage.vue'
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
        { path: '', component: DashboardPage, meta: { memberAllowed: true } },
        { path: 'analytics', component: AnalyticsPage, meta: { memberAllowed: true } },
        { path: 'costs', component: CostStatsPage, meta: { memberAllowed: true } },
        { path: 'cost-optimization', component: CostOptimizationPage },
        { path: 'costs/keys/:id(\\d+)', component: KeyCostDetailPage, meta: { memberAllowed: true } },
        // Bare `costs/models` must come before the catch-all so the `?name=`
        // dot-segment fallback still matches here instead of being swallowed
        // by `:name(.*)`.
        { path: 'costs/models', component: ModelCostDetailPage, meta: { memberAllowed: true } },
        { path: 'costs/models/:name(.*)', component: ModelCostDetailPage, meta: { memberAllowed: true } },
        { path: 'costs/providers/:id(\\d+)', component: ProviderCostDetailPage },
        { path: 'request-logs', component: RequestLogListPage },
        { path: 'request-logs/:requestId', component: RequestLogDetailPage },
        { path: 'providers', component: ProviderListPage },
        { path: 'providers/:id', component: ProviderDetailPage },
        { path: 'models', component: ModelListPage },
        { path: 'models/:id', component: ModelDetailPage },
        { path: 'api-keys', component: ApiKeyListPage, meta: { memberAllowed: true } },
        { path: 'oauth-providers', component: OAuthProviderListPage },
        { path: 'users', component: UserListPage },
        { path: 'about', component: SystemInfoPage },
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
