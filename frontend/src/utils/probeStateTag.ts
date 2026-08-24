import { h, type VNode } from 'vue'
import { NTag } from 'naive-ui'
import type { ProviderCandidate } from '../api/models'
import { candidateDisplayState, pendingStateBadge } from './importProgress'

// The failure-reason cell: the persisted diagnostic in red, or a muted dash
// when the mapping has none. Shared by the import progress view and the
// provider detail model tab (same no-drift reason as the badge below); the
// classes live in styles/global.less.
export function renderFailReasonCell(row: Pick<ProviderCandidate, 'last_test_error'>): VNode {
  return h('span', { class: row.last_test_error ? 'candidate-fail-reason' : 'candidate-muted' }, row.last_test_error ?? '-')
}

// The one renderer for a mapping's verification-state badge, shared by the
// import progress view and the provider detail model tab so the two can never
// drift. "Passed · Enabled" is only claimed when the mapping is actually
// enabled; a passed mapping an admin switched off demotes to a warning badge.
// A pending mapping names its live queue position ("queued" / "probing") when
// the queue holds it; only the no-position presentation differs by context
// (an import in flight says "pending probe", a settled list says "untested"),
// so that fallback label key and tag type are the caller's.
export function renderProbeStateTag(
  t: (key: string) => string,
  row: Pick<ProviderCandidate, 'verification_status' | 'management_status' | 'last_tested_at' | 'queue_state'> &
    Partial<Pick<ProviderCandidate, 'auto_enable_on_pass'>>,
  pending: { labelKey: string; type: 'info' | 'default' },
): VNode {
  const state = candidateDisplayState(row)
  if (state === 'passed')
    return h(NTag, { size: 'small', bordered: false, type: 'success' }, { default: () => t('models.importStatePassed') })
  if (state === 'passed_disabled')
    return h(NTag, { size: 'small', bordered: false, type: 'warning' }, { default: () => t('models.importStatePassedDisabled') })
  if (state === 'failed')
    return h(NTag, { size: 'small', bordered: false, type: 'error' }, { default: () => t('models.importStateFailed') })
  if (state === 'inconclusive')
    return h(NTag, { size: 'small', bordered: false, type: 'warning' }, { default: () => t('models.importStateInconclusive') })
  // idle is terminal: no queue owes this row anything (a manual
  // save-as-disabled, or a promise a retarget revoked). Labeling it with the
  // caller's pending fallback would claim a probe nobody scheduled.
  if (state === 'idle')
    return h(NTag, { size: 'small', bordered: false, type: 'default' }, { default: () => t('models.importStateIdle') })
  const badge = pendingStateBadge(row, pending)
  return h(NTag, { size: 'small', bordered: false, type: badge.type }, { default: () => t(badge.labelKey) })
}
