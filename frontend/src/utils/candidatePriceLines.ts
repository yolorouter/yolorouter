// frontend/src/utils/candidatePriceLines.ts
//
// The price lines a candidate's cell renders, shared by the provider detail
// page's price column and the model detail page's billing/price column: the
// billing mode decides which slot means anything — an image-billed mapping
// prices per delivered image out of its tier table, a video-billed one per
// delivered second per resolution tier, and a token-billed one reads its
// per-million prices with the cache pair as a quiet second line (the other
// modes' token slots are inert). The mode tag itself stays per page: only
// the provider page omits it.
//
// Muted second lines use the global .candidate-muted / .candidate-cache-price
// helpers, so the two pages (and anything later) grey out identically.

import { h, type VNodeChild } from 'vue'
import type { BillingMode, ImagePricingTiers, VideoPricingTiers } from '../api/models'
import { formatImagePrice, imagePriceSummary } from './imagePriceSummary'
import { formatVideoPrice, videoPriceLines, videoTierIsGeneric } from './videoPriceSummary'

type Translator = (key: string, named?: Record<string, unknown>) => string

// The fields both price columns read. Satisfied by ModelCandidate and
// ProviderCandidate alike.
export type PricedCandidate = {
  billing_mode: BillingMode
  image_pricing_tiers: ImagePricingTiers | null
  video_pricing_tiers: VideoPricingTiers | null
  audio_unit_price: number | null
  input_price: number
  output_price: number
  cache_write_price: number | null
  cache_read_price: number | null
}

export function candidatePriceLines(row: PricedCandidate, t: Translator): VNodeChild[] {
  if (row.billing_mode === 'image') {
    const summary = imagePriceSummary(row.image_pricing_tiers)
    if (!summary) {
      return [h('div', { class: 'candidate-muted' }, t('providers.candidateImageUnpriced'))]
    }
    if (summary.range) {
      return [h('div', t('providers.candidateImagePriceRange', { min: formatImagePrice(summary.min), max: formatImagePrice(summary.max) }))]
    }
    return [h('div', t('providers.candidateImagePrice', { price: formatImagePrice(summary.min) }))]
  }
  if (row.billing_mode === 'audio') {
    // One price is the whole declaration; its absence is unpriced, the
    // image table's own muted treatment.
    if (row.audio_unit_price === null) {
      return [h('div', { class: 'candidate-muted' }, t('providers.candidateAudioUnpriced'))]
    }
    return [h('div', t('providers.candidateAudioPrice', { price: row.audio_unit_price }))]
  }
  if (row.billing_mode === 'video') {
    const lines = videoPriceLines(row.video_pricing_tiers)
    if (lines.length === 0) {
      return [h('div', { class: 'candidate-muted' }, t('providers.candidateVideoUnpriced'))]
    }
    return lines.map((line) =>
      h(
        'div',
        videoTierIsGeneric(line)
          ? t('providers.candidateVideoPriceGeneric', { price: formatVideoPrice(line.price) })
          : t('providers.candidateVideoPrice', { resolution: line.resolution, price: formatVideoPrice(line.price) }),
      ),
    )
  }
  const lines = [h('div', `${row.input_price} / ${row.output_price}`)]
  if (row.cache_write_price !== null || row.cache_read_price !== null) {
    lines.push(
      h('div', { class: 'candidate-cache-price' }, t('providers.candidateCachePrice', { write: row.cache_write_price ?? '-', read: row.cache_read_price ?? '-' })),
    )
  }
  return lines
}
