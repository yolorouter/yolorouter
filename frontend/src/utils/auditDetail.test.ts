import { describe, expect, it } from 'vitest'
import { auditDetailLines } from './auditDetail'

// The audit page's expanded row renders whatever this projection returns;
// these tests pin the shapes the backend actually writes (settings diffs
// via DiffDetail, event context objects) plus the degradations that must
// never blank a row.
describe('auditDetailLines', () => {
  it('renders a settings diff as per-field "before → after" lines', () => {
    expect(
      auditDetailLines('{"retention_days":{"old":180,"new":90}}'),
    ).toEqual([{ field: 'retention_days', text: '180 → 90' }])
  })

  it('renders a multi-field diff in the payload\u2019s own key order', () => {
    const doc = '{"proxy_mode":{"old":"off","new":"custom"},"proxy_url":{"old":"","new":"***"}}'
    expect(auditDetailLines(doc)).toEqual([
      { field: 'proxy_mode', text: 'off → custom' },
      { field: 'proxy_url', text: ' → ***' },
    ])
  })

  it('renders event context as key: value lines (string values unquoted)', () => {
    expect(auditDetailLines('{"username":"erin"}')).toEqual([
      { field: 'username', text: 'erin' },
    ])
  })

  it('keeps non-string diff values in their unambiguous JSON form', () => {
    const doc = '{"flags":{"old":null,"new":[1,2]},"count":{"old":0,"new":3}}'
    expect(auditDetailLines(doc)).toEqual([
      { field: 'flags', text: 'null → [1,2]' },
      { field: 'count', text: '0 → 3' },
    ])
  })

  it('does not misread an object that merely carries old/new among other keys as a diff', () => {
    // Event context that happens to include extra keys stays an ordinary
    // value line — only the exact {old, new} pair is a diff entry.
    expect(auditDetailLines('{"attempt":{"old":1,"new":2,"why":"retry"}}')).toEqual([
      { field: 'attempt', text: '{"old":1,"new":2,"why":"retry"}' },
    ])
  })

  it('returns null for empty, unparseable, and non-object payloads', () => {
    expect(auditDetailLines('')).toBeNull()
    expect(auditDetailLines('not json at all')).toBeNull()
    expect(auditDetailLines('"just a string"')).toBeNull()
    expect(auditDetailLines('[1,2,3]')).toBeNull()
    expect(auditDetailLines('null')).toBeNull()
    expect(auditDetailLines('{}')).toEqual([])
  })
})
