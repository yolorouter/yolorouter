// @vitest-environment happy-dom
// (playground.ts transitively imports the API client, whose i18n side
// effects touch localStorage at module load — absent in the node env.)
//
// Node-env tests for the aggregate-channel streaming client (acceptance
// case TC-16, fake timers): the client is its own channel, NOT the shared
// 30s-default JSON client — a text run must survive well past 30s, a
// video-level budget is minutes, both the timeout and the caller's
// AbortSignal are injectable, and a disconnect aborts the underlying
// fetch.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { playgroundChannelTimeoutMs, runPlayground, type PlaygroundRunInput } from './playground'
import { NetworkError, RequestAbortedError } from './client'

// A Response whose body never ends, honoring the abort signal the way a
// real socket-backed stream does: when the signal fires, pending reads
// reject. The promise runPlayground returns stays pending until then.
function hangingEventStream(signal?: AbortSignal | null): Response {
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      const onAbort = () => controller.error(new DOMException('The operation was aborted.', 'AbortError'))
      if (signal) {
        if (signal.aborted) onAbort()
        else signal.addEventListener('abort', onAbort, { once: true })
      }
    },
  })
  return new Response(stream, { status: 200, headers: { 'content-type': 'text/event-stream; charset=utf-8' } })
}

const baseInput: PlaygroundRunInput = {
  key_id: 1,
  modality: 'text',
  models: ['m0'],
  messages: [{ role: 'user', content: 'ping' }],
  params: {},
}

beforeEach(() => {
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('playground streaming client timeouts', () => {
  it('a text run does not fall into the shared 30s default', async () => {
    const fetchMock = vi.fn((_path: string, init?: RequestInit) =>
      Promise.resolve(hangingEventStream(init?.signal)),
    )
    vi.stubGlobal('fetch', fetchMock)
    const onEvent = vi.fn()
    const failure = runPlayground(baseInput, onEvent).then(
      () => new Error('resolved'),
      (err) => err,
    )
    // Let the fetch resolve so the signal reaches the stream reader.
    await vi.advanceTimersByTimeAsync(0)

    // 30s (plus change) into a text run: the channel is still alive. This
    // is the regression leg — wiring this channel through the shared
    // client's default would have aborted it here.
    await vi.advanceTimersByTimeAsync(35_000)
    const signal = fetchMock.mock.calls[0][1]!.signal as AbortSignal
    expect(signal.aborted, 'text channel must outlive 30s').toBe(false)

    // At the text modality's own budget the channel gives up.
    await vi.advanceTimersByTimeAsync(playgroundChannelTimeoutMs.text - 35_000 + 1)
    expect(signal.aborted, 'the channel aborts at the text budget').toBe(true)
    await expect(failure).resolves.toBeInstanceOf(NetworkError)
  })

  it('the video-level budget is minutes, not seconds', () => {
    expect(playgroundChannelTimeoutMs.video).toBeGreaterThanOrEqual(60_000)
    // And the reserved video budget is the longest of the table.
    expect(playgroundChannelTimeoutMs.video).toBeGreaterThanOrEqual(playgroundChannelTimeoutMs.text)
  })

  it('an injected timeout overrides the modality default', async () => {
    const fetchMock = vi.fn((_path: string, init?: RequestInit) =>
      Promise.resolve(hangingEventStream(init?.signal)),
    )
    vi.stubGlobal('fetch', fetchMock)
    const failure = runPlayground(baseInput, vi.fn(), { timeoutMs: 1_500 }).then(
      () => new Error('resolved'),
      (err) => err,
    )
    await vi.advanceTimersByTimeAsync(0)

    await vi.advanceTimersByTimeAsync(1_400)
    const signal = fetchMock.mock.calls[0][1]!.signal as AbortSignal
    expect(signal.aborted, 'the injected budget has not fired yet').toBe(false)

    await vi.advanceTimersByTimeAsync(200)
    expect(signal.aborted, 'the injected budget aborts the channel').toBe(true)
    await expect(failure).resolves.toBeInstanceOf(NetworkError)
  })

  it('a caller disconnect aborts the fetch signal and rejects as control flow', async () => {
    const fetchMock = vi.fn((_path: string, init?: RequestInit) =>
      Promise.resolve(hangingEventStream(init?.signal)),
    )
    vi.stubGlobal('fetch', fetchMock)
    const controller = new AbortController()
    const failure = runPlayground(baseInput, vi.fn(), { signal: controller.signal }).then(
      () => new Error('resolved'),
      (err) => err,
    )
    await vi.advanceTimersByTimeAsync(0)

    controller.abort()
    await vi.advanceTimersByTimeAsync(0)
    const signal = fetchMock.mock.calls[0][1]!.signal as AbortSignal
    expect(signal.aborted, 'the disconnect reached the underlying fetch').toBe(true)
    // The disconnect is control flow, not a user-facing failure.
    await expect(failure).resolves.toBeInstanceOf(RequestAbortedError)
  })

  it('events stream out as frames arrive, before the channel closes', async () => {
    // A controlled stream: two frames enqueued, the body held open — the
    // onEvent callback must have fired for both while the promise is still
    // pending. Incremental delivery is the channel's whole value.
    let controller!: ReadableStreamDefaultController<Uint8Array>
    const stream = new ReadableStream<Uint8Array>({
      start(c) {
        controller = c
      },
    })
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response(stream, { status: 200, headers: { 'content-type': 'text/event-stream' } }))),
    )
    const seen: string[] = []
    void runPlayground(baseInput, (ev) => {
      if (ev.type === 'chunk') seen.push(ev.delta)
    }).catch(() => {})
    await vi.advanceTimersByTimeAsync(0)
    controller.enqueue(new TextEncoder().encode('event: chunk\ndata: {"slot":0,"model":"m0","delta":"Hel"}\n\n'))
    await vi.advanceTimersByTimeAsync(0)
    expect(seen).toEqual(['Hel'])
    controller.enqueue(new TextEncoder().encode('event: chunk\ndata: {"slot":0,"model":"m0","delta":"lo"}\n\n'))
    await vi.advanceTimersByTimeAsync(0)
    expect(seen).toEqual(['Hel', 'lo'])
  })
})
