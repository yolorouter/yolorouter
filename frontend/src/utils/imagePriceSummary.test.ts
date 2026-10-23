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
    video_pricing_tiers: null,
    input_price: 0,
    output_price: 0,
    cache_write_price: null,
    cache_read_price: null,
  }

  it('keeps the all-zero heuristic for token rows on servers without prices_declared', () => {
    expect(candidateUnpriced(tokenRow)).toBe(true)
    expect(candidateUnpriced({ ...tokenRow, input_price: 2 })).toBe(false)
  })

  it('reads the declared fact when the server sends it: undeclared badges, explicitly free does not', () => {
    // Undeclared means unpriced whatever the stored slots hold; a declared
    // all-zero row is an explicit free-price claim, never a badge.
    expect(candidateUnpriced({ ...tokenRow, prices_declared: false })).toBe(true)
    expect(candidateUnpriced({ ...tokenRow, prices_declared: false, input_price: 2, output_price: 5 })).toBe(true)
    expect(candidateUnpriced({ ...tokenRow, prices_declared: true })).toBe(false)
  })

  it('badges undeclared audio rows on the same declared fact', () => {
    const audioRow = { ...tokenRow, billing_mode: 'audio' as const }
    expect(candidateUnpriced({ ...audioRow, prices_declared: false })).toBe(true)
    expect(candidateUnpriced({ ...audioRow, prices_declared: true })).toBe(false)
    // Old-server fallback: the zero-value heuristic stands in.
    expect(candidateUnpriced(audioRow)).toBe(true)
  })

  it('never badges image-billed rows — the tier hint carries the no-tier state instead', () => {
    // Spec change (the badge is a token/audio fact now): an image row with no
    // tier table used to badge; its "unpriced" state is the price cell's own
    // "no tiers configured" hint, and its token slots are inert here.
    const imageRow = {
      ...tokenRow,
      billing_mode: 'image' as const,
      input_price: 0.3,
      output_price: 1.2,
      image_pricing_tiers: null,
    }
    const pricedImageRow = { ...imageRow, image_pricing_tiers: tiers({ default_price: 0.25 }) }
    expect(candidateUnpriced(imageRow)).toBe(false)
    expect(candidateUnpriced(pricedImageRow)).toBe(false)
  })

  it('never badges video-billed rows either', () => {
    const videoRow = { ...tokenRow, billing_mode: 'video' as const, input_price: 0.3, output_price: 1.2 }
    const pricedVideoRow = {
      ...videoRow,
      video_pricing_tiers: { tiers: [{ resolution: '720P', purchase_price: 0.6, sell_price: 0.7 }] },
    }
    expect(candidateUnpriced(videoRow)).toBe(false)
    expect(candidateUnpriced(pricedVideoRow)).toBe(false)
  })
})

describe('formatImagePrice', () => {
  it('strips float noise without touching sane values', () => {
    expect(formatImagePrice(0.30000000000000004)).toBe('0.3')
    expect(formatImagePrice(2)).toBe('2')
    expect(formatImagePrice(0.25)).toBe('0.25')
  })
})
