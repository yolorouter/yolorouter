// frontend/src/utils/capabilityEnforcement.ts
//
// Pure form logic for the capability-enforcement card on the General
// settings page: margin validation plus the GET→form→PUT projections. Same
// module shape as utils/keyAutoRecovery (the sibling switch+number setting
// this page also hosts).
//
// Bounds mirror internal/service/systemsettings's
// UpdateCapabilityEnforcement validation exactly: whole percent, [0, 1000].
// Keep in sync or the client accepts something the server rejects, or vice
// versa.
export const CAPABILITY_MIN_MARGIN_PERCENT = 0
export const CAPABILITY_MAX_MARGIN_PERCENT = 1000

/** Why a margin input was rejected — selects the localized message. */
export type MarginIssue = 'empty' | 'notWholeNumber' | 'belowMin' | 'aboveMax'

export type MarginCheck =
  | { ok: true; margin: number }
  | { ok: false; issue: MarginIssue }

/**
 * Validates one margin-percent input. Accepts the exact shapes
 * NInputNumber's v-model produces (`number | null` — null when cleared or
 * unparsable), so the component passes its form value straight through.
 * NaN is treated as empty: it cannot reach the wire as JSON anyway and
 * reads to the user as "nothing valid entered". 0 is a legal value — it
 * narrows the tolerance band to the declared window itself.
 */
export function checkMarginPercent(input: number | null | undefined): MarginCheck {
  if (input === null || input === undefined || Number.isNaN(input)) {
    return { ok: false, issue: 'empty' }
  }
  if (!Number.isInteger(input)) return { ok: false, issue: 'notWholeNumber' }
  if (input < CAPABILITY_MIN_MARGIN_PERCENT) return { ok: false, issue: 'belowMin' }
  if (input > CAPABILITY_MAX_MARGIN_PERCENT) return { ok: false, issue: 'aboveMax' }
  return { ok: true, margin: input }
}

/**
 * The one place a margin issue becomes an i18n key. Components and tests
 * share it, so renaming a message key can't leave a validation branch
 * pointing at a string that no longer exists.
 */
export function marginIssueMessageKey(issue: MarginIssue): string {
  switch (issue) {
    case 'empty':
      return 'generalSettings.capability.marginRequired'
    case 'notWholeNumber':
      return 'generalSettings.capability.marginWholeNumber'
    case 'belowMin':
      return 'generalSettings.capability.marginMin'
    case 'aboveMax':
      return 'generalSettings.capability.marginMax'
  }
}

/**
 * The card's editable state (what the GET prefills, what the PUT sends):
 * the six enforcement switches plus the margin they share. The switches
 * need no validation — NSwitch only yields booleans — so the margin is the
 * single validated field.
 */
export interface CapabilityEnforcementForm {
  contextLimitBlock: boolean
  temperatureDefault: boolean
  temperatureForce: boolean
  toolsStrict: boolean
  reasoningStrict: boolean
  reasoningEgressClear: boolean
  margin: number | null
}

/** Projects the GET payload's snake_case rows into the editable form state. */
export function capabilityFormFromSetting(setting: {
  context_limit_block: boolean
  temperature_default: boolean
  temperature_force: boolean
  tools_strict: boolean
  reasoning_strict: boolean
  reasoning_egress_clear: boolean
  context_margin_percent: number
}): CapabilityEnforcementForm {
  return {
    contextLimitBlock: setting.context_limit_block,
    temperatureDefault: setting.temperature_default,
    temperatureForce: setting.temperature_force,
    toolsStrict: setting.tools_strict,
    reasoningStrict: setting.reasoning_strict,
    reasoningEgressClear: setting.reasoning_egress_clear,
    margin: setting.context_margin_percent,
  }
}

export type CapabilitySavePlan =
  | {
      ok: true
      payload: {
        context_limit_block: boolean
        temperature_default: boolean
        temperature_force: boolean
        tools_strict: boolean
        reasoning_strict: boolean
        reasoning_egress_clear: boolean
        context_margin_percent: number
        version: number
      }
    }
  | { ok: false; issue: MarginIssue }

/**
 * Everything onSave needs decided in one call: whether the current form can
 * be PUT, the exact whole-snapshot request payload if so, or the blocking
 * margin issue if not. The component never re-derives the bounds itself.
 */
export function capabilitySavePlan(
  form: CapabilityEnforcementForm,
  version: number,
): CapabilitySavePlan {
  const check = checkMarginPercent(form.margin)
  if (!check.ok) return { ok: false, issue: check.issue }
  return {
    ok: true,
    payload: {
      context_limit_block: form.contextLimitBlock,
      temperature_default: form.temperatureDefault,
      temperature_force: form.temperatureForce,
      tools_strict: form.toolsStrict,
      reasoning_strict: form.reasoningStrict,
      reasoning_egress_clear: form.reasoningEgressClear,
      context_margin_percent: check.margin,
      version,
    },
  }
}
