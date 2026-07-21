// frontend/src/utils/money.ts
//
// Conversions between a major-unit amount (the currency's main unit) and
// integer micro-units (millionths, i.e. major_unit * 1e6), which is how
// monetary cost/budget are stored to avoid float drift on a cumulative hard
// cap while keeping 6-decimal precision. Centralizing the rounding and
// formatting policy here keeps CreateKeyModal / EditKeyDrawer / ApiKeyListPage
// and every cost display consistent, and gives one place to change if the
// precision rule ever needs to. Naming is currency-agnostic on purpose: the
// project hard-codes CNY today, but the conversion itself is just
// "major unit <-> micros".

// One major unit = 1e6 micro-units, i.e. 6 decimal places of precision.
export const MICROS_PER_UNIT = 1_000_000

/** Formats an integer-micro amount as a fixed 6-decimal display string. */
export function formatMicros(micros: number): string {
  return fromMicros(micros).toFixed(6)
}

/** Micros -> major-unit number (the inverse of toMicros), for prefilling a form field. */
export function fromMicros(micros: number): number {
  return micros / MICROS_PER_UNIT
}

/** Converts a major-unit amount to integer micros, rounded so fractional input isn't truncated. */
export function toMicros(amount: number): number {
  return Math.round(amount * MICROS_PER_UNIT)
}
