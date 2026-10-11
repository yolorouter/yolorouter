// Test-environment hygiene shared by every suite: a teardown guard that
// disarms window timers a test leaves behind.
//
// Why it exists — NaiveUI's MessageEnvironment arms one
// window.setTimeout(hide, duration) per toast (default 3000ms) and has no
// unmount-time disarm, so a test that fires message.success/error and
// unmounts right after leaves the timer live. In this repo's happy-dom
// files window === globalThis and window.setTimeout IS Node's timer, which
// happy-dom's own abort() cannot cancel — the bomb survives vitest's
// per-file environment teardown and fires into a window that no longer
// exists, throwing `window is not defined`. Vitest reports that as an
// unhandled error: every test green, exit code 1, intermittent under load.
// The guard tracks every window timer a file's code arms and
// clears whatever is still pending once each test is over — by then the
// page is unmounted and any live timer is a stray.
//
// Scope: only window.setTimeout / window.setInterval are tracked — the
// paths NaiveUI and the composables schedule through. Vitest's own
// machinery (test timeouts, vi.waitFor) uses its own timer imports and is
// untouched; timers that fire inside a test run normally.
import { afterEach } from 'vitest'

type WindowWithTimers = {
  setTimeout: typeof setTimeout
  setInterval: typeof setInterval
}

const scope = globalThis as typeof globalThis & { window?: WindowWithTimers }
const win = scope.window

// Node-environment suites (the pure-function files) have no window and no
// DOM code to disarm; the guard applies wherever one exists.
if (win && typeof win.setTimeout === 'function') {
  const pendingTimeouts = new Set<ReturnType<typeof setTimeout>>()
  const pendingIntervals = new Set<ReturnType<typeof setInterval>>()
  const realSetTimeout = win.setTimeout
  const realSetInterval = win.setInterval

  win.setTimeout = ((handler: (...args: unknown[]) => void, timeout?: number, ...rest: unknown[]) => {
    const id = realSetTimeout((...fired: unknown[]) => {
      pendingTimeouts.delete(id)
      handler(...fired)
    }, timeout, ...rest)
    pendingTimeouts.add(id)
    return id
  }) as typeof setTimeout

  win.setInterval = ((handler: (...args: unknown[]) => void, timeout?: number, ...rest: unknown[]) => {
    const id = realSetInterval((...fired: unknown[]) => handler(...fired), timeout, ...rest)
    pendingIntervals.add(id)
    return id
  }) as typeof setInterval

  afterEach(() => {
    for (const id of pendingTimeouts) clearTimeout(id)
    pendingTimeouts.clear()
    for (const id of pendingIntervals) clearInterval(id)
    pendingIntervals.clear()
  })
}
