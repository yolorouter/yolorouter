// frontend/src/api/requestLogs.ts
//
// API client for request-log list + detail + CSV export.
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
  cache_write_tokens: number
  cache_read_tokens: number
  cost_micros: number
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

// Mirrors service.RequestLogDetail's body fields.
// The four *_body strings are the redacted, non-stream bodies embedded
// directly in the detail response; empty string means "not recorded" (early
// failure before body capture, or a stream request where the response body
// lives on disk instead — see has_stream_body / stream_body_path below).
export interface RequestLogDetail extends RequestLogRow {
  attempts_detail: AttemptRecord[]
  // request_headers is the caller's headers as a JSON object string, with
  // sensitive headers masked server-side. Empty = not captured.
  request_headers: string
  request_body: string
  upstream_request_body: string
  response_body: string
  upstream_response_body: string
  stream_body_path: string
  stream_body_truncated: boolean
  has_stream_body: boolean
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

// STREAM_BODY_PREVIEW_CAP bounds how much of a captured stream body this
// page loads into a JS string / the DOM. The backend allows a capture up to
// 1GiB (gateway/stream.go's maxStreamBodyFileBytes) before it even starts
// truncating — buffering that much into one JS string and handing it to
// NCode for syntax highlighting can hang or crash the admin's browser tab.
// handler.GetRequestLogBodyStream serves the file via
// http.ServeContent, which already supports Range requests, so a Range
// header caps what we actually transfer and hold in memory.
export const STREAM_BODY_PREVIEW_CAP = 2 * 1024 * 1024 // 2 MiB

export interface StreamBodyPreview {
  text: string
  truncated: boolean
  rawUrl: string
}

// streamRequestLogBody fetches up to STREAM_BODY_PREVIEW_CAP bytes of the raw
// sent-SSE capture for a streaming request (handler.GetRequestLogBodyStream),
// via a Range request so neither the network transfer nor the in-memory
// string ever exceeds the cap regardless of how large the actual capture is.
// `truncated` is true when the file is larger than what was fetched — the
// caller can offer `rawUrl` as a "view full file" escape hatch that lets the
// browser handle the full download/render itself, entirely outside this
// page's own JS string/DOM. Deliberately bypasses apiFetch: the response is
// raw text, not the JSON envelope — same reasoning as exportRequestLogsCSV
// above. A missing/failed body degrades to an empty, non-truncated result
// rather than throwing, since the detail page already knows from
// has_stream_body whether a body should exist and just shows "not recorded".
export async function streamRequestLogBody(
  requestId: string,
  timeoutMs = 30_000,
): Promise<StreamBodyPreview> {
  const rawUrl = `/api/admin/request-logs/${encodeURIComponent(requestId)}/body/stream`
  // This bypasses apiFetch (raw text, not the JSON envelope), so it must arm
  // its own abort timeout — otherwise a stalled on-disk file serve (slow disk,
  // near-1GiB file, wedged connection) leaves `await res.text()` hanging
  // forever, spinning the detail page's "stream chunks" loader with no way to
  // recover. The signal covers the body read too, not
  // just the initial response.
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const res = await fetch(rawUrl, {
      method: 'GET',
      credentials: 'include',
      headers: { Range: `bytes=0-${STREAM_BODY_PREVIEW_CAP - 1}` },
      signal: controller.signal,
    })
    if (!res.ok) return { text: '', truncated: false, rawUrl }
    const text = await res.text()
    // http.ServeContent replies 206 with Content-Range "bytes 0-N/total" when
    // it honored the Range header and more bytes remain; a 200 (Range ignored
    // or the whole file already fit under the cap) means we got everything.
    let truncated = res.status === 206
    const contentRange = res.headers.get('Content-Range')
    if (truncated && contentRange) {
      const total = Number(contentRange.split('/')[1])
      if (Number.isFinite(total) && total <= STREAM_BODY_PREVIEW_CAP) truncated = false
    }
    return { text, truncated, rawUrl }
  } catch {
    // Timeout (abort), network failure, or a read stall — degrade to an empty,
    // non-truncated result, same as a missing/failed body, so the caller's
    // finally always runs and the loader never hangs.
    return { text: '', truncated: false, rawUrl }
  } finally {
    clearTimeout(timeout)
  }
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
