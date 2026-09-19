import { describe, expect, it } from 'vitest'

import {
  filterCCSwitchCompatibleKeys,
  planCCSwitchModelChoices,
  type CCSwitchCatalogModel,
  type CCSwitchKeyRow,
} from './ccswitchExport'

function catalogRow(id: number, name: string, running_status: string): CCSwitchCatalogModel {
  return { id, name, running_status }
}

function keyRow(over: Partial<CCSwitchKeyRow> & { id: number }): CCSwitchKeyRow {
  return {
    key_prefix: `sk-yr-prefix${over.id}`,
    remark: '',
    owner_username: 'alice',
    display_status: 'active',
    allow_all_models: false,
    model_ids: [],
    ...over,
  }
}

const CATALOG = [
  catalogRow(1, 'glm-4.7', 'available'),
  catalogRow(2, 'kling-video', 'unavailable'),
  catalogRow(3, 'gpt-5-mini', 'available'),
]

describe('planCCSwitchModelChoices', () => {
  it('uses discovered names for admins, annotated with catalog availability', () => {
    const plan = planCCSwitchModelChoices({
      discovered: ['kling-video', 'glm-4.7'],
      catalog: CATALOG,
      key: { allow_all_models: false, model_ids: [1, 2] },
    })
    expect(plan).toEqual({
      mode: 'select',
      choices: [
        { name: 'kling-video', available: false },
        { name: 'glm-4.7', available: true },
      ],
      // First catalog-available discovered name wins over plain order.
      preselect: 'glm-4.7',
    })
  })

  it('keeps discovery order for members, with availability unknown', () => {
    const plan = planCCSwitchModelChoices({
      discovered: ['glm-4.7', 'kling-video'],
      catalog: null,
      key: { allow_all_models: false, model_ids: [] },
    })
    expect(plan).toEqual({
      mode: 'select',
      choices: [
        { name: 'glm-4.7', available: null },
        { name: 'kling-video', available: null },
      ],
      preselect: 'glm-4.7',
    })
  })

  it('preselects the first choice when no discovered name is catalog-available', () => {
    const plan = planCCSwitchModelChoices({
      discovered: ['kling-video'],
      catalog: CATALOG,
      key: { allow_all_models: false, model_ids: [2] },
    })
    expect(plan).toMatchObject({ mode: 'select', preselect: 'kling-video' })
  })

  it('falls back to the full catalog for an allow-all key when discovery fails', () => {
    const plan = planCCSwitchModelChoices({
      discovered: null,
      catalog: CATALOG,
      key: { allow_all_models: true, model_ids: [] },
    })
    expect(plan).toMatchObject({
      mode: 'select',
      choices: [
        { name: 'glm-4.7', available: true },
        { name: 'kling-video', available: false },
        { name: 'gpt-5-mini', available: true },
      ],
      preselect: 'glm-4.7',
    })
  })

  it('scopes the catalog fallback to the key model_ids otherwise', () => {
    const plan = planCCSwitchModelChoices({
      discovered: null,
      catalog: CATALOG,
      key: { allow_all_models: false, model_ids: [2, 3, 99] },
    })
    expect(plan).toMatchObject({
      mode: 'select',
      choices: [
        { name: 'kling-video', available: false },
        { name: 'gpt-5-mini', available: true },
      ],
      // Only catalog availability breaks the tie — 99 matches no row.
      preselect: 'gpt-5-mini',
    })
  })

  it('degrades to manual entry when discovery fails and no catalog is readable', () => {
    expect(
      planCCSwitchModelChoices({
        discovered: null,
        catalog: null,
        key: { allow_all_models: false, model_ids: [1] },
      }),
    ).toEqual({ mode: 'manual' })
  })

  it('degrades to manual entry when discovery fails and the key scope is empty', () => {
    expect(
      planCCSwitchModelChoices({
        discovered: null,
        catalog: CATALOG,
        key: { allow_all_models: false, model_ids: [] },
      }),
    ).toEqual({ mode: 'manual' })
  })

  it('treats an empty discovery result like a failed one', () => {
    expect(
      planCCSwitchModelChoices({
        discovered: [],
        catalog: null,
        key: { allow_all_models: false, model_ids: [1] },
      }),
    ).toEqual({ mode: 'manual' })
  })
})

describe('filterCCSwitchCompatibleKeys', () => {
  const MODEL_ID = 7

  it('keeps own, active, in-scope keys and drops each violation', () => {
    const rows = [
      keyRow({ id: 1, allow_all_models: true }),                              // ✓ allow-all
      keyRow({ id: 2, model_ids: [MODEL_ID] }),                               // ✓ scoped
      keyRow({ id: 3, model_ids: [99] }),                                     // ✗ scope
      keyRow({ id: 4, allow_all_models: true, owner_username: 'bob' }),       // ✗ owner
      keyRow({ id: 5, allow_all_models: true, display_status: 'revoked' }),   // ✗ status
      keyRow({ id: 6, allow_all_models: true, display_status: 'expired' }),   // ✗ status
      keyRow({ id: 8, allow_all_models: true, display_status: 'budget_exhausted' }), // ✗ status
    ]
    expect(filterCCSwitchCompatibleKeys(rows, MODEL_ID, 'alice').map((k) => k.id)).toEqual([1, 2])
  })

  it('matches the owner by exact username', () => {
    const rows = [keyRow({ id: 1, owner_username: 'alice2', allow_all_models: true })]
    expect(filterCCSwitchCompatibleKeys(rows, MODEL_ID, 'alice')).toEqual([])
  })

  it('returns empty when nothing qualifies', () => {
    expect(filterCCSwitchCompatibleKeys([], MODEL_ID, 'alice')).toEqual([])
  })
})
