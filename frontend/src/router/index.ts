import { createRouter, createWebHistory } from 'vue-router'
import DefaultLayout from '../layouts/DefaultLayout.vue'
import SetupPage from '../views/auth/SetupPage.vue'
import LoginPage from '../views/auth/LoginPage.vue'
import DashboardPlaceholder from '../views/DashboardPlaceholder.vue'
import ProviderListPage from '../views/providers/ProviderListPage.vue'
import ProviderDetailPage from '../views/providers/ProviderDetailPage.vue'
import { useAuthStore } from '../store/auth'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/setup', component: SetupPage },
    { path: '/login', component: LoginPage },
    {
      path: '/',
      component: DefaultLayout,
      children: [
        { path: '', component: DashboardPlaceholder },
        { path: 'providers', component: ProviderListPage },
        { path: 'providers/:id', component: ProviderDetailPage },
      ],
    },
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
      // RequireAdminSession middleware still protects the actual data,
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
  return true
})
