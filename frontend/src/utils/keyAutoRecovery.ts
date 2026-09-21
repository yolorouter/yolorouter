// Pure form logic for the key-auto-recovery settings modal: interval
// validation plus the GET→form→PUT projections. Kept out of the component so
// the rules are unit-testable without mounting anything, mirroring the
// backend service-layer bounds exactly — the client rejects a bad interval
// with an explicit message instead of letting the request bounce (or worse,
// a numeric input silently clamping the value into range).

// Whole minutes, [1, 1440] — mirrors internal/service/systemsettings's
// UpdateKeyAutoRecovery validation (min 1, max one day). Keep in sync or the
// client accepts something the server rejects, or vice versa.
export const KEY_RECOVERY_MIN_INTERVAL_MINUTES = 1
export const KEY_RECOVERY_MAX_INTERVAL_MINUTES = 1440

/** Why an interval input was rejected — selects the localized message. */
export type IntervalIssue = 'empty' | 'notWholeNumber' | 'belowMin' | 'aboveMax'

export type IntervalCheck =
  | { ok: true; minutes: number }
  | { ok: false; issue: IntervalIssue }

/**
 * Validates one probe-interval input. Accepts the exact shapes NInputNumber's
 * v-model produces (`number | null` — null when cleared or unparsable), so
 * the component passes its form value straight through. NaN is treated as
 * empty: it cannot reach the wire as JSON anyway and reads to the user as
 * "nothing valid entered", not "wrong kind of number".
 */
export function checkIntervalMinutes(input: number | null | undefined): IntervalCheck {
  if (input === null || input === undefined || Number.isNaN(input)) {
    return { ok: false, issue: 'empty' }
  }
  if (!Number.isInteger(input)) return { ok: false, issue: 'notWholeNumber' }
  if (input < KEY_RECOVERY_MIN_INTERVAL_MINUTES) return { ok: false, issue: 'belowMin' }
  if (input > KEY_RECOVERY_MAX_INTERVAL_MINUTES) return { ok: false, issue: 'aboveMax' }
  return { ok: true, minutes: input }
}

/**
 * The one place an interval issue becomes an i18n key. Components and tests
 * share it, so renaming a message key can't leave a validation branch
 * pointing at a string that no longer exists.
 */
export function intervalIssueMessageKey(issue: IntervalIssue): string {
  switch (issue) {
    case 'empty':
      return 'keyAutoRecovery.intervalRequired'
    case 'notWholeNumber':
      return 'keyAutoRecovery.intervalWholeNumber'
    case 'belowMin':
      return 'keyAutoRecovery.intervalMin'
    case 'aboveMax':
      return 'keyAutoRecovery.intervalMax'
  }
}

/** The modal's editable form state (what the GET prefills, what the PUT sends). */
export interface KeyRecoveryForm {
  enabled: boolean
  interval: number | null
}

/** Projects the GET payload's snake_case row into the editable form state. */
export function keyRecoveryFormFromSetting(setting: {
  enabled: boolean
  interval_minutes: number
}): KeyRecoveryForm {
  return { enabled: setting.enabled, interval: setting.interval_minutes }
}

export type KeyRecoverySavePlan =
  | { ok: true; payload: { enabled: boolean; interval_minutes: number; version: number } }
  | { ok: false; issue: IntervalIssue }

/**
 * Everything onSave needs decided in one call: whether the current form can
 * be PUT, the exact request payload if so, or the blocking interval issue if
 * not. The component never re-derives the bounds itself.
 */
export function keyRecoverySavePlan(form: KeyRecoveryForm, version: number): KeyRecoverySavePlan {
  const check = checkIntervalMinutes(form.interval)
  if (!check.ok) return { ok: false, issue: check.issue }
  return { ok: true, payload: { enabled: form.enabled, interval_minutes: check.minutes, version } }
}
