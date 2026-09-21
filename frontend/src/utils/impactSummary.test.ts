// @vitest-environment happy-dom
// (impactSummary transitively imports the API client, whose i18n side
// effects touch localStorage at module load — absent in the node env.)
import { describe, expect, it } from 'vitest'
import type { ModelImpact } from '../api/models'
import type { ProviderImpact } from '../api/providers'
import { modelDeleteImpactView, providerDeleteImpactView } from './impactSummary'

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

function makeModelImpact(overrides: Partial<ModelImpact> = {}): ModelImpact {
  return {
    allowlisted_keys: [],
    allow_all_key_count: 0,
    candidate_count: 0,
    recent_request_count: 0,
    recent_window_days: 7,
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

describe('modelDeleteImpactView', () => {
  it('leads with the cascade candidate count, then keys, allow-all, traffic', () => {
    const view = modelDeleteImpactView(
      makeModelImpact({
        candidate_count: 4,
        allowlisted_keys: [{ id: 1, remark: 'prod', key_prefix: 'sk-a' }],
        allow_all_key_count: 2,
        recent_request_count: 12,
        recent_window_days: 7,
      }),
      t,
    )
    expect(view.lines[0]).toContain('models.deleteModelCandidates')
    expect(view.lines[0]).toContain('"count":4')
    expect(view.lines[1]).toContain('models.impactKeys ')
    expect(view.lines[1]).toContain('prod')
    expect(view.lines[2]).toContain('models.impactAllowAll')
    expect(view.lines[2]).toContain('"count":2')
    expect(view.lines[3]).toContain('models.impactTraffic')
    expect(view.lines[3]).toContain('"count":12')
    expect(view.lines[3]).toContain('"days":7')
  })

  it('folds key names beyond five into a +N suffix', () => {
    const keys = Array.from({ length: 7 }, (_, i) => ({ id: i, remark: `k${i}`, key_prefix: 'sk-x' }))
    const view = modelDeleteImpactView(makeModelImpact({ allowlisted_keys: keys }), t)
    const keysLine = view.lines.find((l) => l.includes('models.impactKeys '))!
    expect(keysLine).toContain('k4')
    expect(keysLine).toContain('(+2)')
    expect(keysLine).not.toContain('k5')
  })

  it('says no key allowlists the model instead of an empty list', () => {
    const view = modelDeleteImpactView(makeModelImpact(), t)
    expect(view.lines.some((l) => l.includes('models.impactKeysNone'))).toBe(true)
    // A zero allow-all count stays silent — there is nothing to warn about.
    expect(view.lines.some((l) => l.includes('models.impactAllowAll'))).toBe(false)
  })

  it('carries the fixed retention note', () => {
    const view = modelDeleteImpactView(makeModelImpact(), t)
    expect(view.historyNote).toContain('models.deleteModelHistoryNote')
  })
})
