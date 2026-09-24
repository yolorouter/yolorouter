import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  createRestartWait,
  RESTART_POLL_INTERVAL_MS,
  RESTART_REASSURANCE_AFTER_MS,
  RESTART_WAIT_TIMEOUT_MS,
} from './restartWait'

// The extracted wait machine, driven exactly like the page runs it in
// production: default pacing, default deadlines, Date.now as the clock.
// vi.useFakeTimers replaces both the timers and Date, so advancing the fake
// clock IS the machine's clock — no real minutes elapse in these tests.

const sameVersion = () => Promise.resolve({ version: 'v0.2.5' })

describe('restartWait machine', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('keeps the production pacing the page always used: 2s polls, 5-minute give-up', () => {
    // The duration half of the pacing contract: the give-up budget stays
    // 5 minutes, and the reassurance line lands at ~60s. Pinned here so a
    // "just tweak the constant" change cannot pass silently.
    expect(RESTART_POLL_INTERVAL_MS).toBe(2_000)
    expect(RESTART_REASSURANCE_AFTER_MS).toBe(60_000)
    expect(RESTART_WAIT_TIMEOUT_MS).toBe(5 * 60_000)
  })

  it('waits silently before 60s, then moves to reassuring', async () => {
    const states: string[] = []
    const wait = createRestartWait({
      before: 'v0.2.5',
      probe: sameVersion,
      onDone: vi.fn(),
      onTimeout: vi.fn(),
      onStateChange: (s) => states.push(s),
    })
    wait.start()
    await vi.advanceTimersByTimeAsync(59_999)
    expect(wait.state, 'one tick short of 60s: still plain waiting').toBe('waiting')
    expect(states).toEqual([])
    await vi.advanceTimersByTimeAsync(1)
    expect(wait.state, '60s of silence: reassurance due').toBe('reassuring')
    expect(states).toEqual(['reassuring'])
  })

  it('keeps polling until exactly the 5-minute budget, then times out', async () => {
    const onDone = vi.fn()
    const onTimeout = vi.fn()
    const wait = createRestartWait({ before: 'v0.2.5', probe: sameVersion, onDone, onTimeout })
    wait.start()
    // 4:58 — one poll short of the deadline: reassuring, not timed out.
    await vi.advanceTimersByTimeAsync(4 * 60_000 + 58_000)
    expect(wait.state).toBe('reassuring')
    expect(onTimeout).not.toHaveBeenCalled()
    // The remaining 2s reach exactly 5:00 → timeout, never a done.
    await vi.advanceTimersByTimeAsync(2_000)
    expect(wait.state).toBe('timeout')
    expect(onTimeout).toHaveBeenCalledTimes(1)
    expect(onDone).not.toHaveBeenCalled()
  })

  it('finishes on the first DIFFERENT version — the criterion the page always used', async () => {
    const onDone = vi.fn()
    let answer = 'v0.2.5'
    const wait = createRestartWait({
      before: 'v0.2.5',
      probe: () => Promise.resolve({ version: answer }),
      onDone,
      onTimeout: vi.fn(),
    })
    wait.start()
    // The same version answers for 70s — polls ran, reassurance passed, but
    // the old version is not a comeback.
    await vi.advanceTimersByTimeAsync(70_000)
    expect(onDone).not.toHaveBeenCalled()
    expect(wait.state).toBe('reassuring')
    // The restarted service answers its new version: done, with that version.
    answer = 'v0.2.6'
    await vi.advanceTimersByTimeAsync(2_000)
    expect(wait.state).toBe('done')
    expect(onDone).toHaveBeenCalledTimes(1)
    expect(onDone).toHaveBeenCalledWith('v0.2.6')
  })

  it('an empty version is not a comeback either (startup race guard kept)', async () => {
    let answer = ''
    const onDone = vi.fn()
    const wait = createRestartWait({
      before: 'v0.2.5',
      probe: () => Promise.resolve({ version: answer }),
      onDone,
      onTimeout: vi.fn(),
    })
    wait.start()
    await vi.advanceTimersByTimeAsync(10_000)
    expect(onDone, 'an empty version string must not finish the wait').not.toHaveBeenCalled()
    answer = 'v0.2.6'
    await vi.advanceTimersByTimeAsync(2_000)
    expect(onDone).toHaveBeenCalledWith('v0.2.6')
  })

  it('rides out probe rejections while the service is down, then finishes on the new version', async () => {
    let down = true
    const onDone = vi.fn()
    const wait = createRestartWait({
      before: 'v0.2.5',
      probe: () => (down ? Promise.reject(new Error('connection refused')) : Promise.resolve({ version: 'v0.2.6' })),
      onDone,
      onTimeout: vi.fn(),
    })
    wait.start()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(wait.state, 'rejections ride out, but the 60s reassurance still lands').toBe('reassuring')
    expect(onDone).not.toHaveBeenCalled()
    down = false
    await vi.advanceTimersByTimeAsync(2_000)
    expect(onDone).toHaveBeenCalledWith('v0.2.6')
  })

  it("aborts silently when a probe error says 'stop' (lapsed session), never claiming a timeout", async () => {
    const onTimeout = vi.fn()
    const wait = createRestartWait({
      before: 'v0.2.5',
      probe: () => Promise.reject(new Error('session invalid')),
      onDone: vi.fn(),
      onTimeout,
      onProbeError: () => 'stop',
    })
    wait.start()
    await vi.advanceTimersByTimeAsync(2_000)
    // Past the whole 5-minute budget: no timeout claim — the session notice
    // owns the UX, a "restart timed out" toast would misreport it.
    await vi.advanceTimersByTimeAsync(5 * 60_000)
    expect(wait.state).toBe('waiting')
    expect(onTimeout).not.toHaveBeenCalled()
  })

  it('stop() cancels the wait: no further probes, no callbacks', async () => {
    const probe = vi.fn(sameVersion)
    const wait = createRestartWait({ before: 'v0.2.5', probe, onDone: vi.fn(), onTimeout: vi.fn() })
    wait.start()
    await vi.advanceTimersByTimeAsync(2_000)
    expect(probe).toHaveBeenCalledTimes(1)
    wait.stop()
    await vi.advanceTimersByTimeAsync(5 * 60_000)
    expect(probe, 'the cancelled loop must not probe again').toHaveBeenCalledTimes(1)
    expect(wait.state).toBe('waiting')
  })

  it('runs on injected durations and an injected clock (the test seams)', async () => {
    // A manual clock the probe itself advances, with budgets far from the
    // defaults: 30ms of timer time reaching "90s" proves the machine follows
    // the injected clock and durations, not Date.now and the constants.
    let clock = 0
    const onTimeout = vi.fn()
    const wait = createRestartWait({
      before: 'a',
      probe: () => {
        clock += 30_000
        return Promise.resolve({ version: 'a' })
      },
      now: () => clock,
      pollIntervalMs: 10,
      reassuranceAfterMs: 60_000,
      timeoutMs: 90_000,
      onDone: vi.fn(),
      onTimeout,
    })
    wait.start()
    await vi.advanceTimersByTimeAsync(10) // probe 1: manual clock 30s
    expect(wait.state).toBe('waiting')
    await vi.advanceTimersByTimeAsync(10) // probe 2: manual clock 60s
    expect(wait.state).toBe('reassuring')
    await vi.advanceTimersByTimeAsync(10) // probe 3: manual clock 90s → budget spent
    expect(wait.state).toBe('timeout')
    expect(onTimeout).toHaveBeenCalledTimes(1)
  })
})
