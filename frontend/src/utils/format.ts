// Shared number / rate formatters used across dashboard, analytics, and cost
// optimization pages. Kept here so every page renders numbers with the same
// locale-aware grouping and the same rate precision.

// formatNumber renders an integer with locale-aware grouping separators
// (e.g. 1234567 -> "1,234,567").
export function formatNumber(n: number): string {
  return n.toLocaleString()
}

// formatRate renders a [0,1] ratio as a percentage string with one decimal
// place (e.g. 0.875 -> "87.5%"). One decimal keeps column width stable.
export function formatRate(r: number): string {
  return `${(r * 100).toFixed(1)}%`
}

// callerDisplay renders a per-key aggregate row's label: the owning
// account's username disambiguated by the key prefix (one account usually
// owns several keys, so the username alone would produce identical rows).
// Returns '' when neither part is known — callers supply their own
// "unknown" fallback text.
export function callerDisplay(username: string, keyPrefix: string): string {
  if (username && keyPrefix) return `${username} (${keyPrefix}…)`
  if (username) return username
  if (keyPrefix) return `${keyPrefix}…`
  return ''
}

// ccsProfileName renders the provider name the CC-Switch import deep link
// carries: the fixed "YoloRouter" brand plus the importing entry's identity
// (a model name on the models page, owner + key id on the API-keys page).
// An empty identity still yields a valid, brand-only name.
export function ccsProfileName(identity?: string): string {
  return `YoloRouter${identity ? ` - ${identity}` : ''}`
}

// Hand-entered yuan amounts (price tables typed by an operator) round to 4
// decimals: that only strips float noise like 0.30000000000000004, never a
// price someone meant. One formatter serves the image and video tables,
// whose inputs are the same hand-typed yuan values.
export function formatYuan(value: number): string {
  return String(Math.round(value * 10000) / 10000)
}

// formatSpan renders a duration that can range from a single fast call to a
// whole working session: sub-second in ms, sub-minute in seconds with two
// decimals, longer as h/m/s with higher zero units dropped ("5m 0s" keeps
// the seconds so a whole-minute span does not read as "5m" of unknown
// precision). Used by the tool-session summary card (session length =
// last minus first request).
export function formatSpan(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  const totalSeconds = ms / 1000
  if (totalSeconds < 60) return `${totalSeconds.toFixed(2)}s`
  const h = Math.floor(totalSeconds / 3600)
  const m = Math.floor((totalSeconds % 3600) / 60)
  const s = Math.floor(totalSeconds % 60)
  if (h > 0) return `${h}h ${m}m ${s}s`
  return `${m}m ${s}s`
}

// formatDuration renders a single request's duration: sub-second in ms,
// anything longer as flat seconds with two decimals ("2730.00s"). It is
// deliberately NOT formatSpan — duration sits in dense table cells, the
// waterfall tooltip, and the request drawer, where a fixed "NN.NNs" shape
// keeps columns aligned instead of switching to h/m/s mid-table. Shared by
// the request-log table cells, the agent-session timeline, and the drawer.
export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(2)}s`
}

// formatShortClock renders a locale-aware timestamp at the table-cell
// granularity — every unit 2-digit ("26/09/28, 08:00:00"-style; the exact
// shape follows the runtime locale). The long variant with a 4-digit year
// stays on the request-log detail page, which is the only consumer of that
// granularity. Accepts anything the Date constructor understands so iso
// strings, epoch milliseconds, and Date objects all share one entry point.
export function formatShortClock(value: string | number | Date): string {
  return new Date(value).toLocaleString(undefined, {
    year: '2-digit',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}

