// frontend/src/utils/cacheEcon.ts
//
// The two verified cache formulas, defined once for every surface that
// renders them (dashboard KPI cards, the report tables' cache columns).
// The primitives are number-based rather than row-shaped on purpose: the
// backend calls the uncached count `input_tokens` on report rows and
// `uncached_input_tokens` on the platform totals, and a shape-typed helper
// would force one caller through an adapter. overviewCacheHitRate is the
// one shape-typed convenience on top, for the surfaces that all read the
// same OverviewRow payload.

import type { OverviewRow } from '../api/analytics'

/**
 * The one gate both cache figures share: did this traffic record any cache
 * metering (a nonzero read or write count)? Without it there is nothing to
 * rate or to price, and the caller renders an em-dash — never 0% / ¥0.00.
 */
export function hasCacheMetering(readTokens: number, writeTokens: number): boolean {
  return readTokens + writeTokens > 0
}

/**
 * Token-weighted cache hit rate: read ÷ (read + write + uncached), or null
 * only when the denominator is empty (no traffic at all). A genuine
 * zero-hit window over real input yields 0 — the platform card must be able
 * to display a true 0%, so this function does NOT apply the metering gate;
 * callers whose traffic may include unmetered providers (the report
 * columns) layer hasCacheMetering on top themselves.
 */
export function cacheHitRateRatio(readTokens: number, writeTokens: number, uncachedTokens: number): number | null {
  const denom = readTokens + writeTokens + uncachedTokens
  if (denom <= 0) return null
  return readTokens / denom
}

/**
 * Metering-gated hit rate for an analytics overview payload, shared by every
 * card that renders one figure for a filtered window. Null both before the
 * payload loads and when the window recorded no cache tokens, so the caller
 * renders an em-dash — never a false 0%.
 */
export function overviewCacheHitRate(ov: OverviewRow | null | undefined): number | null {
  if (!ov || !hasCacheMetering(ov.cache_read_tokens, ov.cache_write_tokens)) return null
  return cacheHitRateRatio(ov.cache_read_tokens, ov.cache_write_tokens, ov.input_tokens)
}

/**
 * Net verified cache saving in micros: read saving minus write premium.
 * Deliberately signed — a window whose cache writes cost more than its
 * reads saved must show the loss, never a clamped zero.
 */
export function cacheNetMicros(readSavedMicros: number, writeExtraMicros: number): number {
  return readSavedMicros - writeExtraMicros
}
