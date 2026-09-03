import type { VideoPricingTier, VideoPricingTiers } from '../api/models'
import { formatYuan } from './format'

// frontend/src/utils/videoPriceSummary.ts
//
// What a list column can show for a video-billed mapping's price: the
// per-second tier table as display rows, one line per resolution (a video
// table has no min..max compression worth reading — 720P and 1080P are
// different products, not a range of one). Empty when no tier is
// configured — the "unpriced" state for this billing mode, which the
// per-M token prices on the row say nothing about (they are inert under
// video settlement).

export interface VideoPriceLine {
  // The tier's own resolution label; '' is the generic tier, rendered by
  // the caller's "any other resolution" wording.
  resolution: string
  price: number
}

export function videoPriceLines(tiers: VideoPricingTiers | null): VideoPriceLine[] {
  if (!tiers) return []
  return (tiers.tiers ?? []).map((tier: VideoPricingTier) => ({ resolution: tier.resolution, price: tier.sell_price }))
}

// The generic tier (empty resolution) answers any resolution no named
// tier matches — worth its own wording rather than an anonymous blank.
export function videoTierIsGeneric(line: VideoPriceLine): boolean {
  return line.resolution === ''
}

// Same hand-entered yuan amounts as the image table; the shared formatter.
export function formatVideoPrice(value: number): string {
  return formatYuan(value)
}
