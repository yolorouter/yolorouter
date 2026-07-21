// Maps a connection-test outcome int (the backend service.TestOutcome enum,
// 0-indexed) to its i18n label key. Kept in the same order as the Go enum and
// shared by NewProviderDrawer.vue and ProviderDetailPage.vue so both provider
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
] as const

// Resolves an outcome int to its `providers.*` i18n key, falling back to
// outcomeUpstreamError for any value outside the known enum range.
export function testOutcomeI18nKey(outcome: number): string {
  return OUTCOME_I18N_KEYS[outcome] ?? 'outcomeUpstreamError'
}
