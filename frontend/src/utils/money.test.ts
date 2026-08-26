import { describe, expect, it } from 'vitest'
import { formatSignedYuan, netCacheSavedMicros } from './money'

describe('formatSignedYuan', () => {
  it('puts the sign before the currency mark', () => {
    expect(formatSignedYuan(-150_000)).toBe('-¥0.15')
    expect(formatSignedYuan(150_000)).toBe('¥0.15')
  })
})

describe('netCacheSavedMicros', () => {
  const base = {
    total_calls: 0,
    success_calls: 0,
    ended_calls: 0,
    success_rate: 0,
    unknown_cost_calls: 0,
    input_tokens: 0,
    output_tokens: 0,
    cache_write_tokens: 0,
    cache_read_tokens: 0,
    cost_micros: 0,
  }

  it('is zero before the overview has loaded', () => {
    expect(netCacheSavedMicros(null)).toBe(0)
  })

  it('is signed — read saving minus write premium', () => {
    expect(
      netCacheSavedMicros({ ...base, cache_read_saved_micros: 100, cache_write_extra_micros: 250 }),
    ).toBe(-150)
    expect(
      netCacheSavedMicros({ ...base, cache_read_saved_micros: 250, cache_write_extra_micros: 100 }),
    ).toBe(150)
  })
})
