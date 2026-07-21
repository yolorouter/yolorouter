// frontend/src/api/system.ts
//
// API client for the system info + update-check endpoint
// (GET /api/admin/system/version). The response carries both the static
// build/runtime metadata shown on the "System Info" page and the resolved
// update status that drives the sidebar badge. Mirrors the Go struct
// assembled in internal/handler/system_handler.go — when that changes, update
// this interface in the same commit.

import { apiFetch } from './client'

export interface SystemVersion {
  version: string
  commit: string
  build_time: string
  go_version: string
  goos: string
  goarch: string
  db_driver: string
  uptime_seconds: number
  latest: string
  has_update: boolean
  release_url: string
  check_failed: boolean
}

export function getSystemVersion(): Promise<SystemVersion> {
  return apiFetch('/api/admin/system/version')
}
