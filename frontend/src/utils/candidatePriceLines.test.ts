import { describe, expect, it } from 'vitest'
import { isVNode, type VNodeChild } from 'vue'
import type { BillingMode } from '../api/models'
import { candidatePriceLines, type PricedCandidate } from './candidatePriceLines'

// The shared price renderer's five branches: an image-billed row reads its
// tier table (unpriced / range / single), a video-billed row reads one line
// per resolution tier (per-second prices, a generic tier getting its own
// wording), a token row reads its per-million prices with the cache pair as
// a quiet second line only when present. The translator stands in for
// vue-i18n by echoing key and named args, so the assertions pin WHICH
// string lands in the cell without duplicating locale copy.
const t = (key: string, named?: Record<string, unknown>) =>
  named ? `${key} ${JSON.stringify(named)}` : key

const imageRow = (tiers: PricedCandidate['image_pricing_tiers']): PricedCandidate => ({
  billing_mode: 'image' as BillingMode,
  image_pricing_tiers: tiers,
  video_pricing_tiers: null,
    audio_unit_price: null,
  input_price: 1,
  output_price: 2,
  cache_write_price: null,
  cache_read_price: null,
})

const videoRow = (tiers: PricedCandidate['video_pricing_tiers']): PricedCandidate => ({
  billing_mode: 'video' as BillingMode,
  image_pricing_tiers: null,
  video_pricing_tiers: tiers,
  audio_unit_price: null,
  input_price: 1,
  output_price: 2,
  cache_write_price: null,
  cache_read_price: null,
})

const tokenRow = (cacheWrite: number | null, cacheRead: number | null): PricedCandidate => ({
  billing_mode: 'token' as BillingMode,
  image_pricing_tiers: null,
  video_pricing_tiers: null,
    audio_unit_price: null,
  input_price: 1,
  output_price: 2,
  cache_write_price: cacheWrite,
  cache_read_price: cacheRead,
})

// Narrow one rendered line to its cell facts; a non-vnode here is a bug in
// the renderer under test, not a test-harness concern.
function cell(line: VNodeChild): { text: string; className: string } {
  if (!isVNode(line)) throw new Error('expected a vnode line')
  return { text: String(line.children), className: String(line.props?.class ?? '') }
}

describe('audio row', () => {
  it('renders the single character price, or unpriced when unset', () => {
    const row = { billing_mode: 'audio' as BillingMode, image_pricing_tiers: null, video_pricing_tiers: null, audio_unit_price: 350, input_price: 0, output_price: 0, cache_write_price: null, cache_read_price: null }
    const lines = candidatePriceLines(row, (k, n) => (k === 'providers.candidateAudioPrice' ? `¥${n?.price} / M chars` : k))
    expect(lines).toHaveLength(1)
    expect(cell(lines[0]).text).toContain('350')
    const unpriced = candidatePriceLines({ ...row, audio_unit_price: null }, (k) => k)
    expect(cell(unpriced[0]).text).toContain('candidateAudioUnpriced')
  })
})

describe('candidatePriceLines', () => {
  it('renders an unpriced image row as a muted note', () => {
    const lines = candidatePriceLines(imageRow(null), t)
    expect(lines).toHaveLength(1)
    expect(cell(lines[0]!).text).toBe('providers.candidateImageUnpriced')
    expect(cell(lines[0]!).className).toBe('candidate-muted')
  })

  it('renders a tier range as min–max', () => {
    const tiers = { mode: 'per_image', tiers: [{ quality: '', size: '', price: 0.2 }, { quality: 'standard', size: '', price: 0.3 }] }
    const lines = candidatePriceLines(imageRow(tiers), t)
    expect(lines).toHaveLength(1)
    const range = cell(lines[0]!).text
    expect(range).toContain('providers.candidateImagePriceRange')
    expect(range).toContain('"min":"0.2"')
    expect(range).toContain('"max":"0.3"')
  })

  it('renders a single-tier table as one price', () => {
    const tiers = { mode: 'per_image', tiers: [{ quality: '', size: '', price: 0.25 }] }
    const lines = candidatePriceLines(imageRow(tiers), t)
    expect(lines).toHaveLength(1)
    expect(cell(lines[0]!).text).toBe('providers.candidateImagePrice {"price":"0.25"}')
  })

  it('renders one per-resolution line for a video table, generic tier apart', () => {
    const tiers = {
      tiers: [
        { resolution: '720P', purchase_price: 0.6, sell_price: 0.7 },
        { resolution: '1080P', purchase_price: 1, sell_price: 1.2 },
      ],
    }
    const lines = candidatePriceLines(videoRow(tiers), t)
    expect(lines).toHaveLength(2)
    expect(cell(lines[0]!).text).toBe('providers.candidateVideoPrice {"resolution":"720P","price":"0.7"}')
    expect(cell(lines[1]!).text).toBe('providers.candidateVideoPrice {"resolution":"1080P","price":"1.2"}')
  })

  it('renders the generic video tier with its own wording', () => {
    const tiers = { tiers: [{ resolution: '', purchase_price: 0.5, sell_price: 0.55 }] }
    const lines = candidatePriceLines(videoRow(tiers), t)
    expect(lines).toHaveLength(1)
    expect(cell(lines[0]!).text).toBe('providers.candidateVideoPriceGeneric {"price":"0.55"}')
  })

  it('renders an unpriced video row as a muted note', () => {
    const lines = candidatePriceLines(videoRow(null), t)
    expect(lines).toHaveLength(1)
    expect(cell(lines[0]!).text).toBe('providers.candidateVideoUnpriced')
    expect(cell(lines[0]!).className).toBe('candidate-muted')
  })

  it('renders token prices with the cache pair as a quiet second line only when present', () => {
    const withCache = candidatePriceLines(tokenRow(0.5, 0.1), t)
    expect(withCache).toHaveLength(2)
    expect(cell(withCache[0]!).text).toBe('1 / 2')
    expect(cell(withCache[1]!).text).toBe('providers.candidateCachePrice {"write":0.5,"read":0.1}')
    expect(cell(withCache[1]!).className).toBe('candidate-cache-price')

    const withoutCache = candidatePriceLines(tokenRow(null, null), t)
    expect(withoutCache).toHaveLength(1)
    expect(cell(withoutCache[0]!).text).toBe('1 / 2')
  })
})
