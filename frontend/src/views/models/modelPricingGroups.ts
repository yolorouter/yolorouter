// frontend/src/views/models/modelPricingGroups.ts
//
// The model pricing page's pure layer: how the flat GET /api/admin/models
// view becomes the grouped comparison — one group per model with candidates,
// each group header's summary metrics, the per-group row order, and the
// client-side filter. No Vue, no i18n beyond label keys the page translates:
// every rule here is a pure function the page (and its tests) can call
// directly.
//
// Vocabulary note: the row sort key is the "billing primary price" (token
// row = input price, audio row = character price) — a SORT key only. The
// group header's lowest input price is a separate, stricter sum: token rows
// only that the shared candidateUnpriced predicate calls priced, so audio
// character prices (different unit) and unpriced zero-stored rows (an
// unpriced import, not "free") can never hang a fake "cheapest: 0" answer
// on a header.

import { candidateUnpriced } from '../../utils/imagePriceSummary'
import type { BillingMode, Model, ModelCandidate } from '../../api/models'

/** One mapping row inside a group: the candidate plus the model it belongs
 * to (the group context every row filter and edit action needs). */
export interface PricingRow {
  model: Model
  candidate: ModelCandidate
}

/** One model group: the (sorted) mapping rows plus the header's summary
 * metrics, all derived from the same rows so header and body can never
 * disagree. */
export interface PricingGroup {
  model: Model
  rows: PricingRow[]
  /** Mapping count ≡ provider count (UNIQUE(model_id, provider_id)). */
  providerCount: number
  /** Lowest input price among the group's token rows the shared
   * candidateUnpriced predicate calls priced (declared pricing when the
   * server sends prices_declared, the zero-value heuristic on legacy
   * servers that predate the field — the same criterion the badge, the
   * count, and the row sort use); null when the group has none (the header
   * hides the metric then). An explicitly free row (declared, stored 0)
   * participates and makes this 0. */
  minTokenInputPrice: number | null
  /** Rows whose prices were never declared (candidateUnpriced). Image and
   * video tier rows always carry their tier prices and never count. */
  unpricedCount: number
}

/** The three-part client-side filter: keyword over model / provider /
 * upstream names, unpriced-only, and one provider. */
export interface PricingFilter {
  keyword: string
  unpricedOnly: boolean
  provider: number | null
}

export function emptyPricingFilter(): PricingFilter {
  return { keyword: '', unpricedOnly: false, provider: null }
}

/** Header metrics for a set of rows — the one definition both the initial
 * build and the filtered rebuild use, so filtering re-summarizes exactly the
 * rows that remain visible. */
function groupMetrics(rows: PricingRow[]): Pick<PricingGroup, 'providerCount' | 'minTokenInputPrice' | 'unpricedCount'> {
  const pricedTokenInputs: number[] = []
  let unpricedCount = 0
  for (const { candidate } of rows) {
    // One unpriced verdict per row, consumed by every metric: the header
    // minimum, the warning count, the row sort, and the badge all read the
    // same candidateUnpriced predicate (declared pricing when the server
    // sends prices_declared, the zero-value heuristic on legacy servers
    // that predate the field), so the four can never disagree.
    const unpriced = candidateUnpriced(candidate)
    if (candidate.billing_mode === 'token' && !unpriced) pricedTokenInputs.push(candidate.input_price)
    if (unpriced) unpricedCount++
  }
  return {
    providerCount: rows.length,
    minTokenInputPrice: pricedTokenInputs.length ? Math.min(...pricedTokenInputs) : null,
    unpricedCount,
  }
}

/** One group per model that HAS candidates (a mapping-less model never
 * appears), models sorted by name ascending. */
export function buildPricingGroups(models: Model[]): PricingGroup[] {
  const groups: PricingGroup[] = []
  for (const model of models) {
    if (!model.candidates.length) continue
    const rows = sortPricingRows(model.candidates.map((candidate) => ({ model, candidate })))
    groups.push({ model, rows, ...groupMetrics(rows) })
  }
  return groups.sort((a, b) => a.model.name.localeCompare(b.model.name))
}

/** The row order inside a group: priced token/audio rows first by their
 * billing primary price ascending (input price / character price — ties
 * break by upstream name so the order is deterministic), then the tier rows
 * (image / video — no comparable single price) by upstream name, and the
 * unpriced rows sink to the bottom. */
export function sortPricingRows(rows: PricingRow[]): PricingRow[] {
  const priced: Array<{ row: PricingRow; price: number }> = []
  const tier: PricingRow[] = []
  const unpriced: PricingRow[] = []
  for (const row of rows) {
    const candidate = row.candidate
    if (candidateUnpriced(candidate)) {
      unpriced.push(row)
    } else if (candidate.billing_mode === 'token') {
      priced.push({ row, price: candidate.input_price })
    } else if (candidate.billing_mode === 'audio' && candidate.audio_unit_price !== null) {
      priced.push({ row, price: candidate.audio_unit_price })
    } else {
      tier.push(row)
    }
  }
  const byUpstream = (a: PricingRow, b: PricingRow) => a.candidate.provider_model_name.localeCompare(b.candidate.provider_model_name)
  priced.sort((a, b) => a.price - b.price || byUpstream(a.row, b.row))
  tier.sort(byUpstream)
  unpriced.sort(byUpstream)
  return [...priced.map((entry) => entry.row), ...tier, ...unpriced]
}

/** Apply an applied-filter snapshot: a row survives when it passes all three
 * constraints (the model-name keyword keeps a whole group's rows; provider
 * and upstream keywords keep only the matching rows). Groups whose rows all
 * drop out disappear with them, and the survivors' header metrics recompute
 * from exactly the visible rows. */
export function filterPricingGroups(groups: PricingGroup[], applied: PricingFilter): PricingGroup[] {
  const q = applied.keyword.trim().toLowerCase()
  const visible: PricingGroup[] = []
  for (const group of groups) {
    const rows = group.rows.filter(({ model, candidate }) => {
      if (q) {
        const modelHit = model.name.toLowerCase().includes(q)
        const providerHit = candidate.provider_name.toLowerCase().includes(q)
        const upstreamHit = candidate.provider_model_name.toLowerCase().includes(q)
        if (!modelHit && !providerHit && !upstreamHit) return false
      }
      if (applied.provider !== null && candidate.provider_id !== applied.provider) return false
      if (applied.unpricedOnly && !candidateUnpriced(candidate)) return false
      return true
    })
    if (!rows.length) continue
    visible.push({ ...group, rows, ...groupMetrics(rows) })
  }
  return visible
}

/** The billing-mode tag the row renders (label key + tag color), keyed to
 * the same billing-mode vocabulary the mapping tables use. */
export function billingModeDisplay(mode: BillingMode): { labelKey: string; tagType: 'info' | 'success' | 'default' } {
  if (mode === 'image') return { labelKey: 'models.billingPerImage', tagType: 'info' }
  if (mode === 'video') return { labelKey: 'models.billingPerVideo', tagType: 'success' }
  if (mode === 'audio') return { labelKey: 'models.billingModeAudio', tagType: 'info' }
  return { labelKey: 'models.billingPerToken', tagType: 'default' }
}
