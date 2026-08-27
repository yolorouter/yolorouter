// Display projection for a key test's per-protocol results: turns the wire
// rows (see KeyTestTarget) into everything a view needs to render one line per
// protocol destination, so the presentation logic is testable without mounting
// a component and stays identical wherever the breakdown is shown.
import type { KeyTestTarget } from '../api/providers'
import { protocolLabel } from './providerProtocol'
import { isTestSuccess, testOutcomeLabel } from './testOutcomeDisplay'

export interface KeyTestTargetRow {
  /** Raw protocol id, kept as the list key. */
  protocol: string
  /** Localized protocol name, or the raw id when it has no label yet. */
  protocolLabel: string
  /** Localized outcome category ("Auth failed", "Upstream error", ...). */
  outcomeLabel: string
  passed: boolean
  durationMs: number
  /** Upstream diagnostic, shown verbatim; empty means there is none to show. */
  detail: string
}

// Mirrors the backend provider-key verification_status "passed" value.
export const VERIFICATION_STATUS_PASSED = 1

// Whether a stored key row's PASSED presentation (green verification tag with
// the per-protocol breakdown behind it) is truthful. last_test_result alone
// is not enough: verification_status can change without a new test being
// recorded — the gateway demotes a key it watched fail in production, and
// submitting a new plaintext resets verification while the previous
// credential's test columns stay behind. In both states the stored breakdown
// no longer speaks for the key, so the row must fall back to its plain
// verification tag instead of a green disclosure.
export function passedBreakdownVisible(row: {
  needs_reentry: boolean
  verification_status: number
  last_test_result: number | null
  last_test_targets?: KeyTestTarget[] | null
}): boolean {
  if (row.needs_reentry) return false
  if (row.verification_status !== VERIFICATION_STATUS_PASSED) return false
  if (row.last_test_result === null || !isTestSuccess(row.last_test_result)) return false
  return hasKeyTestBreakdown(row.last_test_targets)
}

// Whether the breakdown panel has anything the aggregate verdict does not
// already say, which is what every view gates the panel on. Two answers make
// it worth showing: more than one destination (the verdict keeps only the
// worst and never says which one that was), or a destination that quoted its
// upstream (the verdict names a category, never the upstream's own words).
//
// Deliberately cheaper than the projection below — no i18n lookups, no row
// objects — because a table cell asks this question on every render and only
// builds rows for the panel it actually opens.
export function hasKeyTestBreakdown(targets: KeyTestTarget[] | null | undefined): boolean {
  if (!Array.isArray(targets) || targets.length === 0) return false
  return targets.length > 1 || targets.some((target) => (target.detail ?? '').trim() !== '')
}

// Projects the stored per-target array into display rows. A key tested before
// per-target results existed carries null, and callers read the empty array
// as "no breakdown to offer" and render only their aggregate verdict tag.
export function keyTestTargetRows(
  t: (key: string) => string,
  targets: KeyTestTarget[] | null | undefined,
): KeyTestTargetRow[] {
  if (!Array.isArray(targets)) return []
  return targets.map((target) => ({
    protocol: target.proto,
    protocolLabel: protocolLabel(t, target.proto),
    outcomeLabel: testOutcomeLabel(t, target.outcome),
    passed: isTestSuccess(target.outcome),
    durationMs: target.duration_ms,
    // Surrounding whitespace only decides whether an empty diagnostic renders
    // as an empty box; the message itself is never rewritten. Nullish-guarded
    // like every other read in this file: an entry from an older or foreign
    // payload may omit the field entirely.
    detail: (target.detail ?? '').trim(),
  }))
}
