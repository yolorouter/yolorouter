// frontend/src/api/auditLogs.ts
//
// API client for the admin audit-log list + CSV export — the console's
// audit page backing store. Mirrors the backend contract in
// internal/handler/audit_log_handler.go (GET /api/admin/audit-logs and
// GET /api/admin/audit-logs/export): rows serialize as model.AdminAuditLog
// itself, filters are exact literals on the wire (admin_user_id / action /
// resource_type / resource_id / status / start / end), multiple dimensions
// intersect server-side.
//
// CSV export bypasses apiFetch: the response is a UTF-8-BOM text/csv
// stream, not the JSON envelope, so the regular envelope parser would
// reject it — same reasoning as api/requestLogs.ts.
import { apiFetch } from './client'

// Mirrors model.AuditStatusSuccess / AuditStatusDenied / AuditStatusError —
// the only three literals the backend's ValidAuditStatuses allowlist accepts.
export type AuditStatus = 'success' | 'denied' | 'error'

// Mirrors model.AdminAuditLog. admin_username is a capture-time snapshot:
// renaming or deleting the account later never changes what a historical
// row says. detail is the server's JSON *string* (already redacted before
// persist — no plaintext secret can ever appear here); '' when the event
// carried no detail. request_id equals that response's X-Request-Id header
// (a reconciliation key — admin requests never write request_logs).
export interface AuditLogRow {
  id: number
  created_at: string
  admin_user_id: number | null
  admin_username: string
  action: string
  resource_type: string
  resource_id: string
  detail: string
  status: AuditStatus
  error_brief: string
  source_ip: string
  user_agent: string
  request_id: string
}

export interface AuditLogPage {
  total: number
  page: number
  page_size: number
  list: AuditLogRow[]
}

export interface AuditLogListParams {
  page: number
  page_size: number
  /** Exact admin account id; null = no constraint. */
  admin_user_id?: number
  /** Exact action name, e.g. "system_settings.update". */
  action?: string
  /** Exact resource namespace, e.g. "system_setting". */
  resource_type?: string
  /** Exact resource id as the audited request's path carried it. */
  resource_id?: string
  status?: AuditStatus
  /** RFC3339; inclusive lower bound. */
  start?: string
  /** RFC3339; exclusive upper bound. */
  end?: string
}

// buildQuery turns a sparse param object into URLSearchParams, skipping
// undefined / null / empty-string values so absent filters don't end up as
// `?key=` on the wire (the backend treats "" as "param absent" anyway, but
// staying explicit keeps the request URL readable). Same shape as
// api/requestLogs.ts's private helper.
function buildQuery(params: Record<string, string | number | undefined | null>): URLSearchParams {
  const sp = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === '') continue
    sp.set(k, String(v))
  }
  return sp
}

export function listAuditLogs(filter: AuditLogListParams): Promise<AuditLogPage> {
  const sp = buildQuery({
    page: filter.page,
    page_size: filter.page_size,
    admin_user_id: filter.admin_user_id,
    action: filter.action,
    resource_type: filter.resource_type,
    resource_id: filter.resource_id,
    status: filter.status,
    start: filter.start,
    end: filter.end,
  })
  return apiFetch(`/api/admin/audit-logs?${sp.toString()}`)
}

// exportAuditLogsCSV streams every row matching the current filter as a CSV
// download. Uses a transient <a download> element to trigger the browser's
// "Save As" flow rather than window.location.href = url, which would
// navigate the SPA away and lose filter state — same mechanism (and same
// blob-URL cleanup) as api/requestLogs.ts's export.
export async function exportAuditLogsCSV(
  filter: Omit<AuditLogListParams, 'page' | 'page_size'>,
): Promise<void> {
  const sp = buildQuery({
    admin_user_id: filter.admin_user_id,
    action: filter.action,
    resource_type: filter.resource_type,
    resource_id: filter.resource_id,
    status: filter.status,
    start: filter.start,
    end: filter.end,
  })
  const url = `/api/admin/audit-logs/export?${sp.toString()}`
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
  const fallback = `admin-audit-logs-${new Date().toISOString().replace(/[:.]/g, '-')}.csv`
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
