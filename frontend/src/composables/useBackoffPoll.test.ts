import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useBackoffPoll } from './useBackoffPoll'

// The composable registers onUnmounted; outside a component instance Vue only
// warns, which is fine for these unit tests.

describe('useBackoffPoll', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('polls at the base pace and stops on done', async () => {
    const outcomes = ['again', 'again', 'done'] as const
    let calls = 0
    const poll = useBackoffPoll(1000, 8000)
    poll.start(async () => outcomes[calls++] ?? 'done')
    await vi.advanceTimersByTimeAsync(0)
    expect(calls).toBe(1)
    await vi.advanceTimersByTimeAsync(1000)
    expect(calls).toBe(2)
    await vi.advanceTimersByTimeAsync(1000)
    expect(calls).toBe(3)
    // done: no further ticks however long we wait.
    await vi.advanceTimersByTimeAsync(60_000)
    expect(calls).toBe(3)
  })

  it('doubles the delay on errors up to the cap and resets on success', async () => {
    const outcomes = ['error', 'error', 'error', 'again', 'done'] as const
    let calls = 0
    const poll = useBackoffPoll(1000, 3000)
    poll.start(async () => outcomes[calls++] ?? 'done')
    await vi.advanceTimersByTimeAsync(0)
    expect(calls).toBe(1) // errored → next in 2000
    await vi.advanceTimersByTimeAsync(1999)
    expect(calls).toBe(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(calls).toBe(2) // errored → next in 3000 (capped)
    await vi.advanceTimersByTimeAsync(3000)
    expect(calls).toBe(3) // errored → still capped at 3000
    await vi.advanceTimersByTimeAsync(3000)
    expect(calls).toBe(4) // succeeded → pace resets to 1000
    await vi.advanceTimersByTimeAsync(1000)
    expect(calls).toBe(5)
  })

  it('stop invalidates an in-flight tick: its late writes see isCurrent()=false and nothing reschedules', async () => {
    let resolveTick: (v: 'again') => void = () => {}
    let sawCurrent: boolean | null = null
    const poll = useBackoffPoll(1000, 8000)
    poll.start(
      (isCurrent) =>
        new Promise((resolve) => {
          resolveTick = (v) => {
            sawCurrent = isCurrent()
            resolve(v)
          }
        }),
    )
    await vi.advanceTimersByTimeAsync(0)
    poll.stop()
    resolveTick('again')
    await vi.advanceTimersByTimeAsync(0)
    expect(sawCurrent).toBe(false)
    await vi.advanceTimersByTimeAsync(60_000)
    // The superseded tick must not have rescheduled itself.
    expect(vi.getTimerCount()).toBe(0)
  })

  it('start supersedes a previous loop and resets the backoff', async () => {
    let firstCalls = 0
    const poll = useBackoffPoll(1000, 8000)
    poll.start(async () => {
      firstCalls++
      return 'error'
    })
    await vi.advanceTimersByTimeAsync(0)
    expect(firstCalls).toBe(1) // backoff now 2000
    let secondCalls = 0
    poll.start(async () => {
      secondCalls++
      return 'again'
    })
    await vi.advanceTimersByTimeAsync(0)
    expect(secondCalls).toBe(1)
    // The new loop runs at the BASE pace — the old loop's backoff is gone —
    // and the old loop never ticks again.
    await vi.advanceTimersByTimeAsync(1000)
    expect(secondCalls).toBe(2)
    expect(firstCalls).toBe(1)
  })
})
