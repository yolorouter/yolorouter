import { describe, expect, it } from 'vitest'
import { hasKeyTestBreakdown, keyTestTargetRows, passedBreakdownVisible, VERIFICATION_STATUS_PASSED } from './keyTestTargets'
import type { KeyTestTarget } from '../api/providers'

// The identity t makes the exact i18n keys visible, so a row that silently
// stopped naming its protocol or its outcome category is a failed assertion
// rather than an empty string nobody notices.
const t = (key: string) => key

function target(overrides: Partial<KeyTestTarget> = {}): KeyTestTarget {
  return { proto: 'openai', outcome: 0, duration_ms: 12, detail: '', ...overrides }
}

describe('keyTestTargetRows', () => {
  it('projects one row per target, keeping protocol, category, duration and detail', () => {
    const rows = keyTestTargetRows(t, [
      target({ proto: 'openai', outcome: 0, duration_ms: 340, detail: '' }),
      target({ proto: 'anthropic', outcome: 7, duration_ms: 91, detail: 'HTTP 400: unsupported endpoint' }),
    ])

    expect(rows).toEqual([
      {
        protocol: 'openai',
        protocolLabel: 'providers.protocol_openai',
        outcomeLabel: 'providers.outcomeSuccess',
        passed: true,
        durationMs: 340,
        detail: '',
      },
      {
        protocol: 'anthropic',
        protocolLabel: 'providers.protocol_anthropic',
        outcomeLabel: 'providers.outcomeUpstreamError',
        passed: false,
        durationMs: 91,
        detail: 'HTTP 400: unsupported endpoint',
      },
    ])
  })

  it('degrades to no breakdown for a key tested before per-target results existed', () => {
    expect(keyTestTargetRows(t, null)).toEqual([])
    expect(keyTestTargetRows(t, undefined)).toEqual([])
    expect(keyTestTargetRows(t, [])).toEqual([])
  })

  it('labels an outcome outside the known enum as an upstream error', () => {
    const [row] = keyTestTargetRows(t, [target({ outcome: 99 })])
    expect(row.outcomeLabel).toBe('providers.outcomeUpstreamError')
    expect(row.passed).toBe(false)
  })

  it('reports an empty detail as empty so nothing renders an empty diagnostic box', () => {
    expect(keyTestTargetRows(t, [target({ detail: '' })])[0].detail).toBe('')
    expect(keyTestTargetRows(t, [target({ detail: '  \n ' })])[0].detail).toBe('')
  })
})

describe('hasKeyTestBreakdown', () => {
  it('is true when a run probed more than one destination, whichever way they went', () => {
    expect(hasKeyTestBreakdown([target({ proto: 'openai' }), target({ proto: 'anthropic' })])).toBe(true)
  })

  it('is true for a single destination that quoted its upstream', () => {
    expect(hasKeyTestBreakdown([target({ outcome: 1, detail: 'HTTP 401: invalid api key' })])).toBe(true)
  })

  it('is false for a lone destination with nothing to add to the verdict', () => {
    expect(hasKeyTestBreakdown([target({ outcome: 1 })])).toBe(false)
    expect(hasKeyTestBreakdown([target({ outcome: 1, detail: '  \n ' })])).toBe(false)
  })

  it('is false for a key tested before per-target results existed', () => {
    expect(hasKeyTestBreakdown(null)).toBe(false)
    expect(hasKeyTestBreakdown(undefined)).toBe(false)
    expect(hasKeyTestBreakdown([])).toBe(false)
  })
})

describe('passedBreakdownVisible', () => {
  const breakdown = [
    target({ proto: 'openai', detail: '' }),
    target({ proto: 'anthropic', detail: '' }),
  ]
  function row(overrides: Partial<Parameters<typeof passedBreakdownVisible>[0]> = {}) {
    return {
      needs_reentry: false,
      verification_status: VERIFICATION_STATUS_PASSED,
      last_test_result: 0,
      last_test_targets: breakdown,
      ...overrides,
    }
  }

  it('is true only for a verified key whose last test passed with a breakdown', () => {
    expect(passedBreakdownVisible(row())).toBe(true)
  })

  it('is false when verification was later revoked without a new test being recorded', () => {
    // A key the gateway demoted after watching it fail in production: the
    // stored test columns still say "passed", but the row must not present a
    // green disclosure over stale evidence.
    expect(passedBreakdownVisible(row({ verification_status: 2 }))).toBe(false)
  })

  it('is false when a new plaintext reset verification but the old test columns remain', () => {
    expect(passedBreakdownVisible(row({ verification_status: 0 }))).toBe(false)
  })

  it('is false for needs_reentry, a failed or missing last test, and a missing breakdown', () => {
    expect(passedBreakdownVisible(row({ needs_reentry: true }))).toBe(false)
    expect(passedBreakdownVisible(row({ last_test_result: 7 }))).toBe(false)
    expect(passedBreakdownVisible(row({ last_test_result: null }))).toBe(false)
    expect(passedBreakdownVisible(row({ last_test_targets: null }))).toBe(false)
  })
})
