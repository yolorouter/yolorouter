// frontend/src/api/playgroundDemux.ts
//
// The aggregate channel's demultiplexer: a pure fold from interleaved
// PlaygroundStreamEvents (any slot, any order) to per-column state
// machines. Pure by design — no DOM, no network, no clock — so the exact
// behaviours the channel promises (slot routing, per-column terminal
// states, tolerance for truncation and stray events) are testable as a
// function of input alone.
import type {
  PlaygroundAudioRef,
  PlaygroundMediaResponse,
  PlaygroundStreamEvent,
  PlaygroundUsage,
  PlaygroundVideoRef,
} from './playground'

/**
 * The lifecycle of one comparison column as the demux tracks it. The one
 * non-obvious member is 'stopped': a video column whose operator stopped
 * WAITING — the task itself keeps running at full cost and settles into
 * the request log later, which is exactly what the column must render
 * instead of a cancelled reading.
 */
export type DemuxColumnState = 'running' | 'streaming' | 'done' | 'error' | 'cancelled' | 'stopped'

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
  /**
   * A media modality's delivered response: the parsed answer JSON when it
   * arrived whole, or the capped byte prefix when the channel cut it.
   * Null on text columns (their text arrived as chunks).
   */
  response: PlaygroundMediaResponse | null
  /** True when `response` is a cut prefix rather than the whole body. */
  responseTruncated: boolean
  /**
   * A speech column's playback reference (the console URL serving the
   * retained clip). Null on every other column, and on a speech column
   * whose clip was cut — a cut clip is not playable and the channel
   * passes nothing off as whole audio.
   */
  audio: PlaygroundAudioRef | null
  /**
   * A finished video column's playback reference (the console URL proxying
   * the clip from the upstream's result URL). Null on every other column
   * and on a video column that never reached its terminal done.
   */
  video: PlaygroundVideoRef | null
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
    response: null,
    responseTruncated: false,
    audio: null,
    video: null,
    errorStatus: 0,
    errorMessage: '',
    errorType: '',
  }))
}

function isTerminal(state: DemuxColumnState): boolean {
  return state === 'done' || state === 'error' || state === 'cancelled' || state === 'stopped'
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
        response?: unknown
        response_truncated?: unknown
        audio?: unknown
        video?: unknown
      }
      col.state = 'done'
      col.status = num(payload.status)
      col.requestId = str(payload.request_id)
      col.durationMs = num(payload.duration_ms)
      col.costMicros = num(payload.cost_micros)
      col.costKnown = payload.cost_known === true
      const usage = payload.usage as Partial<PlaygroundUsage> | undefined
      if (usage && typeof usage === 'object') {
        col.usage = {
          input_tokens: num(usage.input_tokens),
          output_tokens: num(usage.output_tokens),
          images: num(usage.images),
          characters: num(usage.characters),
          seconds: num(usage.seconds),
        }
      }
      // A media answer rides the done event itself: the parsed answer JSON
      // when whole, the capped string prefix when the channel cut it. The
      // string-or-object shape is the event's own contract — a string
      // payload always folds as cut, even when a corrupted frame lost its
      // response_truncated flag (guessing "whole" would pass a cut body
      // off as complete; the channel never inlines a whole body as a
      // string). Anything else (a malformed frame) degrades to "no
      // response" rather than throwing.
      if (typeof payload.response === 'string') {
        col.response = payload.response
        col.responseTruncated = true
      } else if (payload.response && typeof payload.response === 'object') {
        col.response = payload.response as PlaygroundMediaResponse
        col.responseTruncated = payload.response_truncated === true
      }
      // A speech column's playback reference — an object with a URL. A
      // malformed frame degrades to "no reference" (no player) rather
      // than throwing; nothing here ever invents a clip.
      const audio = payload.audio as Partial<PlaygroundAudioRef> | undefined
      if (audio && typeof audio === 'object' && typeof audio.url === 'string' && audio.url !== '') {
        col.audio = {
          url: audio.url,
          content_type: typeof audio.content_type === 'string' ? audio.content_type : '',
          bytes: num(audio.bytes),
        }
      }
      // A finished video column's playback reference — same defensive
      // reading as the audio one: a malformed frame degrades to "no
      // reference" (no player), never a throw.
      const video = payload.video as Partial<PlaygroundVideoRef> | undefined
      if (video && typeof video === 'object' && typeof video.url === 'string' && video.url !== '') {
        col.video = {
          url: video.url,
          content_type: typeof video.content_type === 'string' ? video.content_type : '',
          seconds: num(video.seconds),
        }
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
    case 'submitted': {
      // A video column's submit acknowledgement: the audit id lands while
      // the column keeps its running state — it is what the stop-waiting
      // control addresses, and what the request-log cross-reference shows
      // before any terminal event names it.
      col.requestId = str((ev as { request_id?: unknown }).request_id)
      return columns
    }
    case 'wait_stopped': {
      // The operator stopped waiting on a video column: the column is NOT
      // cancelled (the task keeps running at full cost), and the honest
      // rendering is the stopped-waiting note, which is what the 'stopped'
      // state exists to carry.
      col.state = 'stopped'
      col.requestId = str((ev as { request_id?: unknown }).request_id)
      return columns
    }
  }
  return columns
}
