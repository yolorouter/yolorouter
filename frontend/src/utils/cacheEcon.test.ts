import { describe, expect, it } from 'vitest'
import { cacheHitRateRatio, cacheNetMicros, hasCacheMetering, overviewCacheHitRate } from './cacheEcon'
import type { OverviewRow } from '../api/analytics'

function overviewWith(input: number, write: number, read: number): OverviewRow {
  return {
    total_calls: 1,
    success_calls: 1,
    ended_calls: 1,
    success_rate: 1,
    unknown_cost_calls: 0,
    input_tokens: input,
    output_tokens: 0,
    cache_write_tokens: write,
    cache_read_tokens: read,
    cost_micros: 0,
    cache_read_saved_micros: 0,
    cache_write_extra_micros: 0,
  }
}

describe('overviewCacheHitRate', () => {
  it('is null before the overview has loaded', () => {
    expect(overviewCacheHitRate(null)).toBeNull()
  })

  it('is null when the window recorded no cache metering', () => {
    // Real input traffic, zero cache tokens: "no cache activity", not 0%.
    expect(overviewCacheHitRate(overviewWith(500, 0, 0))).toBeNull()
  })

  it('computes the token-weighted ratio from the overview sums', () => {
    // read=600, write=200, uncached=1200 → 600 / 2000 = 0.3
    expect(overviewCacheHitRate(overviewWith(1200, 200, 600))).toBeCloseTo(0.3)
  })

  it('reports a true 0% when metering exists but nothing was read', () => {
    expect(overviewCacheHitRate(overviewWith(100, 50, 0))).toBe(0)
  })
})

describe('hasCacheMetering', () => {
  it('requires a nonzero read or write count', () => {
    expect(hasCacheMetering(0, 0)).toBe(false)
    expect(hasCacheMetering(1, 0)).toBe(true)
    expect(hasCacheMetering(0, 1)).toBe(true)
  })
})

describe('cacheHitRateRatio', () => {
  it('is null only on an empty denominator', () => {
    expect(cacheHitRateRatio(0, 0, 0)).toBeNull()
  })

  it('divides read by the full token denominator', () => {
    expect(cacheHitRateRatio(300, 100, 600)).toBeCloseTo(0.3)
  })

  it('reports a true 0% when input exists but nothing was read', () => {
    expect(cacheHitRateRatio(0, 50, 100)).toBe(0)
  })
})

describe('cacheNetMicros', () => {
  it('is signed — a write premium above the read saving goes negative', () => {
    expect(cacheNetMicros(100, 250)).toBe(-150)
    expect(cacheNetMicros(250, 100)).toBe(150)
  })
})
