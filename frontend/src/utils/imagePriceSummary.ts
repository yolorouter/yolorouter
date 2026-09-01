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

// The unpriced badge follows the billing mode: an image-billed mapping is
// unpriced when no tier is configured, a token-billed one when every price
// slot is zero — the mapping's own token slots are meaningless in image mode.
export function candidateUnpriced(
  row: Pick<ProviderCandidate, 'billing_mode' | 'image_pricing_tiers'> &
    Pick<ProviderCandidate, 'input_price' | 'output_price' | 'cache_write_price' | 'cache_read_price'>,
): boolean {
  if (row.billing_mode === 'image') return imagePriceSummary(row.image_pricing_tiers) === null
  return isUnpriced(row)
}

// Prices are small yuan amounts entered by hand; rounding to 4 decimals
// only strips float noise like 0.30000000000000004.
export function formatImagePrice(value: number): string {
  return String(Math.round(value * 10000) / 10000)
}
