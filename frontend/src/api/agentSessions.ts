// frontend/src/api/agentSessions.ts
//
// API client for the tool-session views: one aggregate row per
// agent_session_id plus the cursor-paginated replay transcript. Mirrors the
// backend DTOs in internal/service/requestlog (AgentSessionListItem,
// AgentSessionTranscript and its replay halves) and the query keys parsed
// by internal/handler/agent_session_handler.go. Pagination note: the LIST
// endpoint below uses page / page_size / agent_client, the same offset
// contract every other paginated admin endpoint uses — the TRANSCRIPT
// endpoint deliberately deviates from it (block cursors + a server-side
// byte budget), because offset semantics cannot express "continue toward
// older from where I stopped" or "start at this anchor block", both of
// which the replay page needs; see AgentSessionTranscriptParams.
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
// already excluded by the backend dedup pass), the system-prompt value in
// effect at this request (metadata only — the raw text is the per-request
// on-expand fetch below), and the reply attribution.
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
  /** The system timeline entry in effect at this request; absent/null when
   *  no system value had been recorded at or before it (no prompt row). */
  system?: AgentSessionReplaySystemMeta | null
  assistant: AgentSessionReplayAssistant
  added_truncated: boolean
}

// Mirrors service.AgentSessionReplaySystemMeta: the metadata-only half of
// one system-timeline entry. The transcript response deliberately ships NO
// raw prompt text — a long session can hold dozens of kilobytes per
// distinct value — the raw values are fetched from .../transcript/system
// when the collapsed system group is expanded. RawLength is the raw
// value's JSON serialization size in UTF-8 bytes, exactly the bytes the
// entry adds to that on-expand response.
export interface AgentSessionReplaySystemMeta {
  seen_at_request: string
  truncated: boolean
  raw_length: number
}

// Mirrors service.AgentSessionReplaySystem (the .../transcript/system
// entries): one entry of the deduplicated system-prompt timeline with its
// raw value. Raw is polymorphic by dialect — the Anthropic top-level
// system value (string or block array) or the OpenAI dialect's role=system
// message objects. Index-aligned with the transcript's system metadata
// list by backend construction (both come from the same assembly).
export interface AgentSessionReplaySystem {
  seen_at_request: string
  truncated: boolean
  raw: unknown
}

// Mirrors service.AgentSessionTranscript: one cursor-paginated page of the
// replay. Session and system metadata arrive whole on every page (the info
// bar's numbers are session-wide by contract, so they never depend on how
// much is loaded), while blocks is one page carrying the direction-wise
// continuation metadata:
//   - has_more_before / next_before: whether older blocks exist beyond this
//     page, and the upscroll cursor (the page's oldest block id — the
//     server treats it exclusively);
//   - has_more_after / next_after: whether newer blocks exist beyond this
//     page, and the toward-newer cursor — normally the page's newest block
//     id (the server treats it inclusively, so the anchor entry and its
//     continuation share the after parameter), except when a page carries
//     nothing beyond its anchor block, where it names the first block
//     beyond the page so continuing always advances.
export interface AgentSessionTranscript {
  session: AgentSessionRow
  system: AgentSessionReplaySystemMeta[]
  blocks: AgentSessionReplayBlock[]
  has_more_before: boolean
  next_before: string
  has_more_after: boolean
  next_after: string
}

// Mirrors service.AgentSessionTranscriptSystem: the on-expand payload of
// .../transcript/system — the full deduplicated system-prompt timeline
// with its raw values.
export interface AgentSessionTranscriptSystem {
  system: AgentSessionReplaySystem[]
}

// The transcript endpoint's cursor parameters. This paging contract is a
// deliberate deviation from the page/page_size offset envelope the list
// endpoint uses (see the file header): before pages one call exclusively
// toward older, after pages one call inclusively toward newer, and their
// absence asks for the latest page.
export interface AgentSessionTranscriptParams {
  /** Block-count cap. Default 20, max 200, both server-side. */
  limit?: number
  /** Exclusive older-direction cursor: the blocks strictly BEFORE this request id. */
  before?: string
  /** Inclusive toward-newer cursor: the blocks FROM this request id onward. */
  after?: string
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

// buildTranscriptQuery encodes the transcript cursor params, skipping
// absent values so a defaulted call stays a bare transcript URL (no empty
// ?before= or ?limit= on the wire). The params mirror the handler's names
// one-to-one — the table tests pin that pairing, so the client and the
// handler cannot drift apart silently.
export function buildTranscriptQuery(params: AgentSessionTranscriptParams = {}): URLSearchParams {
  return buildQuery({
    limit: params.limit,
    before: params.before,
    after: params.after,
  })
}

// One cursor-paginated page of the replay (see AgentSessionTranscript):
// no cursor params asks for the latest page, which is how the replay page
// opens; before continues toward older; after starts at (or continues
// from) an anchor block. Every page carries the whole-session aggregate
// and the system timeline metadata, so continuation fetches only ever add
// blocks.
export function getAgentSessionTranscript(sessionId: string, params?: AgentSessionTranscriptParams): Promise<AgentSessionTranscript> {
  const qs = buildTranscriptQuery(params).toString()
  const base = `/api/admin/agent-sessions/${encodeURIComponent(sessionId)}/transcript`
  return apiFetch(qs ? `${base}?${qs}` : base)
}

// The system-prompt raw text. Two forms share the endpoint: no params
// fetches the whole deduplicated timeline (the export/back-compat form),
// while { request } fetches the single value that was in effect at that
// request — the per-block expansion fetch. The block's own system metadata
// names the timeline entry the value resolves to, so blocks between two
// changes share one fetch.
export function getAgentSessionTranscriptSystem(
  sessionId: string,
  opts?: { request?: string },
): Promise<AgentSessionTranscriptSystem> {
  const base = `/api/admin/agent-sessions/${encodeURIComponent(sessionId)}/transcript/system`
  const qs = buildQuery({ request: opts?.request }).toString()
  return apiFetch(qs ? `${base}?${qs}` : base)
}
