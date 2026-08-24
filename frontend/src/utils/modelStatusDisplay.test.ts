import { describe, expect, it } from 'vitest'
import { modelHeaderDescription } from './modelStatusDisplay'

// The header is built from plain values; anything else slipping into the
// interpolation (a ref, an object) would stringify as [object Object] and
// turn the i18n lookup into a missing key. The identity t makes the exact
// keys visible.
describe('modelHeaderDescription', () => {
  const t = (key: string) => key

  it('names the running status and the scheduling mode', () => {
    expect(modelHeaderDescription(t, 'available', false)).toBe(
      'models.runningStatusColumn: models.runningAvailable · models.schedulingMode: models.schedulingModeFailoverShort',
    )
    expect(modelHeaderDescription(t, 'degraded', true)).toBe(
      'models.runningStatusColumn: models.runningDegraded · models.schedulingMode: models.schedulingModeBalancedShort',
    )
  })

  it('falls back to unavailable for an unknown status', () => {
    expect(modelHeaderDescription(t, 'mystery', false)).toContain('models.runningUnavailable')
  })
})
