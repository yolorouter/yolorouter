import { describe, expect, it } from 'vitest'
import type { ConciseOutputProjection } from '../api/analytics'
import {
  benchmarkDocUrl,
  coverageRatio,
  projectionDisplay,
} from './conciseProjection'

// A minimal valid projection. The default figures are internally consistent
// — the rate really is spend / priced tokens x coefficient — so a stale
// number here cannot quietly outlive a benchmark update.
const COEFFICIENT = 0.126
const SPEND_MICROS = 700_000
const PRICED_TOKENS = 700_000

function projection(over: Partial<ConciseOutputProjection>): ConciseOutputProjection {
  return {
    window: { start: '2026-08-17T00:00:00Z', end: '2026-08-24T00:00:00Z', days: 7 },
    output_spend_micros: SPEND_MICROS,
    output_rows: 10,
    output_tokens: 1_000_000,
    priced_rows: 9,
    priced_output_tokens: PRICED_TOKENS,
    projected_savings_per_million_tokens_micros:
      Math.round((SPEND_MICROS * COEFFICIENT * 1e6) / PRICED_TOKENS),
    coefficient: COEFFICIENT,
    ...over,
  }
}

describe('projectionDisplay', () => {
  it('null (not loaded / failed) is missing — em-dashes, never a ¥0 figure', () => {
    expect(projectionDisplay(null)).toEqual({ kind: 'missing' })
  })

  it('no output traffic in the window is the empty state, not a zero amount', () => {
    expect(projectionDisplay(projection({ output_rows: 0, output_tokens: 0, priced_rows: 0, priced_output_tokens: 0 })))
      .toEqual({ kind: 'empty' })
  })

  it('traffic that is entirely unpriced gets its own state — no ¥0.00 over real traffic', () => {
    expect(projectionDisplay(projection({ output_rows: 5, priced_rows: 0 }))).toEqual({ kind: 'unpriced-all' })
  })

  it('priced traffic with no computable rate is missing, not a ¥0.00 amount', () => {
    expect(projectionDisplay(projection({ projected_savings_per_million_tokens_micros: null })))
      .toEqual({ kind: 'missing' })
  })

  it('priced traffic yields the per-million amount', () => {
    expect(projectionDisplay(projection({ projected_savings_per_million_tokens_micros: 711_800 })))
      .toEqual({ kind: 'amount', micros: 711_800 })
  })
})

describe('coverageRatio', () => {
  it('is null while missing or with nothing to cover', () => {
    expect(coverageRatio(null)).toBeNull()
    expect(coverageRatio(projection({ output_tokens: 0, priced_output_tokens: 0 }))).toBeNull()
  })

  it('divides priced output tokens by all output tokens in the window', () => {
    expect(coverageRatio(projection({ output_tokens: 301, priced_output_tokens: 300 }))).toBeCloseTo(0.99668, 5)
    expect(coverageRatio(projection({ output_tokens: 8, priced_output_tokens: 8 }))).toBe(1)
  })

  it('is token-weighted, not request-weighted: 99 tiny priced calls next to one huge unpriced call is ~0%, not 99%', () => {
    const skewed = projection({
      output_rows: 100,
      priced_rows: 99,
      output_tokens: 1_000_099,
      priced_output_tokens: 99,
    })
    expect(coverageRatio(skewed)).toBeCloseTo(0.000099, 6)
  })
})

describe('benchmarkDocUrl', () => {
  it('links the Chinese copy for Chinese locales and English otherwise', () => {
    expect(benchmarkDocUrl('zh-CN')).toBe(
      'https://github.com/yolorouter/yolorouter/blob/main/docs/concise-output-benchmark_zh.md')
    expect(benchmarkDocUrl('zh')).toBe(
      'https://github.com/yolorouter/yolorouter/blob/main/docs/concise-output-benchmark_zh.md')
    expect(benchmarkDocUrl('en')).toBe(
      'https://github.com/yolorouter/yolorouter/blob/main/docs/concise-output-benchmark.md')
  })
})
