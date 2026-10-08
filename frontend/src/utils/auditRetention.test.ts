import { describe, expect, it } from 'vitest'
import {
  ADMIN_AUDIT_MAX_RETENTION_DAYS,
  auditRetentionFormFromSetting,
  auditRetentionIssueMessageKey,
  auditRetentionSavePlan,
  checkAuditRetentionDays,
  type AuditRetentionIssue,
} from './auditRetention'
import en from '../locales/en/generalSettings'
import zhCN from '../locales/zh-CN/generalSettings'

// The message keys must exist in BOTH locale files with non-empty text, so
// renaming or deleting a message fails here instead of surfacing as a raw
// key in the UI. Drives the real locale modules, not a copy of them.
// Mirrors utils/requestLogRetention.test.ts — same module shape, this
// setting's own copy subtree.
const messagesByLocale: Record<string, Record<string, string>> = {
  'zh-CN': zhCN.auditRetention as Record<string, string>,
  en: en.auditRetention as Record<string, string>,
}

const ALL_ISSUES: AuditRetentionIssue[] = ['empty', 'notWholeNumber', 'belowMin', 'aboveMax']

describe('checkAuditRetentionDays', () => {
  it('accepts whole days at and between the bounds — including 0, keep forever', () => {
    expect(checkAuditRetentionDays(0)).toEqual({ ok: true, days: 0 })
    expect(checkAuditRetentionDays(1)).toEqual({ ok: true, days: 1 })
    expect(checkAuditRetentionDays(180)).toEqual({ ok: true, days: 180 })
    expect(checkAuditRetentionDays(ADMIN_AUDIT_MAX_RETENTION_DAYS)).toEqual({ ok: true, days: 3650 })
  })

  it('rejects a cleared or unparsable input as empty — the shapes NInputNumber emits', () => {
    expect(checkAuditRetentionDays(null)).toEqual({ ok: false, issue: 'empty' })
    expect(checkAuditRetentionDays(undefined)).toEqual({ ok: false, issue: 'empty' })
    expect(checkAuditRetentionDays(Number.NaN)).toEqual({ ok: false, issue: 'empty' })
  })

  it('rejects non-integer days rather than rounding them', () => {
    expect(checkAuditRetentionDays(1.5)).toEqual({ ok: false, issue: 'notWholeNumber' })
    expect(checkAuditRetentionDays(0.5)).toEqual({ ok: false, issue: 'notWholeNumber' })
  })

  it('rejects negative days — 0 is legal, so only negatives fall below the bound', () => {
    expect(checkAuditRetentionDays(-1)).toEqual({ ok: false, issue: 'belowMin' })
    expect(checkAuditRetentionDays(-30)).toEqual({ ok: false, issue: 'belowMin' })
  })

  it('rejects above the ten-year maximum', () => {
    expect(checkAuditRetentionDays(ADMIN_AUDIT_MAX_RETENTION_DAYS + 1)).toEqual({
      ok: false,
      issue: 'aboveMax',
    })
  })
})

describe('auditRetentionIssueMessageKey', () => {
  it('maps every issue to a distinct, existing message in both locales', () => {
    const keys = ALL_ISSUES.map((issue) =>
      auditRetentionIssueMessageKey(issue).replace('generalSettings.auditRetention.', ''),
    )
    // Distinct: two issues sharing one message would hide which bound broke.
    expect(new Set(keys).size).toBe(ALL_ISSUES.length)
    for (const key of keys) {
      expect(messagesByLocale['zh-CN'][key], `zh-CN.auditRetention.${key}`).toBeTruthy()
      expect(messagesByLocale.en[key], `en.auditRetention.${key}`).toBeTruthy()
    }
  })
})

describe('auditRetentionFormFromSetting', () => {
  it('projects the GET payload into the editable form state', () => {
    expect(auditRetentionFormFromSetting({ retention_days: 180 })).toEqual({ days: 180 })
    expect(auditRetentionFormFromSetting({ retention_days: 0 })).toEqual({ days: 0 })
  })
})

describe('auditRetentionSavePlan', () => {
  it('assembles the PUT payload with the CAS version when the retention is valid', () => {
    expect(auditRetentionSavePlan({ days: 90 }, 7)).toEqual({
      ok: true,
      payload: { retention_days: 90, version: 7 },
    })
    expect(auditRetentionSavePlan({ days: 0 }, 2)).toEqual({
      ok: true,
      payload: { retention_days: 0, version: 2 },
    })
  })

  it('blocks the save and reports the issue for every invalid retention shape', () => {
    const drafts: { days: number | null; issue: AuditRetentionIssue }[] = [
      { days: null, issue: 'empty' },
      { days: 1.5, issue: 'notWholeNumber' },
      { days: -1, issue: 'belowMin' },
      { days: 3651, issue: 'aboveMax' },
    ]
    for (const { days, issue } of drafts) {
      expect(auditRetentionSavePlan({ days }, 3)).toEqual({ ok: false, issue })
    }
  })
})
