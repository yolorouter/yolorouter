import { describe, expect, it } from 'vitest'
import {
  KEY_RECOVERY_MAX_INTERVAL_MINUTES,
  checkIntervalMinutes,
  intervalIssueMessageKey,
  keyRecoveryFormFromSetting,
  keyRecoverySavePlan,
  type IntervalIssue,
} from './keyAutoRecovery'
import en from '../locales/en/keyAutoRecovery'
import zhCN from '../locales/zh-CN/keyAutoRecovery'

// The message keys must exist in BOTH locale files with non-empty text, so
// renaming or deleting a message fails here instead of surfacing as a raw
// key in the UI. Drives the real locale modules, not a copy of them.
const messagesByLocale: Record<string, Record<string, string>> = {
  'zh-CN': zhCN as Record<string, string>,
  en: en as Record<string, string>,
}

const ALL_ISSUES: IntervalIssue[] = ['empty', 'notWholeNumber', 'belowMin', 'aboveMax']

describe('checkIntervalMinutes', () => {
  it('accepts whole minutes at and between the bounds', () => {
    expect(checkIntervalMinutes(1)).toEqual({ ok: true, minutes: 1 })
    expect(checkIntervalMinutes(30)).toEqual({ ok: true, minutes: 30 })
    expect(checkIntervalMinutes(KEY_RECOVERY_MAX_INTERVAL_MINUTES)).toEqual({ ok: true, minutes: 1440 })
  })

  it('rejects a cleared or unparsable input as empty — the shapes NInputNumber emits', () => {
    expect(checkIntervalMinutes(null)).toEqual({ ok: false, issue: 'empty' })
    expect(checkIntervalMinutes(undefined)).toEqual({ ok: false, issue: 'empty' })
    expect(checkIntervalMinutes(Number.NaN)).toEqual({ ok: false, issue: 'empty' })
  })

  it('rejects non-integer minutes rather than rounding them', () => {
    expect(checkIntervalMinutes(3.5)).toEqual({ ok: false, issue: 'notWholeNumber' })
    expect(checkIntervalMinutes(0.5)).toEqual({ ok: false, issue: 'notWholeNumber' })
  })

  it('rejects below the minimum — 0 must not slip through as a falsy value', () => {
    expect(checkIntervalMinutes(0)).toEqual({ ok: false, issue: 'belowMin' })
    expect(checkIntervalMinutes(-30)).toEqual({ ok: false, issue: 'belowMin' })
  })

  it('rejects above the one-day maximum', () => {
    expect(checkIntervalMinutes(KEY_RECOVERY_MAX_INTERVAL_MINUTES + 1)).toEqual({
      ok: false,
      issue: 'aboveMax',
    })
  })
})

describe('intervalIssueMessageKey', () => {
  it('maps every issue to a distinct, existing message in both locales', () => {
    const keys = ALL_ISSUES.map((issue) => intervalIssueMessageKey(issue).replace('keyAutoRecovery.', ''))
    // Distinct: two issues sharing one message would hide which bound broke.
    expect(new Set(keys).size).toBe(ALL_ISSUES.length)
    for (const key of keys) {
      expect(messagesByLocale['zh-CN'][key], `zh-CN.${key}`).toBeTruthy()
      expect(messagesByLocale.en[key], `en.${key}`).toBeTruthy()
    }
  })
})

describe('keyRecoveryFormFromSetting', () => {
  it('projects the GET payload into the editable form state', () => {
    expect(keyRecoveryFormFromSetting({ enabled: true, interval_minutes: 30 })).toEqual({
      enabled: true,
      interval: 30,
    })
    expect(keyRecoveryFormFromSetting({ enabled: false, interval_minutes: 1440 })).toEqual({
      enabled: false,
      interval: 1440,
    })
  })
})

describe('keyRecoverySavePlan', () => {
  it('assembles the PUT payload with the CAS version when the interval is valid', () => {
    expect(keyRecoverySavePlan({ enabled: false, interval: 45 }, 7)).toEqual({
      ok: true,
      payload: { enabled: false, interval_minutes: 45, version: 7 },
    })
  })

  it('blocks the save and reports the issue for every invalid interval shape', () => {
    const drafts: { interval: number | null; issue: IntervalIssue }[] = [
      { interval: null, issue: 'empty' },
      { interval: 2.5, issue: 'notWholeNumber' },
      { interval: 0, issue: 'belowMin' },
      { interval: 1441, issue: 'aboveMax' },
    ]
    for (const { interval, issue } of drafts) {
      expect(keyRecoverySavePlan({ enabled: true, interval }, 3)).toEqual({ ok: false, issue })
    }
  })
})
