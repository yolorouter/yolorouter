export type RunningStatusTagType = 'default' | 'success' | 'warning' | 'error'

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
