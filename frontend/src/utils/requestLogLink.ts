// Builds /request-logs deep-link locations. Every key emitted here is one
// RequestLogListPage ingests on mount (it also accepts request_id and
// is_stream, which no drill-down emits today), so drill-downs — dashboard
// blocks, analytics report rows — go through this one builder instead of
// each hand-rolling the contract.
import type { RouteLocationRaw } from 'vue-router'

export interface RequestLogLinkQuery {
  model_name?: string | null
  api_key_id?: number | null
  /** Account scope active on the source page — carried through so the
   *  drill-down shows exactly the rows the clicked aggregate counted. */
  user_id?: number | null
  provider_id?: number | null
  status?: string | null
  cost_known?: boolean | null
  /** RFC3339, inclusive. */
  start?: string | null
  /** RFC3339, exclusive. */
  end?: string | null
}

export function requestLogLocation(q: RequestLogLinkQuery): RouteLocationRaw {
  const query: Record<string, string> = {}
  if (q.model_name) query.model_name = q.model_name
  if (q.api_key_id != null) query.api_key_id = String(q.api_key_id)
  if (q.user_id != null) query.user_id = String(q.user_id)
  if (q.provider_id != null) query.provider_id = String(q.provider_id)
  if (q.status) query.status = q.status
  if (q.cost_known != null) query.cost_known = String(q.cost_known)
  // Emitted only as a pair: the log page applies a time range only when both
  // bounds are present, so a lone bound would ride the URL and silently do
  // nothing.
  if (q.start && q.end) {
    query.start = q.start
    query.end = q.end
  }
  return { path: '/request-logs', query }
}

// bucketRange turns a time-report bucket label back into the absolute range
// it covers. Day buckets ("YYYY-MM-DD") are grouped in the admin's browser
// timezone — the page sends it with every analytics request — so they are
// re-anchored in LOCAL time here; Date.parse would anchor them in UTC and
// shift the drill-down by the timezone offset. Hour buckets carry their UTC
// offset in the label ("YYYY-MM-DD HH:MM +08:00", disambiguating DST
// fall-back hours), so they parse to an exact instant. Returns null for a
// label this build does not recognize: a dead click is better than a range
// that silently covers the wrong hours.
export function bucketRange(bucket: string): { start: string; end: string } | null {
  const day = /^(\d{4})-(\d{2})-(\d{2})$/.exec(bucket)
  if (day) {
    const start = new Date(Number(day[1]), Number(day[2]) - 1, Number(day[3]))
    const end = new Date(start)
    end.setDate(end.getDate() + 1)
    return { start: start.toISOString(), end: end.toISOString() }
  }
  const hour = /^(\d{4}-\d{2}-\d{2}) (\d{2}:\d{2}) ([+-]\d{2}:\d{2})$/.exec(bucket)
  if (hour) {
    const startMs = Date.parse(`${hour[1]}T${hour[2]}:00${hour[3]}`)
    if (Number.isNaN(startMs)) return null
    return {
      start: new Date(startMs).toISOString(),
      end: new Date(startMs + 60 * 60 * 1000).toISOString(),
    }
  }
  return null
}
