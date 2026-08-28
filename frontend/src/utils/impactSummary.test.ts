// @vitest-environment happy-dom
// (impactSummary transitively imports the API client, whose i18n side
// effects touch localStorage at module load — absent in the node env.)
import { describe, expect, it } from 'vitest'
import type { ProviderImpact } from '../api/providers'
import { providerDeleteImpactView } from './impactSummary'

// Identity-with-values t: makes both the chosen keys and the interpolated
// numbers visible in the projected lines.
const t = (key: string, values?: Record<string, unknown>) =>
  values ? `${key} ${JSON.stringify(values)}` : key

function makeImpact(overrides: Partial<ProviderImpact>): ProviderImpact {
  return {
    models: [],
    affected_keys: [],
    allow_all_key_count: 0,
    key_count: 0,
    candidate_count: 0,
    ...overrides,
  }
}

describe('providerDeleteImpactView', () => {
  it('leads with the cascade counts', () => {
    const view = providerDeleteImpactView(makeImpact({ key_count: 3, candidate_count: 7 }), t)
    expect(view.lines[0]).toContain('providers.deleteProviderCascadeCounts')
    expect(view.lines[0]).toContain('"keys":3')
    expect(view.lines[0]).toContain('"candidates":7')
  })

  it('reports zero stranded models as non-severe', () => {
    const view = providerDeleteImpactView(
      makeImpact({
        models: [{ id: 1, name: 'survivor', no_other_routable_source: false }],
        key_count: 1,
        candidate_count: 1,
      }),
      t,
    )
    expect(view.strandedCount).toBe(0)
    expect(view.lines.some((l) => l.includes('providers.impactModels '))).toBe(true)
  })

  it('counts stranded models for the escalation signal and names them', () => {
    const view = providerDeleteImpactView(
      makeImpact({
        models: [
          { id: 1, name: 'stranded-a', no_other_routable_source: true },
          { id: 2, name: 'survivor', no_other_routable_source: false },
          { id: 3, name: 'stranded-b', no_other_routable_source: true },
        ],
        key_count: 2,
        candidate_count: 3,
      }),
      t,
    )
    expect(view.strandedCount).toBe(2)
    expect(view.lines.some((l) => l.includes('providers.impactStranded'))).toBe(true)
  })
})
