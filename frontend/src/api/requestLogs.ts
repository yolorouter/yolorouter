// frontend/src/api/requestLogs.ts
//
// API client for M6.1 request-log list + detail + CSV export per PRD §6.8.
// Mirrors the backend DTOs defined in
// internal/service/request_log_service.go (RequestLogListItem /
// RequestLogDetail) and the AttemptRecord shape in
// internal/gateway/types.go. Filter params match the query keys parsed by
// internal/handler/request_log_handler.go (request_id / api_key_id /
// model_name / provider_id / status / is_stream / start / end).
//
// CSV export bypasses apiFetch: the response is a UTF-8-BOM text/csv stream,
// not the JSON envelope, so the regular envelope parser would reject it.
// The handler also cannot swap to the JSON envelope mid-stream once the BOM
// is on the wire — see ExportRequestLogsCSV in request_log_handler.go.
import { apiFetch } from './client'

// Mirrors repository.StatusSuccess / StatusFailed / StatusPartial /
// StatusCancelled / StatusRejected. The empty-string "all" bucket is a
// UI-only state (no filter sent); the backend's `validStatusClasses` map
// accepts "" but we treat it as "absent" on the client for clarity.
export type StatusClass = 'success' | 'failed' | 'partial' | 'cancelled' | 'rejected'

// Mirrors service.RequestLogListItem. `attempts` is the total count of every
// candidate try (key rotations + candidate failovers combined); the list row
// does not break that down further — see attempts_detail on the detail DTO.
export interface RequestLogRow {
  request_id: string
  api_key_id: number | null
  owner_label: string
  model_name: string
  provider_id: number | null
  provider_name: string
  is_stream: boolean
  status_code: number
  status_class: StatusClass
  input_tokens: number
  output_tokens: number
  cost_cents: number
  cost_known: boolean
  fail_reason: string | null
  attempts: number
  duration_ms: number
  created_at: string
}

export interface RequestLogPage {
  total: number
  page: number
  page_size: number
  list: RequestLogRow[]
}

// Mirrors gateway.AttemptRecord. `outcome` is one of the AttemptOutcome*
// constants in internal/gateway/types.go (success / auth_failed /
// rate_limited / conn_error / server_error / client_error / bad_status) —
// kept as a string here so the frontend's outcome label/colour map is the
// single source of truth for display.
export interface AttemptRecord {
  candidate_id: number
  provider_id: number
  provider_name: string
  provider_model_name: string
  key_id: number
  key_label: string
  status_code: number
  outcome: string
  fail_reason: string
}

export interface RequestLogDetail extends RequestLogRow {
  attempts_detail: AttemptRecord[]
}

export interface RequestLogListParams {
  request_id?: string
  api_key_id?: number
  model_name?: string
  provider_id?: number
  status?: StatusClass
  is_stream?: boolean
  start?: string
  end?: string
  page: number
  page_size: number
}

// buildQuery turns a sparse param object into URLSearchParams, skipping
// undefined / null / empty-string values so absent filters don't end up as
// `?key=` on the wire (the backend's apply*QueryParam helpers treat "" as
// "param absent" anyway, but staying explicit here keeps the request URL
// readable and means a future backend that rejects empty values doesn't
// break the page).
function buildQuery(params: Record<string, string | number | boolean | undefined | null>): URLSearchParams {
  const sp = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === '') continue
    sp.set(k, String(v))
  }
  return sp
}

export function listRequestLogs(filter: RequestLogListParams): Promise<RequestLogPage> {
  const sp = buildQuery({
    page: filter.page,
    page_size: filter.page_size,
    request_id: filter.request_id,
    api_key_id: filter.api_key_id,
    model_name: filter.model_name,
    provider_id: filter.provider_id,
    status: filter.status,
    is_stream: filter.is_stream,
    start: filter.start,
    end: filter.end,
  })
  return apiFetch(`/api/admin/request-logs?${sp.toString()}`)
}

export function getRequestLogDetail(requestId: string): Promise<RequestLogDetail> {
  return apiFetch(`/api/admin/request-logs/${encodeURIComponent(requestId)}`)
}

// exportRequestLogsCSV streams the current filter as a CSV download. Uses a
// transient <a download> element to trigger the browser's "Save As" flow
// rather than window.location.href = url, which would navigate the SPA away
// and lose filter state if the user comes back. RevokeObjectURL cleans up
// the blob URL after the click — without it, every export leaks a Blob in
// memory until the next full page reload.
export async function exportRequestLogsCSV(filter: Omit<RequestLogListParams, 'page' | 'page_size'>): Promise<void> {
  const sp = buildQuery({
    request_id: filter.request_id,
    api_key_id: filter.api_key_id,
    model_name: filter.model_name,
    provider_id: filter.provider_id,
    status: filter.status,
    is_stream: filter.is_stream,
    start: filter.start,
    end: filter.end,
  })
  const url = `/api/admin/request-logs/export?${sp.toString()}`
  let res: Response
  try {
    res = await fetch(url, { credentials: 'include' })
  } catch {
    throw new Error('network error during CSV export')
  }
  if (!res.ok) {
    throw new Error(`CSV export failed: HTTP ${res.status}`)
  }
  const blob = await res.blob()
  // Prefer the server's timestamped filename; fall back to an ISO timestamp
  // so repeated exports in the same second don't silently overwrite each
  // other in the browser's downloads folder.
  const disposition = res.headers.get('Content-Disposition') ?? ''
  const match = /filename="?([^"]+)"?/.exec(disposition)
  const fallback = `request-logs-${new Date().toISOString().replace(/[:.]/g, '-')}.csv`
  const filename = match?.[1] ?? fallback
  const objectUrl = URL.createObjectURL(blob)
  try {
    const a = document.createElement('a')
    a.href = objectUrl
    a.download = filename
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
  } finally {
    URL.revokeObjectURL(objectUrl)
  }
}
