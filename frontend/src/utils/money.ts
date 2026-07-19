// frontend/src/utils/money.ts
//
// Conversions between a major-unit amount (the currency's main unit) and
// integer minor units (cents), which is how monetary caps are stored to
// avoid float drift on a cumulative hard cap. Centralizing the rounding and
// formatting policy here keeps CreateKeyModal / EditKeyDrawer / ApiKeyListPage
// consistent and gives one place to change if the precision rule ever needs
// to. Naming is currency-agnostic on purpose: the project hard-codes CNY
// today, but the conversion itself is just "major unit <-> cents".

/** Formats an integer-cent amount as a 2-decimal display string. */
export function formatCents(cents: number): string {
  return (cents / 100).toFixed(2)
}

/** Cents -> major-unit number (the inverse of toCents), for prefilling a form field. */
export function fromCents(cents: number): number {
  return cents / 100
}

/** Converts a major-unit amount to integer cents, rounded so 1.19 doesn't silently become 118. */
export function toCents(amount: number): number {
  return Math.round(amount * 100)
}
