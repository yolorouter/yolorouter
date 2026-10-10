import { describe, expect, it } from 'vitest'
import {
  CAPABILITY_MAX_MARGIN_PERCENT,
  capabilityFormFromSetting,
  capabilitySavePlan,
  checkMarginPercent,
  marginIssueMessageKey,
  type MarginIssue,
} from './capabilityEnforcement'
import en from '../locales/en/generalSettings'
import zhCN from '../locales/zh-CN/generalSettings'

// The message keys must exist in BOTH locale files with non-empty text, so
// renaming or deleting a message fails here instead of surfacing as a raw
// key in the UI. Drives the real locale modules, not a copy of them.
// Mirrors utils/auditRetention.test.ts — same module shape, this setting's
// own copy subtree.
const messagesByLocale: Record<string, Record<string, string>> = {
  'zh-CN': zhCN.capability as Record<string, string>,
  en: en.capability as Record<string, string>,
}

const ALL_ISSUES: MarginIssue[] = ['empty', 'notWholeNumber', 'belowMin', 'aboveMax']

describe('checkMarginPercent', () => {
  it('accepts whole percents at and between the bounds — including 0, the tight band', () => {
    expect(checkMarginPercent(0)).toEqual({ ok: true, margin: 0 })
    expect(checkMarginPercent(5)).toEqual({ ok: true, margin: 5 })
    expect(checkMarginPercent(CAPABILITY_MAX_MARGIN_PERCENT)).toEqual({
      ok: true,
      margin: CAPABILITY_MAX_MARGIN_PERCENT,
    })
  })

  it('rejects a cleared or unparsable input as empty — the shapes NInputNumber emits', () => {
    expect(checkMarginPercent(null)).toEqual({ ok: false, issue: 'empty' })
    expect(checkMarginPercent(undefined)).toEqual({ ok: false, issue: 'empty' })
    expect(checkMarginPercent(Number.NaN)).toEqual({ ok: false, issue: 'empty' })
  })

  it('rejects fractional percents rather than rounding them', () => {
    expect(checkMarginPercent(2.5)).toEqual({ ok: false, issue: 'notWholeNumber' })
    expect(checkMarginPercent(0.5)).toEqual({ ok: false, issue: 'notWholeNumber' })
  })

  it('rejects negatives — 0 is legal, so only negatives fall below the bound', () => {
    expect(checkMarginPercent(-1)).toEqual({ ok: false, issue: 'belowMin' })
    expect(checkMarginPercent(-100)).toEqual({ ok: false, issue: 'belowMin' })
  })

  it('rejects above the maximum', () => {
    expect(checkMarginPercent(CAPABILITY_MAX_MARGIN_PERCENT + 1)).toEqual({
      ok: false,
      issue: 'aboveMax',
    })
  })
})

describe('marginIssueMessageKey', () => {
  it('maps every issue to a distinct, existing message in both locales', () => {
    const keys = ALL_ISSUES.map((issue) =>
      marginIssueMessageKey(issue).replace('generalSettings.capability.', ''),
    )
    // Distinct: two issues sharing one message would hide which bound broke.
    expect(new Set(keys).size).toBe(ALL_ISSUES.length)
    for (const key of keys) {
      expect(messagesByLocale['zh-CN'][key], `zh-CN.capability.${key}`).toBeTruthy()
      expect(messagesByLocale.en[key], `en.capability.${key}`).toBeTruthy()
    }
  })
})

describe('capabilityFormFromSetting', () => {
  it('projects the GET payload into the editable form state', () => {
    expect(
      capabilityFormFromSetting({
        context_limit_block: true,
        temperature_default: false,
        temperature_force: true,
        tools_strict: false,
        reasoning_strict: true,
        reasoning_egress_clear: false,
        context_margin_percent: 5,
      }),
    ).toEqual({
      contextLimitBlock: true,
      temperatureDefault: false,
      temperatureForce: true,
      toolsStrict: false,
      reasoningStrict: true,
      reasoningEgressClear: false,
      margin: 5,
    })
  })
})

describe('capabilitySavePlan', () => {
  it('assembles the whole-snapshot PUT payload with the CAS version', () => {
    expect(
      capabilitySavePlan(
        {
          contextLimitBlock: true,
          temperatureDefault: false,
          temperatureForce: false,
          toolsStrict: true,
          reasoningStrict: false,
          reasoningEgressClear: true,
          margin: 10,
        },
        11,
      ),
    ).toEqual({
      ok: true,
      payload: {
        context_limit_block: true,
        temperature_default: false,
        temperature_force: false,
        tools_strict: true,
        reasoning_strict: false,
        reasoning_egress_clear: true,
        context_margin_percent: 10,
        version: 11,
      },
    })
  })

  it('blocks the save and reports the issue for every invalid margin shape', () => {
    const drafts: { margin: number | null; issue: MarginIssue }[] = [
      { margin: null, issue: 'empty' },
      { margin: 2.5, issue: 'notWholeNumber' },
      { margin: -1, issue: 'belowMin' },
      { margin: 1001, issue: 'aboveMax' },
    ]
    for (const { margin, issue } of drafts) {
      expect(
        capabilitySavePlan(
          {
            contextLimitBlock: true,
            temperatureDefault: true,
            temperatureForce: true,
            toolsStrict: true,
            reasoningStrict: true,
            reasoningEgressClear: true,
            margin,
          },
          3,
        ),
      ).toEqual({ ok: false, issue })
    }
  })
})
