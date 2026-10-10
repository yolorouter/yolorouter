// frontend/src/api/agentSessions.ts
//
// API client for the tool-session views: one aggregate row per
// agent_session_id plus the whole-session replay transcript. Mirrors the
// backend DTOs in internal/service/requestlog (AgentSessionListItem,
// AgentSessionTranscript and its replay halves) and the query keys parsed
// by internal/handler/agent_session_handler.go (page / page_size /
// agent_client — the same pagination contract every paginated admin
// endpoint uses).
import { apiFetch } from './client'
import type { StatusClass } from './requestLogs'

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

// Mirrors service.AgentSessionReplayAssistant: where one replay block's
// reply came from. Exactly one source applies per block, by backend
// priority: a recorded inline response body (inline), else the sent-SSE
// capture (stream — raw is the capture text, the frontend merges), else the
// assistant copies the NEXT request resends as history (resend — raw is the
// parsed message objects, possibly several), else nothing was recorded
// (none — raw is null and the replay renders an explicit placeholder).
export interface AgentSessionReplayAssistant {
  source: 'inline' | 'stream' | 'resend' | 'none'
  truncated: boolean
  raw: unknown
}

// Mirrors service.AgentSessionReplayBlock: one request's replay turn — the
// row metadata the turn's labels need, the caller-side messages this
// request newly contributed (raw wire objects, assistant and system roles
// already excluded by the backend dedup pass), and the reply attribution.
export interface AgentSessionReplayBlock {
  request_id: string
  created_at: string
  model_name: string
  status_class: StatusClass
  is_stream: boolean
  input_tokens: number
  output_tokens: number
  duration_ms: number
  provider_name: string
  added: unknown[]
  assistant: AgentSessionReplayAssistant
  added_truncated: boolean
}

// Mirrors service.AgentSessionReplaySystem: one entry of the deduplicated
// system-prompt timeline. Raw is polymorphic by dialect — the Anthropic
// top-level system value (string or block array) or the OpenAI dialect's
// role=system message objects.
export interface AgentSessionReplaySystem {
  seen_at_request: string
  truncated: boolean
  raw: unknown
}

// Mirrors service.AgentSessionTranscript: the whole replay in one payload —
// the session-level aggregate (AgentSessionListItem verbatim, so the replay
// header bar and the list row can never drift into two shapes or two
// numbers), the system-prompt timeline, and one block per request in
// chronological order. One fetch renders the entire page.
export interface AgentSessionTranscript {
  session: AgentSessionRow
  system: AgentSessionReplaySystem[]
  blocks: AgentSessionReplayBlock[]
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

// The whole session replay in one request — the aggregate, the system
// timeline, and every turn's blocks. The page mounts, fetches this exactly
// once, and renders; there is deliberately no per-request follow-up fetch.
export function getAgentSessionTranscript(sessionId: string): Promise<AgentSessionTranscript> {
  return apiFetch(`/api/admin/agent-sessions/${encodeURIComponent(sessionId)}/transcript`)
}
