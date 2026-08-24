import { h } from 'vue'
import type { VNodeChild } from 'vue'
import { NTag } from 'naive-ui'
import { testOutcomeI18nKey } from './testOutcomeDisplay'
import { hintTag } from './hintTag'

export type RunningStatusTagType = 'default' | 'success' | 'warning' | 'error'

// A candidate that cannot be routed to is shown with the reason, not just the
// fact: each reason names a different repair — switch something back on, add a
// key, fill in a name, run a probe — and the fact alone leaves an operator to
// guess which of them applies. Shared by the model detail route table and the
// list pages' expand panels so ✓/✗ reads identically everywhere.
export function routableMark(
  t: (key: string) => string,
  te: (key: string) => boolean,
  c: { routable: boolean; blocked_by: string },
  opts?: {
    /**
     * Print the blocking reason next to the mark instead of behind a hover
     * tooltip — for touch layouts, where hover doesn't exist and tapping a
     * clickable card navigates away before any tooltip could show.
     */
    inlineReason?: boolean
  },
): VNodeChild {
  if (c.routable) {
    return h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => '✓' })
  }
  // A reason the locale does not know (a newer backend, an older frontend)
  // must not surface as a raw message key; the generic fallback covers it.
  const reasonKey = `models.blockedBy.${c.blocked_by}`
  const reason = c.blocked_by && te(reasonKey) ? t(reasonKey) : t('models.blockedBy.unknown')
  if (opts?.inlineReason) {
    return h('span', { style: 'display:inline-flex; align-items:center; gap:6px; min-width:0;' }, [
      h(NTag, { size: 'small', type: 'warning', bordered: false }, { default: () => '✗' }),
      h('span', { style: 'font-size:var(--text-xs); color:var(--color-text-secondary);' }, reason),
    ])
  }
  return hintTag({ text: '✗', type: 'warning', hint: reason, ariaLabel: reason })
}

// A capability flag records whether the last probe CONFIRMED the capability. It
// is informational: routing ignores it entirely, so an unconfirmed capability is
// not a reason to avoid the candidate — the remedy, if the operator cares, is to
// retest. 'unsupported' exists only because the column can still hold a false
// written by an older build; nothing writes one now.
export type CapabilityState = 'confirmed' | 'unsupported' | 'unconfirmed'

export function capabilityState(flag: boolean | null | undefined): CapabilityState {
  if (flag === true) return 'confirmed'
  if (flag === false) return 'unsupported'
  return 'unconfirmed'
}

// Localized result text for a candidate test: "passed", or "failed: <reason>"
// when the outcome is known, else a plain "failed". Reused by the row-test
// toast and the modal's result alert so both name a failure identically.
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

// modelHeaderDescription builds the detail page's header line — running
// status plus scheduling mode. A pure function over plain values so the
// wiring stays type-checked (passing a ref instead of its value is a compile
// error here) and the string shape is unit-testable.
export function modelHeaderDescription(t: (key: string) => string, status: string, balanced: boolean): string {
  const running = `${t('models.runningStatusColumn')}: ${t(`models.running${modelRunningStatusDisplay(status).i18nKey}`)}`
  const mode = balanced ? t('models.schedulingModeBalancedShort') : t('models.schedulingModeFailoverShort')
  return `${running} · ${t('models.schedulingMode')}: ${mode}`
}
