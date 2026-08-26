// Pure display-decision helpers for the concise-output projection card.
// The card's state machine (missing / empty / all-unpriced / amount) lives
// here rather than in the page component so it is unit-testable without
// mounting anything, and so the benchmark doc link resolves by locale in
// one place.

import type { ConciseOutputProjection } from '../api/analytics'

// Which figures (or explanatory state) the card's saved-cost and
// saved-tokens cells should render. 'missing' covers not-loaded and failed
// loads alike: both render em-dashes and never block the rest of the card.
export type ProjectionDisplay =
  | { kind: 'missing' }
  | { kind: 'empty' }
  | { kind: 'unpriced-all' }
  | { kind: 'amount'; costMicros: number; savedTokens: number }

export function projectionDisplay(p: ConciseOutputProjection | null): ProjectionDisplay {
  if (!p) return { kind: 'missing' }
  if (p.output_rows === 0) return { kind: 'empty' }
  if (p.priced_rows === 0) return { kind: 'unpriced-all' }
  // A backend from before the period-total fields (a tab outliving a server
  // downgrade, or a cached response) omits them; passing undefined through
  // would render NaN, so it degrades to the same em-dash as a failed load.
  if (
    typeof p.projected_saved_cost_micros !== 'number'
    || typeof p.projected_saved_output_tokens !== 'number'
  ) {
    return { kind: 'missing' }
  }
  return {
    kind: 'amount',
    costMicros: p.projected_saved_cost_micros,
    savedTokens: p.projected_saved_output_tokens,
  }
}

// Priced output tokens as a share of all output tokens in the window; null
// when there is nothing to cover yet. Token-weighted, not request-weighted,
// so it answers the question the figures beside it raise — how much of the
// window's volume the estimate actually speaks for. A request share would
// call 99 one-token requests plus one unpriced million-token request "99%".
export function coverageRatio(p: ConciseOutputProjection | null): number | null {
  if (!p || p.output_tokens === 0) return null
  return p.priced_output_tokens / p.output_tokens
}

// The benchmark record is published in both languages; the card links the
// one matching the console locale (zh* -> the Chinese copy, else English).
const BENCHMARK_DOC_BASE =
  'https://github.com/yolorouter/yolorouter/blob/main/docs/concise-output-benchmark'

export function benchmarkDocUrl(locale: string): string {
  return locale.startsWith('zh')
    ? `${BENCHMARK_DOC_BASE}_zh.md`
    : `${BENCHMARK_DOC_BASE}.md`
}
