// frontend/src/utils/money.ts
//
// Conversions between a major-unit amount (the currency's main unit) and
// integer micro-units (millionths, i.e. major_unit * 1e6), which is how
// monetary cost/budget are stored to avoid float drift on a cumulative hard
// cap while keeping 6-decimal precision. Centralizing the rounding and
// formatting policy here keeps CreateKeyModal / EditKeyModal / ApiKeyListPage
// and every cost display consistent, and gives one place to change if the
// precision rule ever needs to. Naming is currency-agnostic on purpose: the
// project hard-codes CNY today, but the conversion itself is just
// "major unit <-> micros".

import type { OverviewRow } from '../api/analytics'
import { cacheNetMicros } from './cacheEcon'

// One major unit = 1e6 micro-units, i.e. 6 decimal places of precision.
export const MICROS_PER_UNIT = 1_000_000

/** Formats an integer-micro amount as a fixed-precision display string. */
export function formatMicros(micros: number, precision = 6): string {
  const s = fromMicros(micros).toFixed(precision)
  // A tiny negative that rounds to zero (e.g. -0.003 at precision 2) renders as
  // "-0.00"; collapse the misleading minus so it shows as plain "0.00".
  return s.replace(/^-(0(?:\.0+)?)$/, '$1')
}

/**
 * True when the amount renders with a real minus sign at `precision` decimals
 * rather than a sub-rounding "-0.00". Derived from formatMicros so the rounding
 * and negative-zero policy stay in one place — callers pass the same precision
 * they format with, so a value that displays as zero is never styled as a loss.
 */
export function isNegativeMicros(micros: number, precision = 6): boolean {
  return formatMicros(micros, precision).startsWith('-')
}

/** Micros -> major-unit number (the inverse of toMicros), for prefilling a form field. */
export function fromMicros(micros: number): number {
  return micros / MICROS_PER_UNIT
}

/** Converts a major-unit amount to integer micros, rounded so fractional input isn't truncated. */
export function toMicros(amount: number): number {
  return Math.round(amount * MICROS_PER_UNIT)
}

/**
 * Formats a signed micros amount with the currency mark inside the sign
 * ("-¥0.12", never "¥-0.12"). Uses formatMicros underneath so the rounding
 * and negative-zero policy stay in one place; pair with isNegativeMicros at
 * the same precision when styling losses.
 */
export function formatSignedYuan(micros: number, precision = 2): string {
  const s = formatMicros(micros, precision)
  return s.startsWith('-') ? `-¥${s.slice(1)}` : `¥${s}`
}

// netCacheSavedMicros is the OverviewRow-shaped convenience over the shared
// net-saving formula (cacheNetMicros) — one formula, two entry points, so the
// card surfaces and the report columns cannot drift.
export function netCacheSavedMicros(ov: OverviewRow | null | undefined): number {
  return cacheNetMicros(ov?.cache_read_saved_micros ?? 0, ov?.cache_write_extra_micros ?? 0)
}
