// Pure display-decision helpers for the concise-output projection card.
// The card's state machine (missing / empty / all-unpriced / amount) lives
// here rather than in the page component so it is unit-testable without
// mounting anything, and so the benchmark doc link resolves by locale in
// one place.

import type { ConciseOutputProjection } from '../api/analytics'

// Which figure (or explanatory state) the card's "per 1M output tokens"
// cell should render. 'missing' covers not-loaded and failed loads alike:
// both render em-dashes and never block the rest of the card.
export type ProjectionDisplay =
  | { kind: 'missing' }
  | { kind: 'empty' }
  | { kind: 'unpriced-all' }
  | { kind: 'amount'; micros: number }

export function projectionDisplay(p: ConciseOutputProjection | null): ProjectionDisplay {
  if (!p) return { kind: 'missing' }
  if (p.output_rows === 0) return { kind: 'empty' }
  if (p.priced_rows === 0) return { kind: 'unpriced-all' }
  // Priced traffic the backend still could not turn into a rate (an absurd
  // unit price puts the scaled figure out of range). Falls back to the
  // em-dash rather than printing the ¥0.00 a numeric default would give.
  if (p.projected_savings_per_million_tokens_micros === null) return { kind: 'missing' }
  return { kind: 'amount', micros: p.projected_savings_per_million_tokens_micros }
}

// Priced output tokens as a share of all output tokens in the window; null
// when there is nothing to cover yet. Token-weighted, not request-weighted,
// so it answers the question the figure beside it raises — how much of the
// window's volume the rate actually speaks for. A request share would call
// 99 one-token requests plus one unpriced million-token request "99%".
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
