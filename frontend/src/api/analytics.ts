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

import { apiFetch, browserTimezone } from './client'

// === Dashboard =====================================================

// The four token sums are mutually exclusive buckets: input_tokens is the net
// prompt (cache reads/writes excluded), so the four add up to the true total.
export interface TodayMetrics {
  calls: number
  total_cost_micros: number
  success_rate: number // [0,1] — frontend formats as percentage
  unknown_cost_calls: number
  input_tokens: number
  output_tokens: number
  cache_write_tokens: number
  cache_read_tokens: number
}

export interface TrendPoint {
  date: string // "2006-01-02", localized
  calls: number
  cost_micros: number
}

export interface TopCaller {
  api_key_id: number
  username: string
  key_prefix: string
  calls: number
  cost_micros: number
}

export interface RecentFailure {
  request_id: string
  api_key_id: number | null
  model_name: string
  /** Absent on member responses and on rows whose request never reached a provider. */
  provider_id?: number | null
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

// SetupStatus drives the onboarding funnel banner: raw existence counts for
// the "add provider -> enable a model -> create an API key" sequence. Kept
// separate from UpstreamStatus health signals — these answer "what to set up
// next", not "is it healthy".
export interface SetupStatus {
  providers: number
  enabled_models: number
  api_keys: number
  total_requests: number // lifetime request count — drives the waiting banner (range-independent)
}

export interface DashboardData {
  today: TodayMetrics
  trend: TrendPoint[]
  top_callers: TopCaller[]
  recent_failures: RecentFailure[]
  upstream_status: UpstreamStatus
  setup: SetupStatus
}

export function getDashboard(filter?: { start: string; end: string }, userId?: number | null): Promise<DashboardData> {
  const params = new URLSearchParams()
  if (filter) {
    params.set('start', filter.start)
    params.set('end', filter.end)
  }
  // Admin-only account scope for the traffic-backed sections; the backend
  // pins member sessions to themselves regardless of this parameter.
  if (userId != null) {
    params.set('user_id', String(userId))
  }
  const qs = params.toString()
  return apiFetch(`/api/admin/dashboard${qs ? `?${qs}` : ''}`)
}

// === Analytics =====================================================

// Dimension vocabulary — mirrors internal/service/analytics_service.go's
// constants. Keep as a string union so a typo at a call site fails typecheck
// instead of sending an unknown value silently.
export type AnalyticsDimension = 'model' | 'provider' | 'time' | 'caller' | 'user'
export type AnalyticsBucket = 'day' | 'hour'

// Shared filter shape across overview / report / export. Pointer-typed fields
// use null (not undefined) so they survive a JSON.stringify round trip when
// added to URLSearchParams below.
export interface AnalyticsFilter {
  start?: string | null // RFC3339, inclusive
  end?: string | null // RFC3339, exclusive
  api_key_id?: number | null
  // Narrow every aggregate to one account's rows (key owner).
  user_id?: number | null
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
  // Cache token sums for the same window; input_tokens is the uncached
  // component (the three counts are mutually exclusive), so the
  // token-weighted hit rate is read ÷ (read + write + input).
  cache_write_tokens: number
  cache_read_tokens: number
  cost_micros: number
  // Cache economics for the window. Net cache saving is
  // cache_read_saved_micros − cache_write_extra_micros; both non-negative.
  cache_read_saved_micros: number
  cache_write_extra_micros: number
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
  cache_read_saved_micros: number
  cache_write_extra_micros: number
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
  // The full token block (the provider table renders duration instead of
  // token volume, but its cache columns divide these sums).
  input_tokens: number
  output_tokens: number
  cache_write_tokens: number
  cache_read_tokens: number
  cache_read_saved_micros: number
  cache_write_extra_micros: number
  cost_micros: number
  unknown_cost_calls: number
  /** Provider-to-provider switches charged to the provider that failed. */
  failovers: number
}

// Mirrors repository.UserReportRow (dimension=user): the caller aggregates
// merged per owning account. Current servers drop the NULL user_id group
// (auth-rejected traffic tied to no account), but the field stays nullable:
// the column is, and a rolling upgrade can serve this UI from an older
// binary that still emits that row.
export interface UserReportRow {
  user_id: number | null
  username: string
  calls: number
  success_calls: number
  ended_calls: number
  success_rate: number
  input_tokens: number
  output_tokens: number
  cache_write_tokens: number
  cache_read_tokens: number
  cache_read_saved_micros: number
  cache_write_extra_micros: number
  cost_micros: number
  unknown_cost_calls: number
}

export interface CallerReportRow {
  api_key_id: number | null
  username: string
  key_prefix: string
  calls: number
  success_calls: number
  ended_calls: number
  success_rate: number
  input_tokens: number
  output_tokens: number
  cache_write_tokens: number
  cache_read_tokens: number
  cache_read_saved_micros: number
  cache_write_extra_micros: number
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
  cache_read_saved_micros: number
  cache_write_extra_micros: number
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
  opts?: { withFailovers?: boolean },
): Promise<ReportResult> {
  const params = buildAnalyticsQuery(filter)
  params.set('dimension', dimension)
  if (dimension === 'time') params.set('bucket', bucket)
  // Opt-in: failover counts walk every multi-attempt row server-side, so
  // only the analytics report asks; cost pages reuse this endpoint without.
  if (opts?.withFailovers) params.set('with_failovers', '1')
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
  // Timezone in the query string so CSV export (which bypasses apiFetch via
  // <a>.click() and can't carry the X-Timezone header) still gets the right
  // day grouping. Regular apiFetch calls also send the header (redundant but
  // harmless — middleware checks header first).
  if (browserTimezone) params.set('timezone', browserTimezone)
  if (filter.start) params.set('start', filter.start)
  if (filter.end) params.set('end', filter.end)
  if (filter.api_key_id != null) params.set('api_key_id', String(filter.api_key_id))
  if (filter.user_id != null) params.set('user_id', String(filter.user_id))
  if (filter.model_name) params.set('model_name', filter.model_name)
  if (filter.provider_id != null) params.set('provider_id', String(filter.provider_id))
  if (filter.status) params.set('status', filter.status)
  return params
}

// === Input-compression stats ======================================

// Mirrors internal/service/analytics_service.go's CompressStatsResult. Every
// array field is guaranteed non-null on the wire (backend normalizes nil →
// empty []), so call sites can use .map / .length without null-checking.
export interface CompressTotals {
  total_calls: number
  // Count of rows where compress_skip_reason = '' (compression actually ran).
  compressed_calls: number
  tokens_saved: number
  cost_saved_micros: number
  // SUM(input_tokens) for the same filtered rows, used to display
  // tokens_saved as a percentage of total input volume.
  total_estimated_tokens: number
}

export interface CompressSkipReasonRow {
  // '' means the OK bucket (compression ran); other values are the short
  // stable skip codes (e.g. 'too_small', 'unsupported_mime').
  skip_reason: string
  calls: number
}

export interface CompressTopAPIKeyRow {
  api_key_id: number | null
  username: string
  key_prefix: string
  calls: number
  tokens_saved: number
}

export interface CompressTopModelRow {
  model_name: string
  tokens_saved: number
  cost_saved_micros: number
  compressed_calls: number
  total_calls: number
}

export interface CompressTopProviderRow {
  provider_id: number | null
  provider_name: string
  tokens_saved: number
  cost_saved_micros: number
  compressed_calls: number
  total_calls: number
}

export interface CompressorHitRow {
  // Compressor name (a single value from the comma-joined
  // compressors_applied column).
  name: string
  // Number of requests whose compressors_applied included this compressor.
  // Backed by SQL `SUM(CASE WHEN compressors_applied LIKE '%name%')`, so a
  // request using the same compressor on multiple blocks (e.g. "diff,gotest,diff")
  // counts ONCE — this is request-coverage, not total invocations.
  hits: number
}

export interface CompressDailySeriesRow {
  // "2006-01-02", localized — same format as TimeReportRow.bucket.
  bucket: string
  tokens_saved: number
  cost_saved_micros: number
  compressed_calls: number
}

export interface CompressStatsResult {
  totals: CompressTotals
  skip_reason_breakdown: CompressSkipReasonRow[]
  top_api_keys: CompressTopAPIKeyRow[]
  top_models: CompressTopModelRow[]
  top_providers: CompressTopProviderRow[]
  compressor_hits: CompressorHitRow[]
  daily_series: CompressDailySeriesRow[]
}

// getCompressStats fetches the input-compression roll-up. limit is the
// per-api-key Top-N row count (default 5, capped at 20 by the backend);
// omitted here so the caller can pass undefined and accept the default.
export function getCompressStats(
  filter: AnalyticsFilter,
  limit?: number,
): Promise<CompressStatsResult> {
  const params = buildAnalyticsQuery(filter)
  if (limit != null && limit > 0) params.set('limit', String(limit))
  return apiFetch(`/api/admin/analytics/compress-stats?${params.toString()}`)
}

// === Cache visibility stats =======================================

// Mirrors internal/service/analytics's CacheStatsResult — the verified cache
// economics behind the dashboard's cache KPI cards. Totals cover only
// cache-capable providers' rows (upstream reports cache metering AND a cache
// price is configured); unsupported_providers discloses whose traffic was
// excluded and why. Per-dimension cache figures ride on the report rows
// instead.
export interface CacheTotals {
  cache_read_tokens: number
  cache_write_tokens: number
  // SUM of the persisted NET input counts (cache excluded on every
  // protocol), so hit rate = read / (read + write + uncached) directly.
  uncached_input_tokens: number
  cache_read_saved_micros: number
  cache_write_extra_micros: number
}

export interface CacheUnsupportedProviderRow {
  provider_id: number
  provider_name: string
  reason: 'no_cache_metering' | 'no_cache_price'
}

export interface CacheStatsResult {
  totals: CacheTotals
  unsupported_providers: CacheUnsupportedProviderRow[]
}

// getCacheStats fetches the verified cache-savings roll-up. Shares the
// analytics filter shape; no extra params.
export function getCacheStats(filter: AnalyticsFilter): Promise<CacheStatsResult> {
  const params = buildAnalyticsQuery(filter)
  return apiFetch(`/api/admin/analytics/cache-stats?${params.toString()}`)
}

// === Concise-output projection ====================================

// Mirrors internal/service/analytics's ConciseOutputProjection — the priced
// output-volume roll-up plus the window's projected saved cost and saved
// output tokens behind the cost-optimization page's concise-output card.
export interface ConciseOutputWindow {
  start: string
  end: string
  days: number
}

export interface ConciseOutputProjection {
  window: ConciseOutputWindow
  // SUM(output_tokens × current output_price) over rows whose
  // (model_name, provider_id) resolves to a priced candidate, in micros.
  output_spend_micros: number
  // Rows with output_tokens > 0.
  output_rows: number
  // Output-token total over those rows, priced or not — the coverage
  // denominator. Tokens rather than requests, because that is the basis the
  // rate itself is weighted on.
  output_tokens: number
  // Rows that actually contributed to output_spend_micros. Lower than
  // output_rows means some output traffic was unpriced and is excluded
  // from the figures.
  priced_rows: number
  // Output-token total over the priced rows — the basis of the saved-token
  // figure.
  priced_output_tokens: number
  // The window's priced output spend × coefficient, in micros — the period
  // total the card's cost cell renders.
  projected_saved_cost_micros: number
  // The window's priced output tokens × coefficient — the volume
  // counterpart of the cost figure, on the same priced-only basis.
  projected_saved_output_tokens: number
  // Deprecated: the per-million unit rate this card rendered before the
  // period totals. The backend still emits it for tabs loaded before an
  // upgrade; nothing here reads it anymore.
  projected_savings_per_million_tokens_micros?: number | null
  // The factory coefficient behind the figures, echoed so the UI renders
  // their basis from the backend's single source of truth (the console
  // labels it the savings ratio).
  coefficient: number
}

// getConciseOutputProjection fetches the concise-output card's projection
// for the same filter the rest of the page uses.
export function getConciseOutputProjection(filter: AnalyticsFilter): Promise<ConciseOutputProjection> {
  const params = buildAnalyticsQuery(filter)
  return apiFetch(`/api/admin/analytics/concise-output-projection?${params.toString()}`)
}
