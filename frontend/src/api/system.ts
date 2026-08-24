// frontend/src/api/system.ts
//
// API client for the system endpoints: version (build/runtime metadata for
// the About page plus the update status behind the sidebar indicator),
// gateway endpoint (the address API clients should point at), and the
// one-click update trigger. Mirrors the Go handlers in
// internal/handler/system_handler.go — when those change, update these
// interfaces in the same commit.

import { apiFetch } from './client'

export interface SystemVersion {
  version: string
  commit: string
  build_time: string
  go_version: string
  goos: string
  goarch: string
  db_driver: string
  update_mode: string
  uptime_seconds: number
  latest: string
  has_update: boolean
  release_url: string
  check_failed: boolean
}

// force=true marks an operator-initiated "check now": the server bypasses
// its result cache so a release published minutes ago is actually seen.
export function getSystemVersion(force = false): Promise<SystemVersion> {
  return apiFetch(force ? '/api/admin/system/version?force=1' : '/api/admin/system/version')
}

export interface SystemEndpoint {
  // The base URL API clients should point at (before any protocol path
  // such as /v1). Resolved server-side: configured server.external_url
  // wins, request-derived otherwise.
  endpoint: string
}

// Separate from getSystemVersion because this one is readable by any
// signed-in account, not just admins.
export function getSystemEndpoint(): Promise<SystemEndpoint> {
  return apiFetch('/api/admin/system/endpoint')
}

export interface SystemUpdateResult {
  status: 'updated' | 'up_to_date'
  target: string
}

// postSystemUpdate triggers the one-click in-place update. The server
// downloads and verifies the release before replying, which can take minutes
// on a slow link — the timeout must comfortably exceed the server's own
// download budget, so the default 30s is overridden here.
export function postSystemUpdate(): Promise<SystemUpdateResult> {
  return apiFetch('/api/admin/system/update', { method: 'POST', timeoutMs: 600_000 })
}
