//
// modelPricingGroups — the pricing page's pure layer, called directly (no
// Vue, no fetch). This suite pins the legacy-server arm of the group
// header's minimum: a server predating prices_declared omits the field, the
// candidateUnpriced predicate's zero-value heuristic then decides, and the
// header minimum, the warning count, and the row sort must all agree with
// that one verdict (a nonzero price is a priced row — it joins the minimum
// and sorts among the priced rows; an all-zero row is unpriced everywhere).
// The modern-server arm (prices_declared present) is pinned through the
// page suites.
import { describe, expect, it } from 'vitest'
import type { Model, ModelCandidate } from '../../api/models'
import { buildPricingGroups } from './modelPricingGroups'

// A minimal model fixture (the group builder reads id / name / candidates).
function modelFixture(id: number, name: string, candidates: ModelCandidate[]): Model {
  return {
    id,
    name,
    management_status: 1,
    running_status: 'available',
    scheduling_mode: 'failover',
    supports_image_input: null,
    output_modalities: ['text'],
    context_length: null,
    input_modalities: null,
    supports_tools: null,
    supports_reasoning: null,
    temperature: null,
    candidates,
    created_at: '2026-09-01T00:00:00Z',
  }
}

// A legacy-server candidate: prices_declared is deliberately ABSENT (the
// field is optional — a legacy server omits it, it never sends false), so
// pricing is judged by the zero-value heuristic alone.
function legacyCandidate(overrides: Partial<ModelCandidate> = {}): ModelCandidate {
  return {
    id: 1,
    provider_id: 10,
    provider_name: 'alpha',
    provider_model_name: 'alpha-up',
    input_price: 1.5,
    output_price: 2.5,
    cache_write_price: null,
    cache_read_price: null,
    max_output: 4096,
    billing_mode: 'token',
    image_pricing_tiers: null,
    video_pricing_tiers: null,
    audio_unit_price: null,
    supports_streaming: null,
    supports_function_calling: null,
    management_status: 1,
    sort_order: 1,
    verification_status: 1,
    routable: true,
    blocked_by: '',
    last_test_result: 0,
    last_test_duration_ms: null,
    last_tested_at: null,
    binding_count: 0,
    ...overrides,
  }
}

describe('modelPricingGroups legacy-server arm (no prices_declared field)', () => {
  it('a legacy token row with a nonzero price joins the header minimum; the all-zero row is unpriced everywhere', () => {
    const group = buildPricingGroups([
      modelFixture(1, 'legacy-model', [
        // Nonzero price: the heuristic calls it priced — joins the minimum,
        // carries no unpriced verdict.
        legacyCandidate({ id: 1, provider_model_name: 'priced-up', input_price: 1.5, output_price: 2.5 }),
        // All-zero: the heuristic's unpriced arm — excluded from the
        // minimum despite being a token row, counted unpriced.
        legacyCandidate({ id: 2, provider_model_name: 'zero-up', input_price: 0, output_price: 0 }),
        // Nonzero but higher, so the minimum really is the smallest.
        legacyCandidate({ id: 3, provider_model_name: 'costly-up', input_price: 3, output_price: 6 }),
      ]),
    ])
    expect(group).toHaveLength(1)
    expect(group[0].minTokenInputPrice, 'both nonzero rows join the minimum, the all-zero row does not').toBe(1.5)
    expect(group[0].unpricedCount, 'only the all-zero row counts unpriced').toBe(1)
    // The same verdict drives the row order: priced rows by input price
    // ascending, the unpriced row sunk to the bottom.
    expect(group[0].rows.map((row) => row.candidate.provider_model_name)).toEqual(['priced-up', 'costly-up', 'zero-up'])
  })

  it('a legacy group of all-zero token rows has no minimum and warns with the full count', () => {
    const group = buildPricingGroups([
      modelFixture(1, 'legacy-unpriced', [
        legacyCandidate({ id: 1, provider_model_name: 'zero-a', input_price: 0, output_price: 0 }),
        legacyCandidate({ id: 2, provider_model_name: 'zero-b', input_price: 0, output_price: 0 }),
      ]),
    ])
    expect(group[0].minTokenInputPrice, 'no priced token row, no header minimum').toBeNull()
    expect(group[0].unpricedCount, 'every all-zero row counts unpriced').toBe(2)
  })
})
