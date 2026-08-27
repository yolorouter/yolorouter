// Pure, side-effect-free helpers for the provider protocol_endpoints feature.
// Mirrors the backend's own logic (internal/service/provider_protocol.go's
// ValidateProviderType / ValidateProtocolEndpoints) so the form model and the
// wire format ({provider_type, protocol_endpoints}) never drift apart:
// - provider_type is the primary wire protocol a provider speaks (default "openai").
// - protocol_endpoints is a JSON object string of ADDITIONAL protocols the
//   provider also accepts, keyed by protocol id, valued by that protocol's
//   base URL — an empty-string value means "reuse the provider's base_url".
//   An empty overall string means "no additional protocols". The primary
//   protocol is never listed in protocol_endpoints.

export const ALL_PROTOCOLS = ['openai', 'anthropic', 'gemini', 'responses'] as const

export type ProtocolId = (typeof ALL_PROTOCOLS)[number]

export interface ProtocolEndpointEntry {
  enabled: boolean
  url: string
}

export interface ProtocolConfigModel {
  providerType: ProtocolId
  // Entries exist for all 4 protocols, including the primary one — the
  // primary's own entry is simply unused/ignored by the UI and by
  // serializeProtocolConfig (it is always excluded from the output).
  endpoints: Record<ProtocolId, ProtocolEndpointEntry>
}

function isProtocolId(value: string): value is ProtocolId {
  return (ALL_PROTOCOLS as readonly string[]).includes(value)
}

// Names a protocol the way the rest of the provider UI names it, falling back
// to the raw id: a protocol the server supports before this build has a label
// is better shown as "grok" than as a missing-message key. Lives beside
// ALL_PROTOCOLS because that list is what decides whether a label exists.
export function protocolLabel(t: (key: string) => string, proto: string): string {
  return isProtocolId(proto) ? t(`providers.protocol_${proto}`) : proto
}

function normalizeProviderType(providerType: string): ProtocolId {
  return isProtocolId(providerType) ? providerType : 'openai'
}

function emptyEndpoints(): Record<ProtocolId, ProtocolEndpointEntry> {
  return Object.fromEntries(ALL_PROTOCOLS.map((p) => [p, { enabled: false, url: '' }])) as Record<
    ProtocolId,
    ProtocolEndpointEntry
  >
}

export function emptyProtocolConfig(providerType: ProtocolId = 'openai'): ProtocolConfigModel {
  return { providerType, endpoints: emptyEndpoints() }
}

// Tolerant parse: an empty or malformed protocolEndpointsJson yields "no
// additional endpoints" rather than throwing — this mirrors the backend's
// own lenient read-path parsing (SupportedProtocolSet/VerificationTargets),
// since validation already happened once at write time.
export function parseProtocolConfig(providerType: string, protocolEndpointsJson: string): ProtocolConfigModel {
  const model = emptyProtocolConfig(normalizeProviderType(providerType))

  if (!protocolEndpointsJson) return model

  let parsed: unknown
  try {
    parsed = JSON.parse(protocolEndpointsJson)
  } catch {
    return model
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return model

  for (const [key, value] of Object.entries(parsed as Record<string, unknown>)) {
    if (!isProtocolId(key) || key === model.providerType) continue
    if (typeof value !== 'string') continue
    model.endpoints[key] = { enabled: true, url: value }
  }

  return model
}

// verificationDestinationCount mirrors VerificationTargets in
// internal/service/provider_protocol.go: verifying a key hits the primary
// protocol plus every ADDITIONAL protocol declared in protocol_endpoints, one
// after another. Callers need the count because it is the multiplier that
// turns the server's per-call probe budget into a key test's real wall-clock
// cost — a browser budget sized for a single destination aborts a two-endpoint
// provider halfway through.
export function verificationDestinationCount(providerType: string, protocolEndpointsJson: string): number {
  const model = parseProtocolConfig(providerType, protocolEndpointsJson)
  const extras = ALL_PROTOCOLS.filter((p) => p !== model.providerType && model.endpoints[p].enabled)
  return 1 + extras.length
}

// Single-URL predicate mirroring the backend's own ValidateProtocolEndpoints
// (internal/service/provider_protocol.go): an empty string is valid (means
// "reuse the provider's base_url"), otherwise the value must parse as an
// absolute http(s) URL with a non-empty host.
export function isValidEndpointUrl(value: string): boolean {
  if (!value) return true
  let parsed: URL
  try {
    parsed = new URL(value)
  } catch {
    return false
  }
  return (parsed.protocol === 'http:' || parsed.protocol === 'https:') && !!parsed.host
}

// ProtocolConfigFields.vue's endpoint-url n-form-items have a `:rule` but no
// `path`, so they are NOT registered in the parent form's validation map and
// formRef.validate() silently skips them. This re-checks the same rule
// across all enabled additional-protocol endpoints so an invalid URL can
// never reach the backend — used by both provider modals before submit.
export function protocolEndpointsValid(model: ProtocolConfigModel): boolean {
  for (const protocol of ALL_PROTOCOLS) {
    const entry = model.endpoints[protocol]
    if (!entry.enabled || !entry.url) continue
    if (!isValidEndpointUrl(entry.url)) return false
  }
  return true
}

// enabledProtocolEndpoints lists the additional (non-primary) protocols a
// provider also accepts, parsed from its wire fields. Each entry's url may be
// an empty string, meaning "reuse the provider's base_url" — callers apply
// that fallback in whatever form they present it (an address, or a note). This
// is the single walk both the provider list and detail views build on, so the
// "which extra endpoints does this provider serve" rule lives in one place.
export function enabledProtocolEndpoints(
  providerType: string,
  protocolEndpointsJson: string,
): { protocol: ProtocolId; url: string }[] {
  const config = parseProtocolConfig(providerType, protocolEndpointsJson)
  const result: { protocol: ProtocolId; url: string }[] = []
  for (const protocol of ALL_PROTOCOLS) {
    const entry = config.endpoints[protocol]
    if (entry.enabled) result.push({ protocol, url: entry.url })
  }
  return result
}

export function serializeProtocolConfig(model: ProtocolConfigModel): {
  provider_type: string
  protocol_endpoints: string
} {
  const extras: Record<string, string> = {}
  for (const proto of ALL_PROTOCOLS) {
    if (proto === model.providerType) continue
    const entry = model.endpoints[proto]
    if (!entry.enabled) continue
    extras[proto] = entry.url.trim()
  }

  const hasExtras = Object.keys(extras).length > 0
  return {
    provider_type: model.providerType,
    protocol_endpoints: hasExtras ? JSON.stringify(extras) : '',
  }
}
