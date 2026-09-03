import { describe, expect, it } from 'vitest'
import type { ImportItemResult, ProviderCandidate } from '../api/models'
import { candidateDisplayState, candidateIsOwedWork, candidateProgressState, isUnpriced, pendingStateBadge, skipsWorthReading, summarizeImportProgress } from './importProgress'

// Full-shape ProviderCandidate factory: the return type pins the fixture to
// the API contract, so a newly required field breaks HERE, at the factory,
// instead of at every call site the fixture feeds.
const cand = (
  id: number,
  verification: number,
  management: number,
  error: string | null = null,
  testedAt: string | null = null,
  queueState: '' | 'queued' | 'probing' = '',
): ProviderCandidate => ({
  candidate_id: id,
  model_id: id,
  model_name: `m-${id}`,
  provider_model_name: `m-${id}`,
  input_price: 1,
  output_price: 2,
  cache_write_price: null,
  cache_read_price: null,
  billing_mode: 'token',
  image_pricing_tiers: null,
  video_pricing_tiers: null,
  max_output: 0,
  management_status: management,
  verification_status: verification,
  last_test_result: null,
  last_tested_at: testedAt,
  last_test_error: error,
  queue_state: queueState,
  auto_enable_on_pass: false,
})

describe('candidateProgressState', () => {
  it('maps verification outcomes to display states', () => {
    expect(candidateProgressState(cand(1, 1, 1))).toBe('passed')
    expect(candidateProgressState(cand(2, 2, 2))).toBe('failed')
    // Untested, unstamped, unqueued and WITH the probe promise: pending.
    expect(candidateProgressState({ ...cand(3, 0, 2), auto_enable_on_pass: true })).toBe('pending')
  })

  it('treats an unqueued untested row whose promise was revoked as idle, not pending', () => {
    // A retarget revokes the auto-enable promise along with the verification
    // it resets, and nothing re-enqueues the renamed row — waiting on it
    // would poll forever. Same shape as a manual save-as-disabled row: no
    // one owes it a probe.
    expect(candidateProgressState(cand(3, 0, 2))).toBe('idle')
  })

  it('treats an attempted probe without a verdict as terminal, not pending', () => {
    // A probe that ran but was inconclusive (auth/rate-limit/upstream error)
    // stamps last_tested_at while leaving the verdict untested; the progress
    // view must not wait on it forever.
    expect(candidateProgressState(cand(4, 0, 2, 'HTTP 400: bad request', '2026-08-20T12:00:00Z'))).toBe('inconclusive')
  })

  it('keeps a queued mapping pending even after a manual retest failed it', () => {
    // The worker skips only already-passed rows, so a queued mapping that a
    // manual retest just failed WILL still be probed — and may pass and
    // enable. Calling it failed now would stop the progress view on a verdict
    // the queue is about to revisit. A passed QUEUED row stays passed: the
    // worker skips those outright, so there is nothing to wait for.
    expect(candidateProgressState(cand(7, 2, 2, 'boom', '2026-08-20T12:00:00Z', 'queued'))).toBe('pending')
    expect(candidateProgressState(cand(8, 1, 1, null, '2026-08-20T12:00:00Z', 'queued'))).toBe('passed')
  })

  it('keeps a Passed+Disabled row still carrying the auto-enable promise pending, even unqueued', () => {
    // The promise can be fulfilled by a queue on ANOTHER instance, whose
    // queue_state this response cannot see. Settling on 'passed' here would
    // freeze the view on "Passed · Disabled" moments before the remote rerun
    // enables the row.
    const row = { ...cand(11, 1, 2, null, '2026-08-20T12:00:00Z', ''), auto_enable_on_pass: true }
    expect(candidateProgressState(row)).toBe('pending')
  })

  it('keeps a queued Passed+Disabled row pending until the queue releases it', () => {
    // A re-import that lands during an active probe can leave the pass
    // recorded but its enable deferred to the queued rerun, which fulfills it
    // without probing again. Settling on 'passed' now would freeze the view
    // on "Passed · Disabled" moments before the rerun enables the row. A
    // passed ENABLED row that is queued stays passed — the rerun has nothing
    // left to deliver (asserted above).
    expect(candidateProgressState(cand(10, 1, 2, null, '2026-08-20T12:00:00Z', 'queued'))).toBe('pending')
  })

  it('keeps a row a worker is on pending, even one already showing passed', () => {
    // The worker commits the pass and then enables in a separate write; a poll
    // in that gap sees Passed+Disabled with the worker still on the row.
    // Settling there would freeze the view on "Passed · Disabled" moments
    // before the enable lands — a probing row is not done until the worker
    // lets go of it.
    expect(candidateProgressState(cand(9, 1, 2, null, '2026-08-20T12:00:00Z', 'probing'))).toBe('pending')
  })

  it('keeps a mapping the queue still holds pending, even a requeued inconclusive one', () => {
    // A re-import can requeue a mapping whose earlier probe was inconclusive;
    // its stale last_tested_at must not make the progress view call it done
    // while a worker is literally about to (or already does) probe it.
    expect(candidateProgressState(cand(5, 0, 2, 'HTTP 400', '2026-08-20T12:00:00Z', 'queued'))).toBe('pending')
    expect(candidateProgressState(cand(6, 0, 2, 'HTTP 400', '2026-08-20T12:00:00Z', 'probing'))).toBe('pending')
  })
})

describe('candidateIsOwedWork', () => {
  it('is true exactly for rows some queue owes a probe or an enable', () => {
    // Queued/probing here, armed anywhere, or Passed+Disabled still armed.
    expect(candidateIsOwedWork(cand(1, 0, 2, null, null, 'queued'))).toBe(true)
    expect(candidateIsOwedWork(cand(2, 0, 2, null, null, 'probing'))).toBe(true)
    expect(candidateIsOwedWork({ ...cand(3, 0, 2), auto_enable_on_pass: true })).toBe(true)
    expect(candidateIsOwedWork({ ...cand(4, 1, 2, null, '2026-08-20T12:00:00Z'), auto_enable_on_pass: true })).toBe(true)
    // Cancelled order / settled rows are owed nothing.
    expect(candidateIsOwedWork(cand(5, 0, 2))).toBe(false)
    expect(candidateIsOwedWork(cand(6, 2, 2, 'boom', '2026-08-20T12:00:00Z'))).toBe(false)
    expect(candidateIsOwedWork(cand(7, 1, 1, null, '2026-08-20T12:00:00Z'))).toBe(false)
  })
})

describe('pendingStateBadge', () => {
  it('names the queue position when the queue holds the mapping, else the caller fallback', () => {
    const fallback = { labelKey: 'models.importStatePending', type: 'info' as const }
    expect(pendingStateBadge(cand(1, 0, 2, null, null, 'queued'), fallback)).toEqual({ labelKey: 'models.importStateQueued', type: 'default' })
    expect(pendingStateBadge(cand(2, 0, 2, null, null, 'probing'), fallback)).toEqual({ labelKey: 'models.importStateProbing', type: 'info' })
    expect(pendingStateBadge(cand(3, 0, 2), fallback)).toEqual(fallback)
  })
})

describe('isUnpriced', () => {
  it('is true only when every price slot is zero or empty', () => {
    expect(isUnpriced({ input_price: 0, output_price: 0, cache_write_price: null, cache_read_price: null })).toBe(true)
    expect(isUnpriced({ input_price: 0, output_price: 0, cache_write_price: 0, cache_read_price: 0 })).toBe(true)
    expect(isUnpriced({ input_price: 2, output_price: 0, cache_write_price: null, cache_read_price: null })).toBe(false)
    expect(isUnpriced({ input_price: 0, output_price: 0, cache_write_price: 0.5, cache_read_price: null })).toBe(false)
  })
})

describe('summarizeImportProgress', () => {
  it('filters to the imported mappings in import order and tallies the states', () => {
    const list = [cand(9, 1, 1), cand(5, 2, 2, 'boom'), { ...cand(7, 0, 2), auto_enable_on_pass: true }, cand(3, 1, 1)]
    const { progress, rows } = summarizeImportProgress(list, [5, 7, 9])
    expect(rows.map((r) => r.candidate_id)).toEqual([5, 7, 9])
    expect(progress).toEqual({ total: 3, passed: 1, failed: 1, inconclusive: 0, pending: 1, done: false })
  })

  it('reports done once nothing is pending, ignoring ids no longer present', () => {
    const list = [cand(1, 1, 1), cand(2, 2, 2)]
    const { progress } = summarizeImportProgress(list, [1, 2, 999])
    expect(progress).toEqual({ total: 2, passed: 1, failed: 1, inconclusive: 0, pending: 0, done: true })
  })

  it('finishes once every mapping has been attempted, counting inconclusive ones', () => {
    const list = [cand(1, 1, 1), cand(2, 0, 2, 'HTTP 400: bad request', '2026-08-20T12:00:00Z')]
    const { progress } = summarizeImportProgress(list, [1, 2])
    expect(progress).toEqual({ total: 2, passed: 1, failed: 0, inconclusive: 1, pending: 0, done: true })
  })
})

describe('candidateDisplayState', () => {
  it('demotes a passed-but-disabled mapping instead of claiming it is enabled', () => {
    expect(candidateDisplayState(cand(1, 1, 1))).toBe('passed')
    expect(candidateDisplayState(cand(2, 1, 2))).toBe('passed_disabled')
    expect(candidateDisplayState(cand(3, 2, 2))).toBe('failed')
    expect(candidateDisplayState({ ...cand(4, 0, 2), auto_enable_on_pass: true })).toBe('pending')
    expect(candidateDisplayState(cand(4, 0, 2))).toBe('idle')
  })
})

describe('skipsWorthReading', () => {
  // The backend's skip reasons (internal/service/modeladmin/import_service.go):
  // 'exists' is routine, 'invalid' and 'modality_mismatch' name admin action.
  const skip = (name: string, reason?: ImportItemResult['reason']): ImportItemResult => ({
    name,
    status: 'skipped',
    ...(reason === undefined ? {} : { reason }),
  })

  it('keeps the actionable skips and hides the routine exists skip', () => {
    const items = [skip('a', 'invalid'), skip('b', 'modality_mismatch'), skip('c', 'exists')]
    expect(skipsWorthReading(items).map((it) => it.name)).toEqual(['a', 'b'])
  })

  it('keeps a skip that carries no reason — undefined is not "exists"', () => {
    // The wire contract omits reason (omitempty), it never sends "". A
    // reasonless skip must still surface in the progress view.
    expect(skipsWorthReading([skip('a')])).toEqual([{ name: 'a', status: 'skipped' }])
  })

  it('drops rows that are not skips, whatever their reason field says', () => {
    const items: ImportItemResult[] = [
      { name: 'a', status: 'created', candidate_id: 1, model_id: 1 },
      { name: 'b', status: 'appended', candidate_id: 2, model_id: 2, reason: 'exists' },
      // No candidate_id either: on the wire created/appended rows always
      // carry one, so this row exercises the status conjunct alone.
      { name: 'c', status: 'created' },
    ]
    expect(skipsWorthReading(items)).toEqual([])
  })

  it('drops a skipped row that carries a candidate_id — it is probed, not displayed', () => {
    const items: ImportItemResult[] = [{ name: 'a', status: 'skipped', reason: 'invalid', candidate_id: 9, model_id: 9 }]
    expect(skipsWorthReading(items)).toEqual([])
  })
})
