// frontend/src/api/playground.ts
//
// The console Playground's console-side API: the two picker lists (plain
// JSON envelope reads) and the run itself — an SSE aggregate channel that
// this module consumes with its own fetch + ReadableStream path rather
// than the shared apiFetch client, because the two clients disagree on
// what a response IS: apiFetch answers one JSON envelope under a 30s
// timeout, while a run answers a long-lived multi-column event stream.
import { apiFetch, APIError, NetworkError, RequestAbortedError } from './client'
import { t } from '../i18n'

/** One row of the key picker: an API key of the signed-in account that a run may still spend under. */
export interface PlaygroundKeyOption {
  id: number
  key_prefix: string
  remark: string
}

/**
 * One row of the model picker: a routing name plus the output modalities
 * it declares. The picker filters this list client-side by the active
 * modality tab, so the declaration travels with the name. A speech model
 * additionally carries `voices` — the preset vocabulary its vendors serve
 * (the backend reads it off the speech dialect table), because the voice
 * control is PER COLUMN: a voice is a vendor-private id, and each column's
 * dropdown offers only the voices its own model's vendors know.
 */
export interface PlaygroundModelOption {
  name: string
  output_modalities: string[]
  voices?: string[]
}

// The key list is pinned to the session's own account server-side for both
// roles; there is no caller-side filter to get wrong here.
export function listPlaygroundKeys(): Promise<{ list: PlaygroundKeyOption[] }> {
  return apiFetch('/api/admin/playground/keys')
}

export function listPlaygroundModels(): Promise<{ list: PlaygroundModelOption[] }> {
  return apiFetch('/api/admin/playground/models')
}

/** The sampling parameters a text run exposes. Applied to every column alike. */
export interface PlaygroundTextParams {
  system?: string
  temperature?: number
  max_tokens?: number
}

/** The two request axes the image dialect declares: the size string and the image count. */
export interface PlaygroundImageParams {
  size?: string
  n?: number
}

/** The speech modality's one SHARED axis: the response format. The voice is per column, not a param. */
export interface PlaygroundAudioParams {
  format?: string
}

/** One member of a vision content array: text, or an attached image as a base64 data URL. */
export type PlaygroundContentPart =
  | { type: 'text'; text: string }
  | { type: 'image_url'; image_url: { url: string } }

/** A run's user turn: plain text, or a vision turn carrying the prompt plus attached images. */
export type PlaygroundUserContent = string | PlaygroundContentPart[]

/** The run request: whose key to spend, which models to compare, and the modality-shaped prompt itself. */
export type PlaygroundRunInput =
  | {
      key_id: number
      modality: 'text'
      models: string[]
      messages: Array<{ role: 'user'; content: PlaygroundUserContent }>
      params?: PlaygroundTextParams
    }
  | {
      key_id: number
      modality: 'image'
      models: string[]
      prompt: string
      params?: PlaygroundImageParams
    }
  | {
      /**
       * A speech (text-to-audio) run. `voices` is the run's one per-column
       * parameter — model name → the voice that column speaks in — because
       * a voice is a vendor-private id and one shared value across a
       * mixed-vendor comparison would 400 every foreign column.
       */
      key_id: number
      modality: 'audio'
      models: string[]
      prompt: string
      voices: Record<string, string>
      params?: PlaygroundAudioParams
    }

/** One column's terminal usage reading, exactly as the done event carries it. */
export interface PlaygroundUsage {
  input_tokens: number
  output_tokens: number
  /** How many images an image column delivered; absent on token rows. */
  images?: number
  /** How many characters a speech column settled by, in the vendor's own meter; absent on other rows. */
  characters?: number
}

/** One delivered image of a media response: a remote reference, or inline base64 pixels. */
export interface PlaygroundImageArtifact {
  url?: string
  b64_json?: string
  revised_prompt?: string
}

/**
 * A media modality's delivered response body. Whole answers arrive as the
 * response's own JSON (an images answer: `{ data: [...] }`); an answer the
 * channel's inline cap cut arrives as the capped byte prefix in a plain
 * string, with the event's `response_truncated` flag set — the two shapes
 * are the channel's contract for "whole" versus "cut", never a guess. A
 * body with no honest form under that contract (not JSON at all, or JSON
 * that is not object-shaped — a bare string would be indistinguishable
 * from a cut prefix) is not inlined: the done event carries no response.
 */
export type PlaygroundMediaResponse = { data?: PlaygroundImageArtifact[] } | string

/**
 * A speech column's playback reference: the console URL that serves the
 * retained clip (same session, same origin — a plain relative URL the
 * audio element resolves itself), the content type the exchange
 * announced, and the clip's size. Present only when the whole clip was
 * retained; a cut clip carries no reference at all.
 */
export interface PlaygroundAudioRef {
  url: string
  content_type: string
  bytes: number
}

// The aggregate channel's event family. slot is the column's position in
// the submitted models list; every event names its slot, and this module
// does nothing with that knowledge except hand the event to the caller —
// routing by slot is the demux pure function's job.
export type PlaygroundStreamEvent =
  | { type: 'chunk'; slot: number; model: string; delta: string }
  | {
      type: 'done'
      slot: number
      model: string
      status: number
      request_id: string
      usage: PlaygroundUsage
      cost_micros: number
      cost_known: boolean
      duration_ms: number
      response?: PlaygroundMediaResponse
      response_truncated?: boolean
      audio?: PlaygroundAudioRef
    }
  | {
      type: 'error'
      slot: number
      model: string
      status: number
      request_id: string
      error: unknown
    }
  | { type: 'cancel_ack'; slot: number; model: string; request_id: string }

// The modality-aware channel timeouts. This client deliberately does NOT
// reuse the shared apiFetch client's 30s default: a comparison run holds
// its columns open for as long as the slowest model generates, and cutting
// the channel at 30s would cut columns the gateway is still legitimately
// serving. Text, image and audio get a generous whole-run budget (media
// generation legitimately runs minutes — the images modality's own
// request budget is the same ten minutes); video reserves a minutes-level
// budget ahead of its modality landing (its jobs run for minutes, not
// seconds). Injectable per call via PlaygroundRunOptions.
export const playgroundChannelTimeoutMs: Record<'text' | 'image' | 'audio' | 'video', number> = {
  text: 10 * 60_000,
  image: 10 * 60_000,
  audio: 10 * 60_000,
  video: 30 * 60_000,
}

/** Options for one aggregate-channel run. */
export interface PlaygroundRunOptions {
  /**
   * The caller's own cancellation. Aborting it disconnects the channel —
   * the backend propagates the disconnect into the in-flight columns — and
   * the returned promise rejects with RequestAbortedError.
   */
  signal?: AbortSignal
  /** Overrides the modality's default channel timeout. */
  timeoutMs?: number
}

interface EnvelopeShape {
  code: number
  message: string
  data?: unknown
  timestamp: number
}

// A unique abort reason for this client's own timeout, so the catch below
// can tell "the channel timed out" apart from "the caller disconnected" —
// the same distinction the shared JSON client makes for its requests.
const CHANNEL_TIMEOUT_REASON = Symbol('playground channel timeout')

/**
 * Runs one comparison and streams its events to onEvent as they arrive.
 * Resolves when the channel closes; rejects with an APIError when the
 * endpoint answered a console JSON envelope instead of opening the stream
 * (key ownership/state gates, bind errors) — those carry a real business
 * code the page displays like any other request failure. Rejects with
 * RequestAbortedError when the caller's signal fired, and NetworkError for
 * a timeout or transport failure.
 */
export async function runPlayground(
  input: PlaygroundRunInput,
  onEvent: (ev: PlaygroundStreamEvent) => void,
  options: PlaygroundRunOptions = {},
): Promise<void> {
  // The channel is bounded by the modality-aware timeout, combined with
  // the caller's signal — either one aborts the fetch and with it the
  // body read, which is what actually stops a hung stream.
  const timeoutController = new AbortController()
  const timeoutMs = options.timeoutMs ?? playgroundChannelTimeoutMs[input.modality] ?? playgroundChannelTimeoutMs.text
  const timer = setTimeout(() => timeoutController.abort(CHANNEL_TIMEOUT_REASON), timeoutMs)
  const signal = options.signal
    ? AbortSignal.any([options.signal, timeoutController.signal])
    : timeoutController.signal
  let res: Response
  try {
    res = await fetch('/api/admin/playground/run', {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
      signal,
    })
  } catch (err) {
    throw classifyChannelAbort(signal, err)
  }

  const contentType = res.headers.get('content-type') ?? ''
  if (!contentType.toLowerCase().includes('text/event-stream')) {
    // Every pre-stream failure keeps the console envelope shape. Parse it
    // the way the shared client would so the page renders the same
    // localized business message a form error gets.
    let parsed: EnvelopeShape | null = null
    try {
      parsed = (await res.json()) as EnvelopeShape
    } catch {
      parsed = null
    }
    if (parsed && typeof parsed.code === 'number' && parsed.message !== undefined) {
      if (parsed.code !== 0) throw new APIError(parsed.code, parsed.data, parsed.timestamp)
      return
    }
    throw new NetworkError(t('unexpectedResponse'))
  }
  if (!res.ok || !res.body) {
    throw classifyChannelAbort(signal, new NetworkError(t('networkError')))
  }

  try {
    // Incremental read: each column's events are dispatched as they
    // arrive, so the page can render finished columns while later ones are
    // still running. Buffering the whole body first would serialize the
    // display to the slowest model for no benefit.
    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''
    for (;;) {
      const { done, value } = await reader.read()
      if (value) buffer += decoder.decode(value, { stream: true })
      let sep: number
      // SSE frames are separated by a blank line; a trailing partial frame
      // stays buffered until its terminator arrives.
      while ((sep = buffer.indexOf('\n\n')) >= 0) {
        const frame = buffer.slice(0, sep)
        buffer = buffer.slice(sep + 2)
        const ev = parseFrame(frame)
        if (ev) onEvent(ev)
      }
      if (done) break
    }
    // Flush a trailing frame the stream ended without a blank line after.
    const tail = parseFrame(buffer)
    if (tail) onEvent(tail)
  } catch (err) {
    throw classifyChannelAbort(signal, err)
  } finally {
    clearTimeout(timer)
  }
}

// classifyChannelAbort turns a caught channel failure into the right
// exception type: the caller's own abort is control flow (the page stops
// its columns, it does not toast an error), this client's timeout is a
// network failure, and anything else passes through when the signal was
// never involved.
function classifyChannelAbort(signal: AbortSignal, err: unknown): Error {
  if (!signal.aborted) {
    if (err instanceof Error) return err
    return new NetworkError(t('networkError'))
  }
  if (signal.reason === CHANNEL_TIMEOUT_REASON) {
    return new NetworkError(t('requestTimedOut'))
  }
  return new RequestAbortedError('playground channel aborted by caller')
}

// parseFrame turns one SSE frame into a typed event, or null when the
// frame carries no event name (a keep-alive comment, stray whitespace).
// Unknown event names are dropped rather than thrown at: the channel's
// vocabulary grows with later modalities, and an unrecognized event is
// information this consumer does not need, not a failure.
function parseFrame(frame: string): PlaygroundStreamEvent | null {
  let eventName = ''
  const dataLines: string[] = []
  for (const line of frame.split('\n')) {
    if (line.startsWith('event: ')) eventName = line.slice('event: '.length).trim()
    else if (line.startsWith('data: ')) dataLines.push(line.slice('data: '.length))
  }
  if (!eventName || dataLines.length === 0) return null
  let payload: Record<string, unknown>
  try {
    payload = JSON.parse(dataLines.join('\n')) as Record<string, unknown>
  } catch {
    return null
  }
  const type = eventName as PlaygroundStreamEvent['type']
  if (type !== 'chunk' && type !== 'done' && type !== 'error' && type !== 'cancel_ack') return null
  return { type, ...payload } as PlaygroundStreamEvent
}
