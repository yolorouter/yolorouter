// frontend/src/views/models/modelPriceSummary.ts
//
// The models list' per-model pricing view: a thin flat-list adaptation of the
// pricing page's pure layer (modelPricingGroups.ts). That module reasons in
// per-model GROUPS; the models table needs the same facts per model row —
// the lowest priced-token input price, the unpriced count, and the
// price-sorted comparison order with its cheapest row. These helpers reuse
// its predicates, aggregation, and row sort instead of restating them (one
// unpriced verdict via candidateUnpriced, one min/count loop via
// candidateMetrics, one sort rule via sortPricingRows), so the list column,
// the filter, and the expanded comparison surface can never drift apart from
// the pricing page they mirror.

import { candidateUnpriced } from '../../utils/imagePriceSummary'
import { candidateMetrics, sortPricingRows, type PricingRow } from './modelPricingGroups'
import type { Model, ModelCandidate } from '../../api/models'

/** Summary metrics for one model's candidates — the flat-list mirror of the
 * pricing page's group header metrics. */
export interface ModelPriceMetrics {
  /** Lowest input price among the model's token candidates the shared
   * candidateUnpriced predicate calls priced (declared pricing when the
   * server sends prices_declared, the zero-value heuristic on legacy servers
   * that predate the field); null when the model has none — the cell hides
   * the metric then. An explicitly free row (declared, stored 0) participates
   * and makes this 0. */
  minTokenInputPrice: number | null
  /** Candidates whose prices were never declared (candidateUnpriced). Image
   * and video tier candidates always carry their tier prices and never
   * count. */
  unpricedCount: number
}

/** The per-model metrics, derived by the group layer's shared aggregation
 * (candidateMetrics) — the same one-verdict-per-row loop the pricing page's
 * group headers run, so the minimum, the count, and the badges all read the
 * same predicate. */
export function modelPriceMetrics(candidates: ModelCandidate[]): ModelPriceMetrics {
  return candidateMetrics(candidates)
}

/** The unpriced-only filter's model-level predicate: the model matches when
 * ANY of its candidates is unpriced. */
export function modelHasUnpriced(candidates: ModelCandidate[]): boolean {
  return candidates.some(candidateUnpriced)
}

/** The expanded comparison surface's rows: the model's candidates under the
 * pricing page's row sort — priced token/audio rows first by their billing
 * primary price ascending (input price / character price, ties by upstream
 * name), then the tier rows (image / video) by upstream name, unpriced rows
 * sunk to the bottom. */
export function comparisonRows(model: Model): PricingRow[] {
  return sortPricingRows(model.candidates.map((candidate) => ({ model, candidate })))
}

/** Whether a candidate sits in the SORT's priced bucket — the exact branch
 * shape sortPricingRows uses, so "cheapest" cannot name a row the sort put
 * in a different bucket. */
function isPricedTokenOrAudio(candidate: ModelCandidate): boolean {
  if (candidateUnpriced(candidate)) return false
  if (candidate.billing_mode === 'token') return true
  return candidate.billing_mode === 'audio' && candidate.audio_unit_price !== null
}

/** The comparison surface's cheapest row: the first row of the sorted order
 * when it is a priced token/audio row (sortPricingRows leads with those,
 * ascending by representative price). Null when the model has no priced
 * token/audio row — tier rows and unpriced rows never carry the tag. */
export function cheapestComparisonRow(rows: PricingRow[]): PricingRow | null {
  const first = rows[0]
  return first && isPricedTokenOrAudio(first.candidate) ? first : null
}
