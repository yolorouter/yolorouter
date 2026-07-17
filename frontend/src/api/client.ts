import { errcodeMessage, t } from '../i18n'

interface Envelope<T> {
  code: number
  message: string
  data?: T
  timestamp: number
}

function isEnvelope(value: unknown): value is Envelope<unknown> {
  if (typeof value !== 'object' || value === null) return false
  const v = value as Record<string, unknown>
  return typeof v.code === 'number' && typeof v.message === 'string' && typeof v.timestamp === 'number'
}

export class APIError extends Error {
  code: number
  constructor(code: number) {
    super(errcodeMessage(code))
    this.code = code
  }
}

/** Thrown for network failures, timeouts, and responses that aren't a recognizable envelope. */
export class NetworkError extends Error {}

/**
 * Thrown when the caller's own AbortSignal fired — distinct from our internal
 * request timeout. This is a control-flow signal (e.g. a component unmounting
 * mid-request), not a user-facing failure — callers are expected to catch and
 * silently discard it, so its message is not localized.
 */
export class RequestAbortedError extends Error {}

// A unique abort reason for our own timeout, so the catch handlers below can
// tell "we gave up waiting" apart from "the caller cancelled this request" by
// comparing reasons directly — not by guessing from the thrown error's name,
// which isn't reliable: fetch() rejects with whatever reason value the
// signal that fired was given, and a caller's own AbortController.abort()
// call may pass any custom reason, not necessarily a DOMException named
// "AbortError".
const TIMEOUT_REASON = Symbol('yolorouter-ce request timeout')

// Body types the browser sets its own Content-Type for (with a boundary, in
// FormData's case, or none at all for raw bytes) — forcing "application/json"
// on these would corrupt the request.
function bodyManagesOwnContentType(body: BodyInit | null | undefined): boolean {
  return (
    body instanceof FormData ||
    body instanceof Blob ||
    body instanceof URLSearchParams ||
    body instanceof ReadableStream ||
    body instanceof ArrayBuffer ||
    ArrayBuffer.isView(body)
  )
}

export async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const timeoutController = new AbortController()
  const timeout = setTimeout(() => timeoutController.abort(TIMEOUT_REASON), 30_000)
  // Combine our own timeout signal with any signal the caller passed in,
  // rather than replacing it — otherwise a caller-provided AbortSignal
  // (e.g. tied to a component unmount) would silently stop working.
  const signal = init?.signal ? AbortSignal.any([init.signal, timeoutController.signal]) : timeoutController.signal

  // The timeout must stay armed for the entire request, including reading
  // the response body below — an earlier version cleared it right after
  // fetch() resolved, so a server that sent headers but then never finished
  // the body would hang res.json() forever with nothing left to time it out.
  try {
    // new Headers(...) normalizes any of the three HeadersInit shapes
    // (a Headers instance, a plain object, or a [key, value][] tuple array)
    // into one case-insensitive container — a plain-object spread breaks for
    // the other two shapes and silently drops headers.
    const headers = new Headers(init?.headers)
    if (init?.body != null && !bodyManagesOwnContentType(init.body) && !headers.has('Content-Type')) {
      headers.set('Content-Type', 'application/json')
    }

    let res: Response
    try {
      res = await fetch(path, {
        ...init,
        credentials: 'include',
        signal,
        headers,
      })
    } catch {
      throw classifyAbort(signal)
    }

    if (res.status === 204) {
      return undefined as unknown as T
    }

    const contentType = res.headers.get('content-type') ?? ''
    if (!contentType.toLowerCase().includes('application/json')) {
      throw new NetworkError(t('unexpectedResponse'))
    }

    let parsed: unknown
    try {
      parsed = await res.json()
    } catch {
      throw classifyAbort(signal, t('unexpectedResponse'))
    }

    if (!isEnvelope(parsed)) {
      throw new NetworkError(t('unexpectedResponse'))
    }

    if (parsed.code !== 0) {
      throw new APIError(parsed.code)
    }
    return parsed.data as T
  } finally {
    clearTimeout(timeout)
  }
}

// classifyAbort turns a caught fetch()/res.json() failure into the right
// exception type. If the combined signal wasn't actually aborted, the
// failure is an unrelated network/parse error, reported via fallbackMessage.
function classifyAbort(signal: AbortSignal, fallbackMessage = t('networkError')): Error {
  if (!signal.aborted) {
    return new NetworkError(fallbackMessage)
  }
  if (signal.reason === TIMEOUT_REASON) {
    return new NetworkError(t('requestTimedOut'))
  }
  return new RequestAbortedError('request aborted by caller')
}
