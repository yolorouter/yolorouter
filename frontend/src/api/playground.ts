// frontend/src/api/playground.ts
//
// The console Playground's console-side API: the two picker lists (plain
// JSON envelope reads) and the run itself — an SSE aggregate channel that
// this module consumes with its own fetch + ReadableStream path rather
// than the shared apiFetch client, because the two clients disagree on
// what a response IS: apiFetch answers one JSON envelope under a 30s
// timeout, while a run answers a long-lived multi-column event stream.
import { apiFetch, APIError, NetworkError } from './client'
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
 * modality tab, so the declaration travels with the name.
 */
export interface PlaygroundModelOption {
  name: string
  output_modalities: string[]
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

/** The run request: whose key to spend, which models to compare, the prompt itself. */
export interface PlaygroundRunInput {
  key_id: number
  modality: 'text'
  models: string[]
  messages: Array<{ role: 'user'; content: string }>
  params: PlaygroundTextParams
}

/** One column's terminal usage reading, exactly as the done event carries it. */
export interface PlaygroundUsage {
  input_tokens: number
  output_tokens: number
}

// The aggregate channel's event family. slot is the column's position in
// the submitted models list; every event names its slot, and this module
// does nothing with that knowledge except hand the event to the caller —
// routing by slot is the page's job.
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
    }
  | {
      type: 'error'
      slot: number
      model: string
      status: number
      request_id: string
      error: unknown
    }

interface EnvelopeShape {
  code: number
  message: string
  data?: unknown
  timestamp: number
}

/**
 * Runs one comparison and streams its events to onEvent as they arrive.
 * Resolves when the channel closes; rejects with an APIError when the
 * endpoint answered a console JSON envelope instead of opening the stream
 * (key ownership/state gates, bind errors) — those carry a real business
 * code the page displays like any other request failure.
 */
export async function runPlayground(
  input: PlaygroundRunInput,
  onEvent: (ev: PlaygroundStreamEvent) => void,
): Promise<void> {
  const res = await fetch('/api/admin/playground/run', {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })

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
    throw new NetworkError(t('networkError'))
  }

  // Incremental read: each column's terminal event is dispatched as it
  // arrives, so the page can render finished columns while later ones are
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
  if (type !== 'chunk' && type !== 'done' && type !== 'error') return null
  return { type, ...payload } as PlaygroundStreamEvent
}
