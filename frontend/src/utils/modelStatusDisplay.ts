import { testOutcomeI18nKey } from './testOutcomeDisplay'

export type RunningStatusTagType = 'default' | 'success' | 'warning' | 'error'

// Whether a candidate passed a given capability test — 'basic' reads its
// verification_status, streaming/function_calling their capability flags.
// Shared by ModelDetailPage.vue and CandidateEditDrawer.vue so the per-type
// pass rule lives in one place.
export function candidateTestPassed(
  testType: 'basic' | 'streaming' | 'function_calling',
  c: { verification_status: number; supports_streaming: boolean; supports_function_calling: boolean },
): boolean {
  if (testType === 'basic') return c.verification_status === 1
  if (testType === 'streaming') return c.supports_streaming
  return c.supports_function_calling
}

// Localized result text for a candidate test: "passed", or "failed: <reason>"
// when the outcome is known, else a plain "failed". Reused by the row-test
// toast and the drawer's result alert so both name a failure identically.
export function candidateTestResultText(
  t: (key: string) => string,
  passed: boolean,
  outcome: number | null | undefined,
): string {
  if (passed) return t('models.testPassed')
  if (outcome !== null && outcome !== undefined) {
    return `${t('models.testFailed')}: ${t(`providers.${testOutcomeI18nKey(outcome)}`)}`
  }
  return t('models.testFailed')
}

// Shared by ModelListPage.vue and ModelDetailPage.vue so the
// running_status → i18n key (and, where needed, NTag color) mapping is
// defined once.
export const MODEL_RUNNING_STATUS_DISPLAY: Record<string, { i18nKey: string; tagType: RunningStatusTagType }> = {
  not_configured: { i18nKey: 'NotConfigured', tagType: 'default' },
  pending_test: { i18nKey: 'Pending', tagType: 'default' },
  available: { i18nKey: 'Available', tagType: 'success' },
  degraded: { i18nKey: 'Degraded', tagType: 'warning' },
  unavailable: { i18nKey: 'Unavailable', tagType: 'error' },
}

export function modelRunningStatusDisplay(status: string) {
  return MODEL_RUNNING_STATUS_DISPLAY[status] ?? MODEL_RUNNING_STATUS_DISPLAY.unavailable
}
