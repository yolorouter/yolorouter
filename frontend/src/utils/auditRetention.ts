// frontend/src/utils/auditRetention.ts
//
// Pure form logic for the admin-audit-retention field on the General
// settings page: retention validation plus the GET→form→PUT projections.
// Same module shape as utils/requestLogRetention, the sibling retention
// setting this page also hosts — kept separate because the two settings'
// localized message keys differ (an audit-retention rejection must point
// at the audit card's copy, not the request-log card's).
//
// Bounds mirror internal/service/systemsettings's
// UpdateAdminAuditRetention validation exactly: whole days, [0, 3650], 0 =
// keep forever. Keep in sync or the client accepts something the server
// rejects, or vice versa.
export const ADMIN_AUDIT_MIN_RETENTION_DAYS = 0
export const ADMIN_AUDIT_MAX_RETENTION_DAYS = 3650

/** Why a retention input was rejected — selects the localized message. */
export type AuditRetentionIssue = 'empty' | 'notWholeNumber' | 'belowMin' | 'aboveMax'

export type AuditRetentionCheck =
  | { ok: true; days: number }
  | { ok: false; issue: AuditRetentionIssue }

/**
 * Validates one retention-days input. Accepts the exact shapes
 * NInputNumber's v-model produces (`number | null` — null when cleared or
 * unparsable), so the component passes its form value straight through.
 * NaN is treated as empty: it cannot reach the wire as JSON anyway and
 * reads to the user as "nothing valid entered". 0 is a legal value — it
 * means "keep forever" (the retention loop then never deletes).
 */
export function checkAuditRetentionDays(input: number | null | undefined): AuditRetentionCheck {
  if (input === null || input === undefined || Number.isNaN(input)) {
    return { ok: false, issue: 'empty' }
  }
  if (!Number.isInteger(input)) return { ok: false, issue: 'notWholeNumber' }
  if (input < ADMIN_AUDIT_MIN_RETENTION_DAYS) return { ok: false, issue: 'belowMin' }
  if (input > ADMIN_AUDIT_MAX_RETENTION_DAYS) return { ok: false, issue: 'aboveMax' }
  return { ok: true, days: input }
}

/**
 * The one place an audit-retention issue becomes an i18n key. Components
 * and tests share it, so renaming a message key can't leave a validation
 * branch pointing at a string that no longer exists.
 */
export function auditRetentionIssueMessageKey(issue: AuditRetentionIssue): string {
  switch (issue) {
    case 'empty':
      return 'generalSettings.auditRetention.daysRequired'
    case 'notWholeNumber':
      return 'generalSettings.auditRetention.daysWholeNumber'
    case 'belowMin':
      return 'generalSettings.auditRetention.daysMin'
    case 'aboveMax':
      return 'generalSettings.auditRetention.daysMax'
  }
}

/** The audit-retention form's editable state (GET prefills, PUT sends). */
export interface AuditRetentionForm {
  days: number | null
}

/** Projects the GET payload's snake_case row into the editable form state. */
export function auditRetentionFormFromSetting(setting: {
  retention_days: number
}): AuditRetentionForm {
  return { days: setting.retention_days }
}

export type AuditRetentionSavePlan =
  | { ok: true; payload: { retention_days: number; version: number } }
  | { ok: false; issue: AuditRetentionIssue }

/**
 * Everything onSave needs decided in one call: whether the current form can
 * be PUT, the exact request payload if so, or the blocking retention issue
 * if not. The component never re-derives the bounds itself.
 */
export function auditRetentionSavePlan(
  form: AuditRetentionForm,
  version: number,
): AuditRetentionSavePlan {
  const check = checkAuditRetentionDays(form.days)
  if (!check.ok) return { ok: false, issue: check.issue }
  return { ok: true, payload: { retention_days: check.days, version } }
}
