import { apiFetch } from './client'
import type { ModelImpactKey } from './models'
import { BATCH_TEST_BUDGET_MS, SINGLE_PROBE_BUDGET_MS, keyTestBudgetMs } from './probeBudget'
import { verificationDestinationCount } from '../utils/providerProtocol'

// One protocol destination of a key verification run: verifying a key walks
// the provider's primary protocol plus every additional protocol_endpoints
// entry, and this is what each of those probes returned on its own. The
// aggregate columns (last_test_result and friends) keep the worst of them, so
// only the per-target rows can say WHICH protocol failed.
export interface KeyTestTarget {
  // Protocol id (openai / anthropic / gemini / responses). Typed as a plain
  // string, not the ProtocolId union: it is stored upstream data, and a
  // protocol added server-side must render as its raw id rather than break
  // the parse.
  proto: string
  // Backend TestOutcome enum, same values as ProviderKey.last_test_result.
  outcome: number
  duration_ms: number
  // Raw upstream diagnostic (HTTP status + the upstream's own message),
  // truncated server-side. Empty when the probe passed or said nothing.
  detail: string
}

export interface ProviderKey {
  id: number
  label: string
  key_prefix: string
  sort_order: number
  test_model: string
  management_status: number
  verification_status: number
  needs_reentry: boolean
  last_test_result: number | null
  last_test_model: string
  last_test_duration_ms: number | null
  last_tested_at: string | null
  // Per-destination results of the run behind last_test_result. Null for keys
  // last tested before per-target results were recorded (and absent from a
  // server that predates the field), which the UI degrades to the aggregate
  // tag alone.
  last_test_targets?: KeyTestTarget[] | null
}

export interface Provider {
  id: number
  name: string
  provider_type: string
  base_url: string
  note: string
  management_status: number
  running_status: 'not_configured' | 'pending_test' | 'available' | 'partial' | 'unavailable'
  keys: ProviderKey[]
  created_at: string
  protocol_endpoints: string
}

export interface BatchTestResult {
  key_id: number
  label: string
  needs_reentry: boolean
  skipped: boolean
  // The run's budget never reached this key (or cut its probe short). Nothing
  // was tested, so it is not a verdict — a second run resumes from here.
  not_run: boolean
  outcome: number | null
  duration_ms: number
  // This run's own per-destination results, in the key view's shape. Null for
  // an entry that probed nothing (awaiting re-entry, or never reached), so it
  // is the run's breakdown even when the verdict lost its write to a
  // concurrent edit — unlike the key row, which such a run never updated.
  last_test_targets?: KeyTestTarget[] | null
}

export interface CreateProviderInput {
  name: string
  base_url: string
  note?: string
  key_label: string
  key_plaintext: string
  test_model: string
  management_status?: number
  provider_type?: string
  protocol_endpoints?: string
}

export interface UpdateProviderInput {
  name: string
  base_url: string
  note?: string
  provider_type?: string
  protocol_endpoints?: string
}

export interface CreateKeyInput {
  label: string
  plaintext: string
  test_model: string
  management_status?: number
}

export interface UpdateKeyInput {
  label: string
  plaintext?: string
  test_model: string
  management_status?: number
}

export interface TestKeyResult {
  // outcome/duration_ms/detail describe the WORST destination the preview
  // probed, which for a single-destination candidate is simply that one.
  outcome: number
  duration_ms: number
  // detail is the aggregate's own diagnostic (HTTP status + the upstream's
  // error message when present; empty on success). The UI reads diagnostics
  // from last_test_targets — the aggregate is always one of those rows — so
  // this field is kept for API consumers, not rendered separately.
  detail: string
  // Per-destination results of this preview, named as the key view names
  // them so one projection reads both. Optional purely for type safety
  // against an absent field; the bundled server always sends it.
  last_test_targets?: KeyTestTarget[] | null
}

// ModelCatalogueResult is what both catalogue endpoints answer with — the
// stateless preview (by credential) and the by-id read for a stored provider.
// outcome is null only alongside no_usable_key: the provider had no enabled,
// current key, so nothing was ever asked of the upstream and there is no
// verdict to report — not even the enum's zero value, which names success. A
// preview always carries a credential, so it always reports an outcome.
export interface ModelCatalogueResult {
  models: string[]
  outcome: number | null
  // detail is the upstream's own refusal text (HTTP status + message) when the
  // catalogue could not be fetched, shown verbatim so an operator can tell a
  // rejected credential from an endpoint the upstream does not serve. Empty on
  // success, and optional so a response from a server that predates it reads
  // as "no detail" rather than undefined.
  detail?: string
  no_usable_key?: boolean
}

export function listProviders(): Promise<{ list: Provider[] }> {
  return apiFetch('/api/admin/providers')
}

export function getProvider(id: number): Promise<Provider> {
  return apiFetch(`/api/admin/providers/${id}`)
}

// Creating a provider verifies its first key against every destination before
// returning, so this write inherits the same per-destination budget as a key
// test. The destination count comes straight off the input rather than from a
// caller argument — it is exactly what the server will parse out of the same
// two fields.
export function createProvider(input: CreateProviderInput): Promise<Provider> {
  return apiFetch('/api/admin/providers', {
    method: 'POST',
    body: JSON.stringify(input),
    timeoutMs: keyTestBudgetMs(verificationDestinationCount(input.provider_type ?? '', input.protocol_endpoints ?? '')),
  })
}

export function updateProvider(id: number, input: UpdateProviderInput): Promise<Provider> {
  return apiFetch(`/api/admin/providers/${id}`, { method: 'PATCH', body: JSON.stringify(input) })
}

export function setProviderStatus(id: number, enabled: boolean): Promise<void> {
  return apiFetch(`/api/admin/providers/${id}/status`, { method: 'PATCH', body: JSON.stringify({ enabled }) })
}

export interface ProviderImpactModel {
  id: number
  name: string
  /** Disabling the provider leaves this model with no routable candidate at all. */
  no_other_routable_source: boolean
}

export interface ProviderImpact {
  models: ProviderImpactModel[]
  /** Callable keys allowlisting a model this disable would strand; empty when nothing is stranded. */
  affected_keys: ModelImpactKey[]
  /** Callable allow-all keys, counted only when something is stranded. */
  allow_all_key_count: number
}

export function getProviderImpact(id: number): Promise<ProviderImpact> {
  return apiFetch(`/api/admin/providers/${id}/impact`)
}

// testKeyPreview checks a key against every destination the candidate
// provider declares — its primary protocol plus each additional
// protocolEndpoints entry — before the provider exists, so a pass here means
// the create that follows verifies the same destinations and passes too.
// Passing no protocolEndpoints probes the primary protocol alone.
//
// The budget is sized off that same destination count, like createProvider's:
// the server walks them one after another, and a browser waiting only for one
// would abort a two-endpoint preview halfway through.
export function testKeyPreview(
  baseUrl: string,
  apiKey: string,
  model: string,
  providerType: string,
  protocolEndpoints = '',
): Promise<TestKeyResult> {
  return apiFetch('/api/admin/providers/test-key', {
    method: 'POST',
    body: JSON.stringify({
      base_url: baseUrl, api_key: apiKey, model,
      provider_type: providerType, protocol_endpoints: protocolEndpoints,
    }),
    timeoutMs: keyTestBudgetMs(verificationDestinationCount(providerType, protocolEndpoints)),
  })
}

// listModelsPreview fetches the upstream model catalogue for a candidate
// credential so the setup UI can offer a picker instead of a free-text model
// field. Stateless preview — nothing is persisted. A non-success outcome
// (with an optional detail) means the catalogue could not be retrieved (bad
// key, unreachable, or the upstream simply doesn't expose /v1/models), in
// which case the caller falls back to manual model entry.
export function listModelsPreview(
  baseUrl: string,
  apiKey: string,
  providerType: string,
): Promise<ModelCatalogueResult> {
  return apiFetch('/api/admin/providers/list-models', {
    method: 'POST',
    body: JSON.stringify({ base_url: baseUrl, api_key: apiKey, provider_type: providerType }),
    timeoutMs: SINGLE_PROBE_BUDGET_MS,
  })
}

// listModelsForProvider fetches the upstream model catalogue for an already-
// stored provider, using one of its server-side keys — the by-id counterpart
// to listModelsPreview (which needs a plaintext key the candidate UI doesn't
// hold). Any failure returns an empty list plus why it failed; the caller
// names the reason and falls back to manual model entry.
export function listModelsForProvider(id: number): Promise<ModelCatalogueResult> {
  return apiFetch(`/api/admin/providers/${id}/models`, { timeoutMs: SINGLE_PROBE_BUDGET_MS })
}

// Both key writes below verify the new plaintext against every destination
// before returning, so they carry the per-destination budget too. Getting this
// wrong is worse here than on a read: the key row is written first and the
// verdict committed after, so a browser that gives up early reports failure
// for a key that may be moments from passing.
export function createProviderKey(
  providerId: number,
  input: CreateKeyInput,
  destinationCount: number,
): Promise<ProviderKey> {
  return apiFetch(`/api/admin/providers/${providerId}/keys`, {
    method: 'POST',
    body: JSON.stringify(input),
    timeoutMs: keyTestBudgetMs(destinationCount),
  })
}

export function updateProviderKey(
  providerId: number,
  keyId: number,
  input: UpdateKeyInput,
  destinationCount: number,
): Promise<ProviderKey> {
  return apiFetch(`/api/admin/providers/${providerId}/keys/${keyId}`, {
    method: 'PATCH',
    body: JSON.stringify(input),
    timeoutMs: keyTestBudgetMs(destinationCount),
  })
}

export function reorderProviderKey(providerId: number, keyId: number, direction: 'up' | 'down'): Promise<void> {
  return apiFetch(`/api/admin/providers/${providerId}/keys/${keyId}/order`, {
    method: 'PATCH',
    body: JSON.stringify({ direction }),
  })
}

export function setProviderKeyStatus(providerId: number, keyId: number, enabled: boolean): Promise<void> {
  return apiFetch(`/api/admin/providers/${providerId}/keys/${keyId}/status`, {
    method: 'PATCH',
    body: JSON.stringify({ enabled }),
  })
}

// destinationCount is the provider's verification destination count (see
// verificationDestinationCount): the server tests this key against each one in
// turn, so it multiplies the budget. It is not derivable from providerId
// alone here, which is why the caller supplies it.
export function testProviderKey(providerId: number, keyId: number, destinationCount: number): Promise<ProviderKey> {
  return apiFetch(`/api/admin/providers/${providerId}/keys/${keyId}/test`, {
    method: 'POST',
    timeoutMs: keyTestBudgetMs(destinationCount),
  })
}

// Batch test can legitimately exceed apiFetch's default
// 30s timeout — passes timeoutMs (apiFetch itself honors this
// override; see client.ts). An earlier version tried to work around this
// with an extra AbortController instead — that only added a SECOND, later abort signal
// on top of apiFetch's own hardcoded 30s internal timer, which still fired
// first regardless, so slow multi-key batches kept failing at 30s anyway.
//
// The budget is flat rather than scaled by key/destination count because the
// server bounds the run itself; keys it could not reach come back as not_run.
export function testAllProviderKeys(providerId: number): Promise<{ results: BatchTestResult[] }> {
  return apiFetch<{ results: BatchTestResult[] }>(`/api/admin/providers/${providerId}/keys/test-all`, {
    method: 'POST',
    timeoutMs: BATCH_TEST_BUDGET_MS,
  })
}
