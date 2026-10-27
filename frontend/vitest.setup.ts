// Test-environment hygiene shared by every suite. Three gates:
//
// 1) Window-timer teardown guard — NaiveUI's MessageEnvironment arms one
//    window.setTimeout(hide, duration) per toast (default 3000ms) and has no
//    unmount-time disarm, so a test that fires message.success/error and
//    unmounts right after leaves the timer live. In this repo's happy-dom
//    files window === globalThis and window.setTimeout IS Node's timer, which
//    happy-dom's own abort() cannot cancel — the bomb survives vitest's
//    per-file environment teardown and fires into a window that no longer
//    exists, throwing `window is not defined`. Vitest reports that as an
//    unhandled error: every test green, exit code 1, intermittent under load.
//    The guard tracks every window timer a file's code arms and
//    clears whatever is still pending once each test is over — by then the
//    page is unmounted and any live timer is a stray.
//    Scope: only window.setTimeout / window.setInterval are tracked — the
//    paths NaiveUI and the composables schedule through. Vitest's own
//    machinery (test timeouts, vi.waitFor) uses its own timer imports and is
//    untouched; timers that fire inside a test run normally.
//
// 2) No-network guard — happy-dom's fetch is network-capable and resolves
//    relative URLs against its default page origin (http://localhost:3000),
//    so an endpoint a test forgot to mock sailed past the module mocks into
//    a real connect attempt and came back as an ECONNREFUSED AggregateError
//    printed from the component's catch path: the test stayed green and the
//    leak kept reappearing in every full-suite run. The guard swaps the
//    default fetch for one that rejects AND records the attempt, so a missed
//    mock is a red test naming the URL, not console noise. Tests that stub
//    fetch (vi.stubGlobal / window.fetch = ...) after setup replace the
//    guard wholesale — only untouched, leaking calls ever reach it.
//
// 3) Console-noise seal — the warn families that printed on every run
//    without failing anything: naive-ui popover deprecations (a component
//    still passing a removed prop — note naive-ui emits these through
//    console.ERROR, not warn), vue-router "No match found" (a test's
//    router fixture missing a path the mounted component links to), and
//    Vue's injection "Symbol(router)" not found (a component that calls
//    useRouter() mounted without a router plugin). All three are always a
//    defect, so matching output is recorded and fails the responsible test
//    the same way the fetch guard does; the original message still prints.
import { afterEach, beforeEach } from 'vitest'

type WindowWithTimers = {
  setTimeout: typeof setTimeout
  setInterval: typeof setInterval
}

const scope = globalThis as typeof globalThis & { window?: WindowWithTimers }
const win = scope.window

// ---------------------------------------------------------------------------
// Violation store: filled by the fetch guard and the warn seal during a
// test, drained by the afterEach below so the failure lands on the test
// that caused it.
// ---------------------------------------------------------------------------
const violations: string[] = []

// Captured before the console seal installs: a violation entry quotes the
// matched text, so printing it through the sealed console.error would
// self-match the banned fragment and recurse without bound.
const realConsoleError = console.error.bind(console)

function recordViolation(kind: string, detail: string) {
  const site = new Error().stack?.split('\n').slice(2, 5).join('\n') ?? ''
  const entry = `${kind}: ${detail}\n${site}`
  violations.push(entry)
  // Also print at call time: an attempt fired from a stray timer between
  // tests would otherwise never surface (no afterEach to claim it).
  realConsoleError(`[test hygiene] ${entry}`)
}

beforeEach(() => {
  violations.length = 0
})

afterEach(() => {
  if (violations.length > 0) {
    throw new Error(
      `test-hygiene violations (${violations.length}) — a real network attempt or a banned console.warn; fix the test's mocks/fixture:\n${violations.join('\n---\n')}`,
    )
  }
})

// ---------------------------------------------------------------------------
// No-network guard. Typed loosely on purpose: the Request/URL classes
// differ between the happy-dom and node environments.
// ---------------------------------------------------------------------------
if (typeof globalThis.fetch === 'function') {
  const guardedFetch = (...args: unknown[]) => {
    const input = args[0]
    const url =
      typeof input === 'string'
        ? input
        : input instanceof URL
          ? input.href
          : ((input as { url?: string } | undefined)?.url ??
            (input as { href?: string } | undefined)?.href ??
            String(input))
    recordViolation(
      'real network fetch',
      `${url} — mock the endpoint (vi.mock the api module or stub fetch) instead`,
    )
    return Promise.reject(
      new TypeError(`[test hygiene] real network fetch blocked: ${url}`),
    )
  }
  globalThis.fetch = guardedFetch as typeof globalThis.fetch
  // In the happy-dom files window === globalThis, so this is usually the
  // same assignment twice — kept so a future environment split can't
  // re-expose a live fetch through window.
  if (win) {
    ;(win as unknown as { fetch: typeof globalThis.fetch }).fetch =
      guardedFetch as typeof globalThis.fetch
  }
}

// ---------------------------------------------------------------------------
// Console-noise seal (see header). Both channels: naive-ui prints its
// deprecations via console.error while vue-router and Vue use console.warn.
// ---------------------------------------------------------------------------
const BANNED_CONSOLE_FRAGMENTS = [
  '[naive/popover]',
  'No match found for location with path',
  'injection "Symbol(router)" not found',
] as const

function sealConsole(channel: 'warn' | 'error') {
  const real = console[channel].bind(console)
  console[channel] = (...args: unknown[]) => {
    const text = args.map((a) => (typeof a === 'string' ? a : String(a))).join(' ')
    if (BANNED_CONSOLE_FRAGMENTS.some((f) => text.includes(f))) {
      recordViolation(`banned console.${channel}`, text)
    }
    real(...args)
  }
}
sealConsole('warn')
sealConsole('error')

// ---------------------------------------------------------------------------
// Window-timer teardown guard (happy-dom files only: node suites have no
// window and no DOM code to disarm).
// ---------------------------------------------------------------------------
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
