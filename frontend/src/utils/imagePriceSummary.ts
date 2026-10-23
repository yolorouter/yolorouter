import { formatYuan } from './format'
import { isUnpriced } from './importProgress'
import type { ImagePricingTiers, ProviderCandidate } from '../api/models'

// What a list column can show for an image-billed mapping's price: the tier
// table compressed to a min..max per delivered image. Null when no price is
// configured at all — the "unpriced" state for this billing mode, which the
// per-M token prices on the row say nothing about (they are inert under
// image settlement).
export interface ImagePriceSummary {
  min: number
  max: number
  range: boolean
}

export function imagePriceSummary(tiers: ImagePricingTiers | null): ImagePriceSummary | null {
  const prices: number[] = []
  if (tiers) {
    for (const tier of tiers.tiers ?? []) prices.push(tier.price)
    if (tiers.default_price != null) prices.push(tiers.default_price)
  }
  if (prices.length === 0) return null
  const min = Math.min(...prices)
  const max = Math.max(...prices)
  return { min, max, range: max > min }
}

// The unpriced badge is a token/audio-mode fact only: the badge reads the
// same isUnpriced helper that carries the prices_declared-first
// classification — the row's own prices_declared fact decides when the
// server sent it, and the zero-value heuristic stands in on servers that
// predate the field. Image- and video-billed mappings NEVER badge —
// their "unpriced" state lives in the tier table, where the price cell's own
// "no tiers configured" hint already tells it, and their token slots are
// inert under that settlement and would say nothing either way.
export function candidateUnpriced(
  row: Pick<ProviderCandidate, 'billing_mode'> &
    Pick<ProviderCandidate, 'input_price' | 'output_price' | 'cache_write_price' | 'cache_read_price'> &
    Partial<Pick<ProviderCandidate, 'prices_declared'>>,
): boolean {
  if (row.billing_mode === 'image' || row.billing_mode === 'video') return false
  return isUnpriced(row)
}

// Prices are small yuan amounts entered by hand; formatYuan strips only
// float noise. Kept as its own name for the image tables' call sites.
export function formatImagePrice(value: number): string {
  return formatYuan(value)
}
