// Maps a connection-test outcome int (the backend service.TestOutcome enum,
// 0-indexed) to its i18n label key. Kept in the same order as the Go enum and
// shared by NewProviderModal.vue and ProviderDetailPage.vue so both provider
// surfaces name a given failure identically — the array mirrors a backend
// enum, so a new/reordered outcome category is updated in exactly one place.
export const OUTCOME_I18N_KEYS = [
  'outcomeSuccess',
  'outcomeAuthFailed',
  'outcomePermissionDenied',
  'outcomeModelNotFound',
  'outcomeQuotaUnavailable',
  'outcomeRateLimited',
  'outcomeUnreachable',
  'outcomeUpstreamError',
  // The Go enum's ninth entry (TestVerificationUnsupported): a probe against
  // a protocol without a success-body validator returns this rather than
  // success. Every current protocol now has one, but rows persisted before
  // that (and any future validator-less protocol) still carry this value —
  // without the entry it fell through the fallback below and was mislabelled
  // "upstream error", pointing operators at a network problem that does not
  // exist.
  'outcomeVerificationUnsupported',
  // The Go enum's tenth entry (TestTimeout). It is split out from
  // outcomeUnreachable because the two point an operator in opposite
  // directions: unreachable means the address never connected, a timeout
  // means it did and the upstream simply took too long, so repeating
  // "check the URL spelling" here would be actively misleading.
  'outcomeTimeout',
] as const

// Resolves an outcome int to its `providers.*` i18n key, falling back to
// outcomeUpstreamError for any value outside the known enum range.
export function testOutcomeI18nKey(outcome: number): string {
  return OUTCOME_I18N_KEYS[outcome] ?? 'outcomeUpstreamError'
}

// TEST_OUTCOME_SUCCESS is the backend TestOutcome enum's success value (its
// zero value). Compare against this instead of a bare `0` so the success
// meaning is named in one place, alongside the other outcome mappings.
export const TEST_OUTCOME_SUCCESS = 0

export function isTestSuccess(outcome: number): boolean {
  return outcome === TEST_OUTCOME_SUCCESS
}

// Named alongside the other outcome constants so callers compare the int, not
// a derived i18n key string — the key lookup falls back for out-of-range
// values, and comparing against its result would silently extend any
// category-specific behavior to unknown outcomes.
export const TEST_OUTCOME_MODEL_NOT_FOUND = 3
export const TEST_OUTCOME_UPSTREAM_ERROR = 7

// testOutcomeLabel resolves an outcome int straight to its localized category
// label (the `providers.*` text) — the shape nearly every caller wants, so
// they don't each re-wrap testOutcomeI18nKey in t(`providers.${...}`).
export function testOutcomeLabel(t: (key: string) => string, outcome: number): string {
  return t(`providers.${testOutcomeI18nKey(outcome)}`)
}
