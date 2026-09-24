// frontend/src/utils/restartWait.ts
//
// The wait-for-restart machine behind the About page's one-click update,
// extracted verbatim from SystemInfoPage's inline waitForRestart loop so the
// wait can be driven by fake clocks in tests. The observable contract of the
// original loop is unchanged: poll the version endpoint every 2s, succeed on
// the first DIFFERENT non-empty version, give up after 5 minutes.
//
//   waiting ──(reassuranceAfterMs elapsed, still the old version)──▶ reassuring
//   waiting | reassuring ──(probe answers a version ≠ before)──▶ done
//   waiting | reassuring ──(timeoutMs elapsed since start)──▶ timeout
//
// The machine owns only pacing and the two deadlines. Everything else — what
// "done" or "timeout" means on screen, what a probe is, and what the clock
// says — is injected, so tests drive it with vi.useFakeTimers and canned
// probe answers instead of real minutes.

export type RestartWaitState = 'waiting' | 'reassuring' | 'done' | 'timeout'

// Pacing and budgets, unchanged from the pre-extraction inline loop (poll
// every 2s; the 5-minute give-up budget predates this module). The
// reassurance line is new: a large database's startup migrations can take
// minutes, so at ~60s of silence the machine moves to 'reassuring' and the
// modal adds one "this is normal" line instead of spinning wordlessly.
export const RESTART_POLL_INTERVAL_MS = 2_000
export const RESTART_REASSURANCE_AFTER_MS = 60_000
export const RESTART_WAIT_TIMEOUT_MS = 5 * 60_000

export interface RestartWaitOptions {
  // The version the update started from. Any probe answering a different
  // non-empty version means the restarted service is back — the same
  // criterion the inline loop always used.
  before: string
  // One "who answers now" probe; may reject while the service is down.
  probe: () => Promise<{ version: string }>
  onDone: (version: string) => void
  onTimeout: () => void
  // Fired on every state transition (reassuring / done / timeout) after
  // start(); the page uses it to surface the 60s reassurance line.
  onStateChange?: (state: RestartWaitState) => void
  // Called on every probe rejection. 'stop' aborts the wait silently (a
  // lapsed session must reauth, not read as a restart timeout); 'continue'
  // keeps polling until the deadline, matching the inline loop's swallow.
  onProbeError?: (err: unknown) => 'stop' | 'continue'
  // Injectable clock and durations — the test seams. Defaults preserve the
  // production pacing above.
  now?: () => number
  pollIntervalMs?: number
  reassuranceAfterMs?: number
  timeoutMs?: number
}

export interface RestartWaitHandle {
  readonly state: RestartWaitState
  start(): void
  stop(): void
}

export function createRestartWait(opts: RestartWaitOptions): RestartWaitHandle {
  const now = opts.now ?? (() => Date.now())
  const pollIntervalMs = opts.pollIntervalMs ?? RESTART_POLL_INTERVAL_MS
  const reassuranceAfterMs = opts.reassuranceAfterMs ?? RESTART_REASSURANCE_AFTER_MS
  const timeoutMs = opts.timeoutMs ?? RESTART_WAIT_TIMEOUT_MS

  // Generation guard (same device as useBackoffPoll): stop() invalidates any
  // in-flight poll — after it, the loop neither probes again nor fires
  // callbacks, so an unmounted page never toasts or writes state.
  let generation = 0
  let state: RestartWaitState = 'waiting'

  const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms))

  function transition(next: RestartWaitState) {
    state = next
    opts.onStateChange?.(next)
  }

  async function run() {
    const gen = generation
    const isCurrent = () => gen === generation
    const deadline = now() + timeoutMs
    const reassureAt = now() + reassuranceAfterMs

    while (isCurrent() && now() < deadline) {
      await sleep(pollIntervalMs)
      if (!isCurrent()) return
      try {
        const info = await opts.probe()
        if (!isCurrent()) return
        if (info.version && info.version !== opts.before) {
          transition('done')
          opts.onDone(info.version)
          return
        }
      } catch (err) {
        if (!isCurrent()) return
        if (opts.onProbeError?.(err) === 'stop') return
        // Connection refused / timeout while the service restarts — keep
        // polling until the deadline.
      }
      if (state === 'waiting' && now() >= reassureAt) transition('reassuring')
    }
    if (!isCurrent()) return
    transition('timeout')
    opts.onTimeout()
  }

  return {
    get state() {
      return state
    },
    start() {
      generation++
      state = 'waiting'
      void run()
    },
    stop() {
      generation++
    },
  }
}
