// Pure form logic for the outbound-proxy settings card and the provider
// edit dialog's proxy override: mode vocabulary, client-side URL
// pre-validation, and the GET→form→PUT projections. Kept out of the
// components so the rules are unit-testable without mounting anything.
//
// The client mirrors only the SYNTAX half of the backend's save validation
// (internal/outboundproxy.ValidateProxyURL): parseable URL, host present,
// scheme in {http, https, socks5, socks5h}. The reserved-range half of the
// rule (multicast/benchmark/reserved/unspecified addresses refused) needs
// address resolution the browser cannot do reliably for the router's
// network position — the server is the authority there and answers with
// errcode 11026, which the components surface verbatim.

export type ProxyMode = 'off' | 'system' | 'custom'

export const PROXY_MODES: ProxyMode[] = ['off', 'system', 'custom']

// The provider-level stance adds one value the global setting does not
// have: '' = inherit the global setting.
export type ProviderProxyMode = '' | 'off' | 'custom'

export const PROVIDER_PROXY_MODES: ProviderProxyMode[] = ['', 'off', 'custom']

/** The transport-supported schemes, socks5h being the accepted alias of socks5. */
const SUPPORTED_SCHEMES = new Set(['http', 'https', 'socks5', 'socks5h'])

/** Why a proxy URL was rejected — selects the localized message. */
export type ProxyUrlIssue = 'empty' | 'unparseable' | 'scheme' | 'noHost'

export type ProxyUrlCheck =
  | { ok: true; url: string }
  | { ok: false; issue: ProxyUrlIssue }

/**
 * Validates one proxy URL input (client-side syntax pre-check; the server
 * re-validates and additionally refuses reserved-range addresses). Returns
 * the trimmed URL on success — the exact string the PUT carries.
 */
export function checkProxyUrl(input: string | null | undefined): ProxyUrlCheck {
  const raw = (input ?? '').trim()
  if (raw === '') return { ok: false, issue: 'empty' }
  let parsed: URL
  try {
    parsed = new URL(raw)
  } catch {
    return { ok: false, issue: 'unparseable' }
  }
  // new URL("proxy.example.com:7890") parses as scheme "proxy.example.com"
  // — a schemeless host:port string the admin clearly intended as a URL.
  // Schemeless inputs are refused with the scheme message, matching the
  // backend (url.Parse yields Scheme "" there).
  if (!SUPPORTED_SCHEMES.has(parsed.protocol.replace(/:$/, ''))) {
    return { ok: false, issue: 'scheme' }
  }
  if (!parsed.hostname) return { ok: false, issue: 'noHost' }
  return { ok: true, url: raw }
}

/**
 * The one place a proxy URL issue becomes an i18n key, shared by the
 * settings card and the provider dialog so a rename can't leave one of
 * them pointing at a message that no longer exists.
 */
export function proxyUrlIssueMessageKey(scope: 'settings' | 'provider', issue: ProxyUrlIssue): string {
  const prefix = scope === 'settings' ? 'generalSettings.outboundProxy' : 'providers'
  switch (issue) {
    case 'empty':
      return `${prefix}.proxyUrlRequired`
    case 'unparseable':
      return `${prefix}.proxyUrlUnparseable`
    case 'scheme':
      return `${prefix}.proxyUrlScheme`
    case 'noHost':
      return `${prefix}.proxyUrlNoHost`
  }
}

/** The settings card's editable form state. */
export interface OutboundProxyForm {
  mode: ProxyMode
  url: string
}

/** Projects the GET payload's snake_case-free row into the editable form state. */
export function outboundProxyFormFromSetting(setting: {
  mode: string
  url: string
}): OutboundProxyForm {
  const mode = PROXY_MODES.includes(setting.mode as ProxyMode) ? (setting.mode as ProxyMode) : 'off'
  return { mode, url: setting.url }
}

export type OutboundProxySavePlan =
  | { ok: true; payload: { mode: ProxyMode; url: string; version: number } }
  | { ok: false; issue: ProxyUrlIssue }

/**
 * Everything onSave needs decided in one call: whether the current form can
 * be PUT, the exact payload if so, or the blocking URL issue if not. A URL
 * is only required under custom mode — off/system legally park an empty
 * URL, and any non-empty URL is syntax-checked whatever the mode so a
 * parked value can never activate unvalidated.
 */
export function outboundProxySavePlan(form: OutboundProxyForm, version: number): OutboundProxySavePlan {
  const trimmed = form.url.trim()
  if (form.mode === 'custom' || trimmed !== '') {
    const check = checkProxyUrl(trimmed)
    if (!check.ok) return { ok: false, issue: check.issue }
    return { ok: true, payload: { mode: form.mode, url: check.url, version } }
  }
  return { ok: true, payload: { mode: form.mode, url: '', version } }
}

/**
 * The provider dialog's save plan for the proxy override pair: mode must be
 * one of inherit/off/custom, custom requires a URL, and any non-empty URL
 * is syntax-checked. Returns the exact proxy_mode/proxy_url pair the PATCH
 * carries.
 */
export type ProviderProxySavePlan =
  | { ok: true; payload: { proxy_mode: ProviderProxyMode; proxy_url: string } }
  | { ok: false; issue: ProxyUrlIssue }

export function providerProxySavePlan(mode: string, url: string): ProviderProxySavePlan {
  const stance = PROVIDER_PROXY_MODES.includes(mode as ProviderProxyMode)
    ? (mode as ProviderProxyMode)
    : null
  if (stance === null) return { ok: false, issue: 'scheme' } // unreachable from a select; keeps the type honest
  const trimmed = url.trim()
  if (stance === 'custom' || trimmed !== '') {
    const check = checkProxyUrl(trimmed)
    if (!check.ok) return { ok: false, issue: check.issue }
    return { ok: true, payload: { proxy_mode: stance, proxy_url: check.url } }
  }
  return { ok: true, payload: { proxy_mode: stance, proxy_url: '' } }
}
