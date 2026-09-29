// frontend/src/api/agentSessions.ts
//
// API client for the tool-session views: one aggregate row per
// agent_session_id plus the per-session request timeline. Mirrors the
// backend DTOs in internal/service/requestlog/agent_session_service.go
// (AgentSessionListItem / AgentSessionDetail) and the query keys parsed by
// internal/handler/agent_session_handler.go (page / page_size /
// agent_client — the same pagination contract every paginated admin
// endpoint uses).
import { apiFetch } from './client'
import type { RequestLogRow } from './requestLogs'

// Mirrors service.AgentSessionListItem — the per-session aggregate the
// repository's GROUP BY computed. Field semantics:
//   - success_count is the success bucket of the shared five-class status
//     taxonomy (nothing broader);
//   - known_cost_micros sums cost_micros over the session's cost_known=true
//     rows ONLY; unknown_cost_count says how many rows could not be priced,
//     so the cost column renders the sum plus an "N rows of unknown cost"
//     marker instead of a silently wrong total (the request-log layer's
//     known/unknown doublet);
//   - first_seen_at / last_seen_at are MIN/MAX created_at serialized
//     RFC3339; the micros→major-currency conversion is a display concern
//     and stays in the page.
export interface AgentSessionRow {
  agent_session_id: string
  agent_client: string
  request_count: number
  success_count: number
  input_tokens: number
  output_tokens: number
  known_cost_micros: number
  unknown_cost_count: number
  first_seen_at: string
  last_seen_at: string
}

export interface AgentSessionPage {
  total: number
  page: number
  page_size: number
  list: AgentSessionRow[]
}

// Mirrors service.AgentSessionDetail: the session's identity, its
// attribution (the earliest row's agent_client — one id mixing tools is an
// anomaly attributed to the first row), and EVERY request of the session in
// chronological order. requests reuses RequestLogRow verbatim so the
// timeline renders the same row shape (five-class status_class included)
// the request-log list already shows, and the :requestId jump target needs
// no translation.
export interface AgentSessionDetail {
  agent_session_id: string
  agent_client: string
  requests: RequestLogRow[]
}

export interface AgentSessionListParams {
  /** Normalized calling-tool name (claude-code / codex / …), matched exactly.
   * The value set is the gateway recognizer's client enum — see
   * utils/agentClient.ts, the frontend mirror of that enum. */
  agent_client?: string
  page: number
  page_size: number
}

// buildQuery turns a sparse param object into URLSearchParams, skipping
// undefined / null / empty-string values so an absent filter doesn't end up
// as `?agent_client=` on the wire. The backend distinguishes "param absent"
// (filter off) from "param present but empty" (a real constraint matching
// zero sessions); dropping the empty value here keeps the dropdown's cleared
// state on the filter-off side, which is what the "all tools" placeholder
// promises.
function buildQuery(params: Record<string, string | number | undefined | null>): URLSearchParams {
  const sp = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === '') continue
    sp.set(k, String(v))
  }
  return sp
}

export function listAgentSessions(filter: AgentSessionListParams): Promise<AgentSessionPage> {
  const sp = buildQuery({
    page: filter.page,
    page_size: filter.page_size,
    agent_client: filter.agent_client,
  })
  return apiFetch(`/api/admin/agent-sessions?${sp.toString()}`)
}

export function getAgentSessionDetail(sessionId: string): Promise<AgentSessionDetail> {
  return apiFetch(`/api/admin/agent-sessions/${encodeURIComponent(sessionId)}`)
}
