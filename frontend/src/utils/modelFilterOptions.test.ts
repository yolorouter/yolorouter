import { describe, expect, it } from 'vitest'
import { mergeModelFilterOptions } from './modelFilterOptions'

// The merge behind the analytics page's model dropdown. The history
// argument has exactly three shapes, each with its own expectation:
//   - a name in both lists        → one option (collision folds)
//   - empty history               → catalog alone
//   - null history (fetch failed) → catalog alone, nothing thrown
// Assertions are whole-array toEqual so they pin the ORDER too, not just
// membership — the merged list must not shuffle as its sources change.
describe('mergeModelFilterOptions', () => {
  it('keeps a deleted model selectable via its history-only name', () => {
    // Catalog arrives in creation order (id ASC); history arrives sorted
    // (SQL ASC). The merge must not care — output is name-sorted either way.
    expect(mergeModelFilterOptions(['zeta-new'], ['alpha-deleted', 'zeta-new'])).toEqual([
      { label: 'alpha-deleted', value: 'alpha-deleted' },
      { label: 'zeta-new', value: 'zeta-new' },
    ])
  })

  it('folds a name present in both lists into a single option', () => {
    // Same name = same model: a recreated model's history is its own, so
    // the overlap must collapse instead of offering duplicate rows.
    const merged = mergeModelFilterOptions(['gpt-4o', 'claude-3'], ['gpt-4o', 'gpt-4o', 'llama-x'])
    expect(merged).toEqual([
      { label: 'claude-3', value: 'claude-3' },
      { label: 'gpt-4o', value: 'gpt-4o' },
      { label: 'llama-x', value: 'llama-x' },
    ])
  })

  it('sorts by name regardless of the order the sources arrived in', () => {
    const one = mergeModelFilterOptions(['b', 'a'], ['c'])
    const two = mergeModelFilterOptions(['a', 'b'], ['c'])
    expect(one).toEqual(two)
    expect(one.map((o) => o.value)).toEqual(['a', 'b', 'c'])
  })

  it('degrades to catalog-only options when history is empty', () => {
    expect(mergeModelFilterOptions(['m1', 'm0'], [])).toEqual([
      { label: 'm0', value: 'm0' },
      { label: 'm1', value: 'm1' },
    ])
  })

  it('degrades to catalog-only options when the history endpoint failed', () => {
    // null is the page's signal for a failed fetch; the dropdown must keep
    // the catalog options rather than lose them all.
    expect(mergeModelFilterOptions(['kept-model'], null)).toEqual([
      { label: 'kept-model', value: 'kept-model' },
    ])
  })

  it('drops empty names', () => {
    // Legacy log rows can carry an empty model_name into the distinct
    // list; an empty option would render blank and filter nothing.
    expect(mergeModelFilterOptions(['', 'real'], ['', ''])).toEqual([
      { label: 'real', value: 'real' },
    ])
  })
})
