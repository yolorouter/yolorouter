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
})
