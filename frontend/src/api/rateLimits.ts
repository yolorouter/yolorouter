// frontend/src/api/rateLimits.ts
//
// API client for the observed rate limits: what upstream 429s have said
// about each provider key's own limits. Mirrors the Go handlers in
// internal/handler/rate_limit_handler.go — when those change, update
// these interfaces in the same commit.

import { apiFetch } from './client'

export interface ObservedRateLimitRow {
  id: number
  provider_id: number
  provider_name: string
  provider_key_id: number
  key_label: string
  meter: 'requests' | 'tokens'
  limit_value: number | null
  window_seconds: number | null
  last_remaining: number | null
  last_reset_at: string | null
  updated_at: string
}

export function getRateLimits(): Promise<{ list: ObservedRateLimitRow[] }> {
  return apiFetch('/api/admin/rate-limits')
}

// Deleting is the reset gesture: the row goes, and the next 429 against
// that key learns fresh limits.
export function deleteRateLimit(id: number): Promise<null> {
  return apiFetch(`/api/admin/rate-limits/${id}`, { method: 'DELETE' })
}
