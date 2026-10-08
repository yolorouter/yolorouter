// frontend/src/api/playgroundDemux.ts
//
// The aggregate channel's demultiplexer: a pure fold from interleaved
// PlaygroundStreamEvents (any slot, any order) to per-column state
// machines. Pure by design — no DOM, no network, no clock — so the exact
// behaviours the channel promises (slot routing, per-column terminal
// states, tolerance for truncation and stray events) are testable as a
// function of input alone.
import type { PlaygroundStreamEvent, PlaygroundUsage } from './playground'

/** The lifecycle of one comparison column as the demux tracks it. */
export type DemuxColumnState = 'running' | 'streaming' | 'done' | 'error' | 'cancelled'

/**
 * One column's current state. `running` is the pre-answer placeholder;
 * `streaming` means at least one chunk arrived; the three terminal states
 * each carry what their event family delivered.
 */
export interface DemuxColumn {
  model: string
  state: DemuxColumnState
  /** The streamed text so far; grows chunk by chunk (the typewriter's source). */
  output: string
  status: number
  requestId: string
  usage: PlaygroundUsage | null
  costMicros: number
  costKnown: boolean
  durationMs: number
  errorStatus: number
  errorMessage: string
  errorType: string
}

/** The columns of a run, in slot order, before any event arrives. */
export function initialColumns(models: string[]): DemuxColumn[] {
  return models.map((model) => ({
    model,
    state: 'running',
    output: '',
    status: 0,
    requestId: '',
    usage: null,
    costMicros: 0,
    costKnown: false,
    durationMs: 0,
    errorStatus: 0,
    errorMessage: '',
    errorType: '',
  }))
}

function isTerminal(state: DemuxColumnState): boolean {
  return state === 'done' || state === 'error' || state === 'cancelled'
}

// Defensive field readers: the channel's payloads come from JSON.parse on
// the wire, so a truncated or malformed frame can leave a field missing or
// in the wrong type. The demux reads what is there and defaults the rest —
// a broken payload degrades one event, never the whole run.
function num(v: unknown, fallback = 0): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : fallback
}
function str(v: unknown): string {
  return typeof v === 'string' ? v : ''
}

/**
 * Folds one aggregate-channel event into its column, in place. The fold's
 * rules are the channel's contract:
 *
 * - Events name their slot; an event for a slot outside the run (or a
 *   second terminal for a column already terminal) is dropped — the first
 *   terminal state stands.
 * - A chunk reaches the column whenever it has not terminated yet and
 *   appends its delta (a chunk after termination is stray by definition).
 * - done settles the column's figures; error keeps the native error
 *   shape; cancel_ack marks the column as stopped from this page.
 *
 * The array itself is mutated (the target column object is the page's
 * reactive state); the function returns the same array for chaining.
 */
export function foldEvent(columns: DemuxColumn[], ev: PlaygroundStreamEvent): DemuxColumn[] {
  const slot = num(ev.slot, -1)
  const col = slot >= 0 && slot < columns.length ? columns[slot] : null
  if (!col) return columns
  if (isTerminal(col.state)) return columns

  switch (ev.type) {
    case 'chunk': {
      const delta = str((ev as { delta?: unknown }).delta)
      if (delta === '') return columns
      col.output += delta
      col.state = 'streaming'
      return columns
    }
    case 'done': {
      const payload = ev as {
        status?: unknown
        request_id?: unknown
        usage?: unknown
        cost_micros?: unknown
        cost_known?: unknown
        duration_ms?: unknown
      }
      col.state = 'done'
      col.status = num(payload.status)
      col.requestId = str(payload.request_id)
      col.durationMs = num(payload.duration_ms)
      col.costMicros = num(payload.cost_micros)
      col.costKnown = payload.cost_known === true
      const usage = payload.usage as Partial<PlaygroundUsage> | undefined
      if (usage && typeof usage === 'object') {
        col.usage = { input_tokens: num(usage.input_tokens), output_tokens: num(usage.output_tokens) }
      }
      return columns
    }
    case 'error': {
      const payload = ev as { status?: unknown; request_id?: unknown; error?: unknown }
      col.state = 'error'
      col.errorStatus = num(payload.status)
      col.requestId = str(payload.request_id)
      // The error object is the relay dialect's native shape; read the two
      // members every OpenAI-shaped dialect carries and leave the rest to
      // the raw payload the run contract documents.
      const err = (payload.error ?? null) as { message?: unknown; type?: unknown } | null
      col.errorMessage = typeof err?.message === 'string' ? err.message : ''
      col.errorType = typeof err?.type === 'string' ? err.type : ''
      return columns
    }
    case 'cancel_ack': {
      col.state = 'cancelled'
      col.requestId = str((ev as { request_id?: unknown }).request_id)
      return columns
    }
  }
  return columns
}
