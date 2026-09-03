import { apiFetch } from './client'
import { CANDIDATE_PROBE_BUDGET_MS } from './probeBudget'

export interface ModelCandidate {
  id: number
  provider_id: number
  provider_name: string
  provider_model_name: string
  input_price: number
  output_price: number
  cache_write_price: number | null
  cache_read_price: number | null
  max_output: number
  /** What settlement prices this candidate in: "token" (default), "image", or "video". */
  billing_mode: BillingMode
  /** The per-image price table, null when the candidate does not bill per image. */
  image_pricing_tiers: ImagePricingTiers | null
  /** The per-second video price table, null when the candidate does not bill per video. */
  video_pricing_tiers: VideoPricingTiers | null
  // Whether the last probe confirmed the capability: true when it did, null when
  // it did not. Informational only — routing ignores these. A false can still
  // arrive from a row written by an older build.
  supports_streaming: boolean | null
  supports_function_calling: boolean | null
  management_status: number
  sort_order: number
  verification_status: number
  routable: boolean
  /** Why the candidate cannot be routed to; empty when it can. */
  blocked_by: string
  last_test_result: number | null
  last_test_duration_ms: number | null
  last_tested_at: string | null
  /**
   * How many caller API keys the gateway currently has pinned to this
   * candidate's provider for this model (balanced scheduling only; always 0
   * for failover). Populated only by the model-detail endpoint — a 0 in any
   * other response means "not collected", not "nobody bound". A momentary
   * in-memory snapshot that a restart resets.
   */
  binding_count: number
}

/** How the gateway orders a model's candidate chain: which candidate leads. */
export type SchedulingMode = 'failover' | 'balanced'

/** What settlement prices a row in: token prices, the per-image tier table,
 * or the per-second video tier table. Mirrors the Go vocabulary
 * (model.BillingModeToken / BillingModeImage / BillingModeVideo) — the
 * backend normalizes every stored value onto it. */
export type BillingMode = 'token' | 'image' | 'video'

export interface Model {
  id: number
  name: string
  management_status: number
  running_status: string
  /** Which candidate leads each request; 'failover' for pre-upgrade rows. */
  scheduling_mode: SchedulingMode
  /**
   * Tri-state image-input declaration: null = undeclared (the gateway leaves
   * images alone), true/false = the admin's statement of whether this model
   * can read images (false enables vision fallback / image stripping).
   */
  supports_image_input: boolean | null
  /** What this model produces ("text", "image"), driving which endpoints answer it. */
  output_modalities: string[]
  candidates: ModelCandidate[]
  created_at: string
}

/** One row of a per-image price table: the price of a single image at this
 *  quality and size. An empty quality or size is a wildcard. */
export interface ImagePricingTier {
  quality: string
  size: string
  price: number
}

/** A candidate's per-image price table, when it bills per image. */
export interface ImagePricingTiers {
  mode: string
  tiers: ImagePricingTier[]
  default_price?: number | null
}

/** One row of a per-second video price table: what one delivered second
 *  costs at this resolution tier. An empty resolution is the generic tier
 *  that answers any resolution no named tier matches. */
export interface VideoPricingTier {
  resolution: string
  purchase_price: number
  sell_price: number
}

/** A candidate's per-second video price table, when it bills per video.
 * The shape is exactly what the backend stores — no mode slot, no default
 * price: the generic tier's own row is the video table's default. */
export interface VideoPricingTiers {
  tiers: VideoPricingTier[]
}

export interface CreateCandidateInput {
  provider_id: number
  provider_model_name: string
  input_price: number
  output_price: number
  cache_write_price?: number
  cache_read_price?: number
  max_output: number
  management_status?: number
  billing_mode?: BillingMode
  image_pricing_tiers?: ImagePricingTiers | null
  video_pricing_tiers?: VideoPricingTiers | null
}

export interface UpdateCandidateInput {
  provider_model_name: string
  input_price: number
  output_price: number
  cache_write_price?: number
  cache_read_price?: number
  max_output: number
  management_status?: number
  billing_mode?: BillingMode
  image_pricing_tiers?: ImagePricingTiers | null
  video_pricing_tiers?: VideoPricingTiers | null
}

// ProbeReport is one probe's result. `ran: false` means the probe was skipped
// (the basic probe failed first), which must read as "not tested" rather than as
// a verdict. `supported` is tri-state for the capability probes.
export interface ProbeReport {
  ran: boolean
  supported: boolean | null
  outcome: number | null
  duration_ms: number
}

export interface CandidateTestReport {
  basic: ProbeReport
  streaming: ProbeReport
  function_calling: ProbeReport
}

// `created: false` means enablement was requested but the basic probe failed, so
// nothing was stored and the operator can fix the config and retry.
export interface TestAndCreateResult {
  report: CandidateTestReport
  created: boolean
  candidate: ModelCandidate | null
}

export interface UpdateCandidateResult {
  candidate: ModelCandidate
  report: CandidateTestReport | null
  // False when a concurrent probe won the commit race: the report describes an
  // outcome the row does not hold, and presenting it as the saved state would
  // contradict the list.
  report_applied: boolean
}

export function listModels(): Promise<{ list: Model[] }> {
  return apiFetch('/api/admin/models')
}

// compactBody serialises the given fields, dropping the ones the caller did
// not submit — an absent optional stays absent on the wire, which the update
// endpoint's "present switches, absent keeps" contract depends on.
function compactBody(fields: Record<string, unknown>): string {
  const body: Record<string, unknown> = {}
  for (const [key, value] of Object.entries(fields)) {
    if (value !== undefined) body[key] = value
  }
  return JSON.stringify(body)
}

// What a model-creation request may carry besides the name. One object for
// the single and batch endpoints alike, so a new creation attribute stops
// being a positional parameter threaded through every caller.
export interface CreateModelOptions {
  schedulingMode?: SchedulingMode
  outputModalities?: string[]
}

export function createModel(name: string, opts?: CreateModelOptions): Promise<Model> {
  return apiFetch('/api/admin/models', {
    method: 'POST',
    body: compactBody({
      name,
      scheduling_mode: opts?.schedulingMode,
      output_modalities: opts?.outputModalities,
    }),
  })
}

export interface BatchSkippedModel {
  name: string
  reason: 'exists' | 'invalid'
}

export interface BatchCreateModelsResult {
  created: Model[]
  skipped: BatchSkippedModel[]
}

export function createModelsBatch(names: string[], opts?: CreateModelOptions): Promise<BatchCreateModelsResult> {
  return apiFetch('/api/admin/models/batch', {
    method: 'POST',
    body: compactBody({
      names,
      scheduling_mode: opts?.schedulingMode,
      output_modalities: opts?.outputModalities,
    }),
  })
}

export function getModel(id: number): Promise<Model> {
  return apiFetch(`/api/admin/models/${id}`)
}

/** The wire form of the tri-state declaration ("unknown" maps to null). */
export type ImageInputChoice = 'unknown' | 'yes' | 'no'

// ModelPatch is one PATCH's submitted fields; optional fields left undefined
// are omitted from the request and keep their current server-side value.
export interface ModelPatch {
  name: string
  imageInput?: ImageInputChoice
  schedulingMode?: SchedulingMode
  outputModalities?: string[]
}

export function updateModel(id: number, patch: ModelPatch): Promise<Model> {
  return apiFetch(`/api/admin/models/${id}`, {
    method: 'PATCH',
    body: compactBody({
      name: patch.name,
      image_input: patch.imageInput,
      scheduling_mode: patch.schedulingMode,
      output_modalities: patch.outputModalities,
    }),
  })
}

export function setModelStatus(id: number, enabled: boolean): Promise<void> {
  return apiFetch(`/api/admin/models/${id}/status`, { method: 'PATCH', body: JSON.stringify({ enabled }) })
}

export interface ModelImpactKey {
  id: number
  remark: string
  key_prefix: string
}

/**
 * What disabling or renaming the model touches. Allowlists reference the model
 * by id and survive a rename, so recent_request_count carries the rename risk:
 * callers ask by name.
 */
export interface ModelImpact {
  allowlisted_keys: ModelImpactKey[]
  allow_all_key_count: number
  recent_request_count: number
  recent_window_days: number
}

export function getModelImpact(id: number): Promise<ModelImpact> {
  return apiFetch(`/api/admin/models/${id}/impact`)
}


export function createCandidate(modelId: number, input: CreateCandidateInput): Promise<ModelCandidate> {
  return apiFetch(`/api/admin/models/${modelId}/candidates`, { method: 'POST', body: JSON.stringify(input) })
}

// PROBE_TIMEOUT_MS covers a save or retest that probes upstream. Its value is
// derived from the server's own per-probe bound rather than written out here,
// because the two rounds this endpoint runs (basic probe, then the two
// capability probes concurrently) each get that full bound — so a hardcoded
// number silently becomes too small the moment the server's changes. Aborting
// early is the worst possible outcome: the candidate is already stored by
// then, so the operator sees a timeout and their retry fails on the
// duplicate-provider constraint.
const PROBE_TIMEOUT_MS = CANDIDATE_PROBE_BUDGET_MS

// testAndCreateCandidate probes the mapping server-side and stores it only if
// the result allows what was asked for. Slower than createCandidate (up to two
// upstream round trips) but it is what removes the manual test step.
export function testAndCreateCandidate(modelId: number, input: CreateCandidateInput): Promise<TestAndCreateResult> {
  return apiFetch(`/api/admin/models/${modelId}/candidates/test-and-create`, {
    method: 'POST',
    body: JSON.stringify(input),
    timeoutMs: PROBE_TIMEOUT_MS,
  })
}

// May probe: renaming the target, or enabling a candidate that is not verified,
// re-verifies it server-side.
export async function updateCandidate(modelId: number, candidateId: number, input: UpdateCandidateInput): Promise<UpdateCandidateResult> {
  const data = await apiFetch<UpdateCandidateResult>(`/api/admin/models/${modelId}/candidates/${candidateId}`, {
    method: 'PATCH',
    body: JSON.stringify(input),
    timeoutMs: PROBE_TIMEOUT_MS,
  })
  // A rolling upgrade can route this call to a server one release behind,
  // whose response predates report_applied. That server has no superseded
  // concept — its probe result is always the row's own — so a missing flag
  // must read as applied, not as losing a race that could not have happened.
  return { ...data, report_applied: data.report_applied ?? true, report: data.report ?? null }
}

export function reorderCandidate(modelId: number, candidateId: number, direction: 'up' | 'down'): Promise<void> {
  return apiFetch(`/api/admin/models/${modelId}/candidates/${candidateId}/order`, {
    method: 'PATCH',
    body: JSON.stringify({ direction }),
  })
}

export function setCandidateStatus(modelId: number, candidateId: number, enabled: boolean): Promise<void> {
  return apiFetch(`/api/admin/models/${modelId}/candidates/${candidateId}/status`, {
    method: 'PATCH',
    body: JSON.stringify({ enabled }),
  })
}

// retestCandidate re-probes a stored candidate. One retest covers the basic
// mapping and both capabilities, so there is no test type to choose.
// `applied` is false when a concurrent probe won the commit race: the returned
// candidate then reflects the competitor's result, not this retest's, and the
// caller must not announce it as this click's outcome.
export interface RetestCandidateResult {
  candidate: ModelCandidate
  applied: boolean
}

export async function retestCandidate(modelId: number, candidateId: number): Promise<RetestCandidateResult> {
  const data = await apiFetch<RetestCandidateResult | ModelCandidate>(
    `/api/admin/models/${modelId}/candidates/${candidateId}/test`,
    { method: 'POST', timeoutMs: PROBE_TIMEOUT_MS },
  )
  // A rolling upgrade can route this call to a server one release behind,
  // which returns the bare candidate with no `applied` flag. That server has
  // no superseded concept either, so its response is always this click's
  // outcome — normalize instead of misreading the missing flag as superseded.
  if ('candidate' in data && data.candidate !== undefined) return data as RetestCandidateResult
  return { candidate: data as ModelCandidate, applied: true }
}

export function deleteCandidate(modelId: number, candidateId: number): Promise<void> {
  return apiFetch(`/api/admin/models/${modelId}/candidates/${candidateId}`, { method: 'DELETE' })
}

// SuggestedPrice is a price to pre-fill when adding a candidate for a provider
// + model. Source records where it came from ("history" = this provider's own
// last-saved price, "seed" = the built-in official catalog, "" = nothing
// matched) so the UI can tell the admin and they can sanity-check it.
export interface SuggestedPrice {
  input_price: number
  output_price: number
  cache_write_price: number | null
  cache_read_price: number | null
  source: 'history' | 'seed' | ''
  // When the built-in catalog was last synced (YYYY-MM-DD), set only for
  // source 'seed'. The seed is a hand-compiled snapshot of vendor pricing, so
  // its age is what tells the admin whether "from the official catalog" means
  // "trust this" or "this may be a year out of date".
  catalog_updated_at: string
}

// suggestCandidatePrice looks up a price for a provider+model (history first,
// then the built-in seed). The form fires it on model-name select so the four
// price fields auto-fill; the admin can still edit them before saving.
export function suggestCandidatePrice(
  providerId: number,
  providerModelName: string,
): Promise<SuggestedPrice> {
  const q = new URLSearchParams({
    provider_id: String(providerId),
    provider_model_name: providerModelName,
  })
  return apiFetch(`/api/admin/models/candidates/suggest-price?${q.toString()}`)
}

// ---- Bulk import (provider-scoped) ----

export interface ImportModelItemInput {
  provider_model_name: string
  // Optional per-row declaration of what the imported model produces; an
  // absent list imports as text-only. When the import creates the model the
  // declaration is stored on it; when the model already exists, a present
  // declaration must match the stored one or the row is skipped with reason
  // 'modality_mismatch'.
  output_modalities?: string[]
  input_price: number
  output_price: number
  cache_write_price: number | null
  cache_read_price: number | null
  max_output?: number
}

export interface ImportItemResult {
  name: string
  status: 'created' | 'appended' | 'skipped'
  reason?: 'exists' | 'invalid' | 'modality_mismatch'
  model_id?: number
  candidate_id?: number
}

export interface ImportProviderModelsResult {
  items: ImportItemResult[]
  created: number
  appended: number
  skipped: number
}

// importProviderModels stores one model + candidate pair per item, skipping
// what already exists. Imported mappings start disabled and unverified; the
// server queues a background probe for each and auto-enables the ones that
// pass, so the dialog polls listProviderCandidates for progress afterwards.
export function importProviderModels(
  providerId: number,
  items: ImportModelItemInput[],
): Promise<ImportProviderModelsResult> {
  return apiFetch(`/api/admin/providers/${providerId}/models/import`, {
    method: 'POST',
    body: JSON.stringify({ items }),
  })
}

// suggestPrices is the batch form of suggestCandidatePrice — one request
// prices every row of the import dialog. Every requested name has an entry;
// an empty source means no match and the row stays unpriced.
export function suggestPrices(
  providerId: number,
  names: string[],
): Promise<{ prices: Record<string, SuggestedPrice> }> {
  return apiFetch(`/api/admin/providers/${providerId}/models/suggest-prices`, {
    method: 'POST',
    body: JSON.stringify({ names }),
  })
}

export interface ProviderCandidate {
  candidate_id: number
  model_id: number
  model_name: string
  provider_model_name: string
  input_price: number
  output_price: number
  cache_write_price: number | null
  cache_read_price: number | null
  // Billing follows the mode: token prices settle "token" rows, the tier
  // table settles "image" rows (the token slots are inert there).
  billing_mode: BillingMode
  image_pricing_tiers: ImagePricingTiers | null
  video_pricing_tiers: VideoPricingTiers | null
  max_output: number
  management_status: number
  verification_status: number
  last_test_result: number | null
  last_tested_at: string | null
  last_test_error: string | null
  // Live probe-queue position, stamped by the list endpoint: 'queued' while
  // waiting for a worker, 'probing' while one is on it, '' otherwise.
  queue_state: '' | 'queued' | 'probing'
  // True while some instance's probe queue still owes this row a probe
  // outcome; an untested unstamped row without it was stored unprobed on
  // purpose and is not pending anything.
  auto_enable_on_pass: boolean
}

// listProviderCandidates returns every mapping a provider serves with its
// verification state — the import dialog's "already added" source, the
// progress poll target, and the provider detail model tab's data.
export function listProviderCandidates(providerId: number): Promise<{ list: ProviderCandidate[] }> {
  return apiFetch(`/api/admin/providers/${providerId}/candidates`)
}
