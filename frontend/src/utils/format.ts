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
