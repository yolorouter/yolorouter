// @vitest-environment happy-dom
//
// Route-table scope conformance: the frontend mirror of the backend's
// member-scope classification (internal/router/member_scope_conformance_test.go).
// The router guard grants a member session exactly the routes flagged
// meta.memberAllowed; every other authenticated route renders only for
// admins. Two invariants, so a new console page cannot silently leak to
// members nor quietly vanish from their surface:
//
//   1. The member surface is a fixed allowlist — a route joins it only by
//      an explicit edit of this test (and a matching backend decision).
//   2. Every admin-only console page the backend classifies as admin-only
//      (request logs, the audit trail, users, providers, models, ...) has
//      NO memberAllowed flag here — audit-logs included.
//
// happy-dom because importing the live router module pulls the i18n
// singleton, whose boot reads localStorage (absent under the node env).
import { describe, expect, it } from 'vitest'
import { router } from './index'

// Layout-child paths (relative, no leading slash) a member session may
// reach. Mirrors the backend's sessionOnly/scoped groups plus the member
// drill-downs they deep-link into. Anything not on this list is admin-only.
const MEMBER_ALLOWED_PATHS = new Set([
  '',
  'analytics',
  'costs',
  'costs/keys/:id(\\d+)',
  'costs/models',
  'costs/models/:name(.*)',
  'api-keys',
])

// Layout-child paths that must STAY admin-only. Mirrors the backend's
// adminOnly list for surfaces the console renders (the backend test covers
// the HTTP endpoints; this one covers the route table a member could try
// to navigate to).
const ADMIN_ONLY_PATHS = [
  'request-logs',
  'request-logs/:requestId',
  'request-messages/:requestId',
  'agent-sessions',
  'agent-sessions/:sessionId',
  'audit-logs',
  'providers',
  'providers/:id',
  'models',
  'models/:id',
  'rate-limits',
  'oauth-providers',
  'users',
  'settings/general',
  'about',
  'cost-optimization',
  'costs/providers/:id(\\d+)',
]

function layoutChildren() {
  const layout = router.options.routes.find((r) => r.path === '/')
  expect(layout?.children, 'authenticated layout with children').toBeTruthy()
  return layout!.children!
}

// The layout's children, found by relative path. A missing route fails
// loudly by name instead of passing vacuously through the catch-all.
function childRoute(path: string) {
  const child = layoutChildren().find((r) => r.path === path)
  expect(child, `route "${path}" registered under the authenticated layout`).toBeTruthy()
  return child!
}

describe('route scope conformance', () => {
  it('the member surface is exactly the fixed allowlist — nothing else carries memberAllowed', () => {
    const flagged = layoutChildren()
      .filter((r) => (r.meta as Record<string, unknown> | undefined)?.memberAllowed)
      .map((r) => r.path)
    expect(new Set(flagged), 'routes flagged memberAllowed').toEqual(MEMBER_ALLOWED_PATHS)
  })

  it.each(ADMIN_ONLY_PATHS)('%s is admin-only (no memberAllowed meta)', (path) => {
    const route = childRoute(path)
    expect((route.meta as Record<string, unknown> | undefined)?.memberAllowed).toBeFalsy()
  })

  it('the audit page route exists with a real component (a missing route would make the above vacuous)', () => {
    const route = childRoute('audit-logs')
    expect(route.component).toBeTruthy()
  })
})
