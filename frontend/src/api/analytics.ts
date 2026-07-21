// frontend/src/api/analytics.ts
//
// API client for the dashboard and analytics
// endpoints. All endpoints live under /api/admin/* and return the unified
// `{code,message,data}` envelope — `apiFetch` unwraps `data` already, so the
// return types below describe the `data` payload shape only.
//
// DTOs mirror the Go structs in internal/service/dashboard_service.go and
// internal/service/analytics_service.go — when those change, update the
// matching TS interface here in the same commit.

import { apiFetch } from './client'

// === Dashboard =====================================================

export interface TodayMetrics {
  calls: number
  total_cost_micros: number
  success_rate: number // [0,1] — frontend formats as percentage
  unknown_cost_calls: number
}

export interface TrendPoint {
  date: string // "2006-01-02", localized
  calls: number
  cost_micros: number
}

export interface TopCaller {
  api_key_id: number
  owner_label: string
  calls: number
  cost_micros: number
}

export interface RecentFailure {
  request_id: string
  api_key_id: number | null
  model_name: string
  provider_id: number | null
  status_code: number
  fail_reason: string | null
  is_stream: boolean
  duration_ms: number
  created_at: string // RFC3339
}

export interface UpstreamStatus {
  available_providers: number
  abnormal_keys: number
  unavailable_models: number
}

export interface DashboardData {
  today: TodayMetrics
  trend: TrendPoint[]
  top_callers: TopCaller[]
  recent_failures: RecentFailure[]
  upstream_status: UpstreamStatus
}

export function getDashboard(): Promise<DashboardData> {
  return apiFetch('/api/admin/dashboard')
}

// === Analytics =====================================================

// Dimension vocabulary — mirrors internal/service/analytics_service.go's
// constants. Keep as a string union so a typo at a call site fails typecheck
// instead of sending an unknown value silently.
export type AnalyticsDimension = 'model' | 'provider' | 'time' | 'caller'
export type AnalyticsBucket = 'day' | 'hour'

// Shared filter shape across overview / report / export. Pointer-typed fields
// use null (not undefined) so they survive a JSON.stringify round trip when
// added to URLSearchParams below.
export interface AnalyticsFilter {
  start?: string | null // RFC3339, inclusive
  end?: string | null // RFC3339, exclusive
  api_key_id?: number | null
  model_name?: string | null
  provider_id?: number | null
  // status class: '' (any) | 'success' | 'failed' | 'partial' | 'cancelled' | 'rejected'
  status?: string | null
}

// Overview cards. Mirrors OverviewRow in the Go service
// layer; success_rate is precomputed by the backend (success/ended, where
// "ended" excludes 499 caller-cancels).
export interface OverviewRow {
  total_calls: number
  success_calls: number
  ended_calls: number
  success_rate: number
  unknown_cost_calls: number
  input_tokens: number
  output_tokens: number
  cost_micros: number
}

// Dimension-specific row types. Each is the per-bucket aggregate the
// analytics_service returns; column definitions in AnalyticsPage map onto
// these 1:1.
export interface ModelReportRow {
  model_name: string
  calls: number
  success_calls: number
  ended_calls: number
  success_rate: number
  input_tokens: number
  output_tokens: number
  cache_write_tokens: number
  cache_read_tokens: number
  cost_micros: number
  unknown_cost_calls: number
}

export interface ProviderReportRow {
  provider_id: number | null
  provider_name: string
  calls: number
  success_calls: number
  ended_calls: number
  success_rate: number
  avg_duration_ms: number
  cost_micros: number
  unknown_cost_calls: number
}

export interface CallerReportRow {
  api_key_id: number | null
  owner_label: string
  calls: number
  success_calls: number
  ended_calls: number
  success_rate: number
  input_tokens: number
  output_tokens: number
  cache_write_tokens: number
  cache_read_tokens: number
  cost_micros: number
  unknown_cost_calls: number
}

export interface TimeReportRow {
  bucket: string
  calls: number
  success_calls: number
  ended_calls: number
  success_rate: number
  input_tokens: number
  output_tokens: number
  cache_write_tokens: number
  cache_read_tokens: number
  cost_micros: number
  unknown_cost_calls: number
}

// ReportResult is a tagged union on `dimension`. `rows` is typed `unknown`
// here — AnalyticsPage narrows it via a dimension-to-row-type lookup table,
// which keeps the wire type honest without forcing a generic handshake
// through apiFetch.
export interface ReportResult {
  dimension: AnalyticsDimension
  rows: unknown
}

export function getAnalyticsOverview(bucket: string, filter: AnalyticsFilter): Promise<OverviewRow> {
  // bucket is sent even though overview doesn't bucket itself — the backend
  // uses it to apply the same range cap the report uses, so overview cards
  // match the time-dimension report's window.
  const params = buildAnalyticsQuery(filter)
  if (bucket) params.set('bucket', bucket)
  return apiFetch(`/api/admin/analytics/overview?${params.toString()}`)
}

export function getAnalyticsReport(
  dimension: AnalyticsDimension,
  bucket: AnalyticsBucket,
  filter: AnalyticsFilter,
): Promise<ReportResult> {
  const params = buildAnalyticsQuery(filter)
  params.set('dimension', dimension)
  if (dimension === 'time') params.set('bucket', bucket)
  return apiFetch(`/api/admin/analytics/report?${params.toString()}`)
}

// exportAnalyticsCSV triggers a browser download by navigating to the export
// URL. apiFetch can't carry this — the response is text/csv with a BOM, not
// the JSON envelope — so we hand-craft a same-origin <a download> click. The
// session cookie is sent automatically (it's a same-origin GET).
export function exportAnalyticsCSV(
  dimension: AnalyticsDimension,
  bucket: AnalyticsBucket,
  filter: AnalyticsFilter,
): void {
  const params = buildAnalyticsQuery(filter)
  params.set('dimension', dimension)
  if (dimension === 'time') params.set('bucket', bucket)
  const url = `/api/admin/analytics/export?${params.toString()}`
  const a = document.createElement('a')
  a.href = url
  a.rel = 'noopener'
  // The handler sets Content-Disposition with a timestamped filename; leaving
  // `download` empty lets the browser use that server-side name.
  a.click()
}

// buildAnalyticsQuery turns the sparse filter into URLSearchParams, skipping
// undefined / null / empty-string values so the backend sees only the
// constraints the user actually set (and applies its own defaults for the
// rest, e.g. the 7-day lookback for dimension=time).
export function buildAnalyticsQuery(filter: AnalyticsFilter): URLSearchParams {
  const params = new URLSearchParams()
  if (filter.start) params.set('start', filter.start)
  if (filter.end) params.set('end', filter.end)
  if (filter.api_key_id != null) params.set('api_key_id', String(filter.api_key_id))
  if (filter.model_name) params.set('model_name', filter.model_name)
  if (filter.provider_id != null) params.set('provider_id', String(filter.provider_id))
  if (filter.status) params.set('status', filter.status)
  return params
}
