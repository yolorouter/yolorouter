// Node-env unit tests for the aggregate-channel demultiplexer (acceptance
// case TC-21, the pure-function seam): out-of-order chunks, interleaved
// columns, a truncated stream, error and cancellation paths — each folded
// against a hand-built expected per-column state machine.
import { describe, expect, it } from 'vitest'
import { foldEvent, initialColumns, type DemuxColumn } from './playgroundDemux'
import type { PlaygroundStreamEvent } from './playground'

function run(models: string[], events: PlaygroundStreamEvent[]): DemuxColumn[] {
  const columns = initialColumns(models)
  for (const ev of events) foldEvent(columns, ev)
  return columns
}

const done0 = {
  type: 'done',
  slot: 0,
  model: 'm0',
  status: 200,
  request_id: 'req-0',
  usage: { input_tokens: 5, output_tokens: 3 },
  cost_micros: 11,
  cost_known: true,
  duration_ms: 1234,
} as const satisfies PlaygroundStreamEvent

const error1 = {
  type: 'error',
  slot: 1,
  model: 'm1',
  status: 429,
  request_id: 'req-1',
  error: { message: 'You exceeded your current quota', type: 'insufficient_quota' },
} as const satisfies PlaygroundStreamEvent

describe('playgroundDemux', () => {
  it('routes every event to its own slot and keeps per-column order (interleaved stream)', () => {
    // Column 1's chunks interleave with column 0's, out of submission
    // order — each column only ever sees its own deltas.
    const columns = run(['m0', 'm1'], [
      { type: 'chunk', slot: 1, model: 'm1', delta: 'world' },
      { type: 'chunk', slot: 0, model: 'm0', delta: 'hello' },
      { type: 'chunk', slot: 1, model: 'm1', delta: '!' },
      { type: 'chunk', slot: 0, model: 'm0', delta: ' there' },
      { ...done0, duration_ms: 99 },
      {
        type: 'done',
        slot: 1,
        model: 'm1',
        status: 200,
        request_id: 'req-1',
        usage: { input_tokens: 2, output_tokens: 2 },
        cost_micros: 6,
        cost_known: true,
        duration_ms: 2500,
      },
    ])
    expect(columns[0]).toMatchObject({
      model: 'm0',
      state: 'done',
      output: 'hello there',
      requestId: 'req-0',
      durationMs: 99,
      costMicros: 11,
      usage: { input_tokens: 5, output_tokens: 3 },
    })
    expect(columns[1]).toMatchObject({
      model: 'm1',
      state: 'done',
      output: 'world!',
      requestId: 'req-1',
      durationMs: 2500,
    })
  })

  it('moves a column from running to streaming on its first chunk', () => {
    const columns = run(['m0'], [{ type: 'chunk', slot: 0, model: 'm0', delta: 'a' }])
    expect(columns[0].state).toBe('streaming')
    expect(columns[0].output).toBe('a')
  })

  it('drops events for slots outside the run and stray chunks after a terminal event', () => {
    const columns = run(['m0'], [
      done0,
      // The terminal state stands: later strays cannot resurrect it.
      { type: 'chunk', slot: 0, model: 'm0', delta: 'late' },
      { ...error1, slot: 0 },
      // A slot the run never had.
      { type: 'chunk', slot: 7, model: 'ghost', delta: 'x' },
    ])
    expect(columns[0]).toMatchObject({ state: 'done', output: '', requestId: 'req-0' })
    expect(columns).toHaveLength(1)
  })

  it('keeps the native error shape on the failed column only', () => {
    const columns = run(['m0', 'm1', 'm2'], [
      { type: 'chunk', slot: 0, model: 'm0', delta: 'ok' },
      error1,
      done0,
      {
        type: 'done',
        slot: 2,
        model: 'm2',
        status: 200,
        request_id: 'req-2',
        usage: { input_tokens: 1, output_tokens: 1 },
        cost_micros: 3,
        cost_known: true,
        duration_ms: 5,
      },
    ])
    expect(columns[1]).toMatchObject({
      state: 'error',
      errorStatus: 429,
      errorMessage: 'You exceeded your current quota',
      errorType: 'insufficient_quota',
      requestId: 'req-1',
      output: '',
    })
    // The failure did not take the neighbours with it.
    expect(columns[0].state).toBe('done')
    expect(columns[0].output).toBe('ok')
    expect(columns[2].state).toBe('done')
  })

  it('marks a cancelled column and keeps its request id', () => {
    const columns = run(['m0'], [
      { type: 'chunk', slot: 0, model: 'm0', delta: 'partial' },
      { type: 'cancel_ack', slot: 0, model: 'm0', request_id: 'req-x' },
    ])
    expect(columns[0]).toMatchObject({ state: 'cancelled', output: 'partial', requestId: 'req-x' })
  })

  it('invents no terminal state when the stream is cut short', () => {
    // The channel ended before every column answered: the truncated
    // column keeps its last real state rather than a fabricated one.
    const columns = run(['m0', 'm1'], [
      { type: 'chunk', slot: 0, model: 'm0', delta: 'seen' },
      done0,
      // ...and nothing for slot 1, ever.
    ])
    expect(columns[0].state).toBe('done')
    expect(columns[1].state).toBe('running')
    expect(columns[1].output).toBe('')
  })

  it('degrades gracefully over malformed payload fields', () => {
    // parseFrame spreads whatever JSON arrived; a frame with missing or
    // wrongly-typed fields must degrade to defaults, not throw.
    const columns = initialColumns(['m0'])
    foldEvent(columns, { type: 'done', slot: 0, model: 'm0' } as unknown as PlaygroundStreamEvent)
    expect(columns[0]).toMatchObject({ state: 'done', status: 0, requestId: '', costKnown: false, usage: null })
  })

  it('folds a media done event: whole response as object, cut response as string (TC-09/17 image arms)', () => {
    // Whole: the images answer arrives as its own JSON with the per-image
    // count in usage — the column keeps the parsed object and no
    // truncation.
    const whole = run(['img-a', 'img-b'], [
      {
        type: 'done',
        slot: 0,
        model: 'img-a',
        status: 200,
        request_id: 'req-img-a',
        usage: { input_tokens: 4, output_tokens: 0, images: 2 },
        cost_micros: 80000,
        cost_known: true,
        duration_ms: 4321,
        response: { data: [{ url: 'https://img.example/one.png' }, { b64_json: 'QUJD' }] },
      },
      {
        type: 'done',
        slot: 1,
        model: 'img-b',
        status: 200,
        request_id: 'req-img-b',
        usage: { input_tokens: 4, output_tokens: 0, images: 1 },
        cost_micros: 40000,
        cost_known: true,
        duration_ms: 99,
        response_truncated: true,
        response: '{"created":1,"data":[{"b64_json":"AAAA… (cut',
      },
    ] satisfies PlaygroundStreamEvent[],
    )
    expect(whole[0]).toMatchObject({
      state: 'done',
      responseTruncated: false,
      usage: { images: 2 },
    })
    expect(whole[0].response).toEqual({ data: [{ url: 'https://img.example/one.png' }, { b64_json: 'QUJD' }] })
    // Cut: the byte prefix arrives as a plain string and the flag is set —
    // the two shapes are the channel's whole-versus-cut contract, and a
    // consumer never has to guess which one it holds.
    expect(whole[1]).toMatchObject({
      state: 'done',
      responseTruncated: true,
      response: '{"created":1,"data":[{"b64_json":"AAAA… (cut',
      usage: { images: 1 },
    })
  })

  it('degrades a malformed media response field without throwing', () => {
    // A response that is neither object nor string (a corrupted frame)
    // leaves the column responseless rather than broken.
    const columns = initialColumns(['m0'])
    foldEvent(columns, {
      type: 'done',
      slot: 0,
      model: 'm0',
      status: 200,
      request_id: 'r',
      usage: { input_tokens: 0, output_tokens: 0 },
      cost_micros: 0,
      cost_known: false,
      duration_ms: 0,
      response: 42,
    } as unknown as PlaygroundStreamEvent)
    expect(columns[0]).toMatchObject({ state: 'done', response: null, responseTruncated: false })
  })

  it('folds a string response as cut even when the flag is missing', () => {
    // The shape alone is the whole-versus-cut contract: a plain string is
    // always a cut prefix, and the channel never inlines a whole body as
    // a string. A frame that lost its response_truncated flag between the
    // channel and this fold still reads as cut — guessing "whole" would
    // pass a cut body off as complete.
    const columns = initialColumns(['img-a'])
    foldEvent(columns, {
      type: 'done',
      slot: 0,
      model: 'img-a',
      status: 200,
      request_id: 'req-img-a',
      usage: { input_tokens: 2, output_tokens: 0 },
      cost_micros: 9,
      cost_known: true,
      duration_ms: 7,
      response: '{"created":1,"data":[{"b64_json":"AAAA',
    })
    expect(columns[0]).toMatchObject({ state: 'done', responseTruncated: true })
    expect(columns[0].response).toBe('{"created":1,"data":[{"b64_json":"AAAA')
  })

  it('folds a speech done event: character usage and the playback reference land on the column (TC-10 audio arm)', () => {
    const columns = run(['speech-a', 'speech-b'], [
      {
        type: 'done',
        slot: 0,
        model: 'speech-a',
        status: 200,
        request_id: 'req-aud-0',
        usage: { input_tokens: 0, output_tokens: 0, characters: 9 },
        cost_micros: 18,
        cost_known: true,
        duration_ms: 2100,
        audio: { url: '/api/admin/playground/audio/req-aud-0', content_type: 'audio/mpeg', bytes: 2048 },
      },
      // A cut clip carries NO reference at all — a cut clip is not
      // playable and the fold never invents one.
      {
        type: 'done',
        slot: 1,
        model: 'speech-b',
        status: 200,
        request_id: 'req-aud-1',
        usage: { input_tokens: 0, output_tokens: 0, characters: 1024 },
        cost_micros: 2048,
        cost_known: true,
        duration_ms: 9000,
      },
    ])
    expect(columns[0].usage).toMatchObject({ characters: 9 })
    expect(columns[0].audio).toEqual({
      url: '/api/admin/playground/audio/req-aud-0',
      content_type: 'audio/mpeg',
      bytes: 2048,
    })
    expect(columns[1].usage).toMatchObject({ characters: 1024 })
    expect(columns[1].audio).toBeNull()
  })

  it('degrades a malformed audio reference without throwing and without inventing a clip', () => {
    const columns = initialColumns(['speech-a'])
    foldEvent(columns, {
      type: 'done',
      slot: 0,
      model: 'speech-a',
      status: 200,
      request_id: 'req-aud-x',
      usage: { input_tokens: 0, output_tokens: 0, characters: 3 },
      cost_micros: 6,
      cost_known: true,
      duration_ms: 100,
      audio: { content_type: 'audio/mpeg', bytes: 'huge' },
    } as unknown as PlaygroundStreamEvent)
    expect(columns[0]).toMatchObject({ state: 'done' })
    expect(columns[0].audio).toBeNull()
  })
})
