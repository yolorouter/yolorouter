import { describe, expect, it } from 'vitest'
import { candidateUnpriced, formatImagePrice, imagePriceSummary } from './imagePriceSummary'
import type { ImagePricingTiers } from '../api/models'

const tiers = (over: Partial<ImagePricingTiers>): ImagePricingTiers => ({
  mode: 'per_image',
  tiers: [],
  default_price: null,
  ...over,
})

describe('imagePriceSummary', () => {
  it('is null with no table at all', () => {
    expect(imagePriceSummary(null)).toBeNull()
    expect(imagePriceSummary(tiers({}))).toBeNull()
  })

  it('compresses a single tier to one price', () => {
    const summary = imagePriceSummary(tiers({ tiers: [{ quality: 'standard', size: '1024*1024', price: 0.2 }] }))
    expect(summary).toEqual({ min: 0.2, max: 0.2, range: false })
  })

  it('shows a range across tiers and folds the default in', () => {
    const summary = imagePriceSummary(
      tiers({
        tiers: [
          { quality: 'standard', size: '1024*1024', price: 0.2 },
          { quality: 'pro', size: '2K', price: 0.6 },
        ],
        default_price: 0.4,
      }),
    )
    expect(summary).toEqual({ min: 0.2, max: 0.6, range: true })
  })

  it('works from the default price alone', () => {
    const summary = imagePriceSummary(tiers({ default_price: 0.35 }))
    expect(summary).toEqual({ min: 0.35, max: 0.35, range: false })
  })
})

describe('candidateUnpriced', () => {
  const tokenRow = {
    billing_mode: 'token' as const,
    image_pricing_tiers: null,
    input_price: 0,
    output_price: 0,
    cache_write_price: null,
    cache_read_price: null,
  }

  it('keeps the all-zero token rule for token-billed rows', () => {
    expect(candidateUnpriced(tokenRow)).toBe(true)
    expect(candidateUnpriced({ ...tokenRow, input_price: 2 })).toBe(false)
  })

  it('judges image-billed rows by the tier table, not the inert token slots', () => {
    // Imported image mappings carry submitted token prices that settle
    // nothing — the badge must not read them as "priced".
    const imageRow = {
      ...tokenRow,
      billing_mode: 'image' as const,
      input_price: 0.3,
      output_price: 1.2,
      image_pricing_tiers: null,
    }
    expect(candidateUnpriced(imageRow)).toBe(true)
    expect(
      candidateUnpriced({
        ...imageRow,
        image_pricing_tiers: tiers({ default_price: 0.25 }),
      }),
    ).toBe(false)
  })
})

describe('formatImagePrice', () => {
  it('strips float noise without touching sane values', () => {
    expect(formatImagePrice(0.30000000000000004)).toBe('0.3')
    expect(formatImagePrice(2)).toBe('2')
    expect(formatImagePrice(0.25)).toBe('0.25')
  })
})
