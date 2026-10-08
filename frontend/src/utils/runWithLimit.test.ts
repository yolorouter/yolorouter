// frontend/src/utils/runWithLimit.test.ts
//
// The batch-delete concurrency limiter as a pure function (node environment,
// no DOM). Covered acceptance case: TC-12 — 10 items, concurrency cap 3, two
// items reject: at no moment are more than 3 calls in flight, all 10 items
// settle (8 ok / 2 failed), and a single failure never aborts the batch.
import { describe, expect, it } from 'vitest'
import { runWithLimit } from './runWithLimit'

// A controllable task: resolves after `ms`, rejects instead when `fail` is
// set. Bookkeeping is synchronous at entry/exit so the in-flight ceiling is
// observable exactly.
function tracker() {
  let active = 0
  let maxActive = 0
  const task = (ms: number, fail = false) => async (item: number) => {
    active++
    maxActive = Math.max(maxActive, active)
    try {
      await new Promise((resolve) => setTimeout(resolve, ms))
    } finally {
      active--
    }
    if (fail) throw new Error(`boom-${item}`)
    return item * 10
  }
  return { task, maxActive: () => maxActive }
}

describe('runWithLimit', () => {
  it('TC-12: caps in-flight calls at the limit, settles every item, and survives per-item failures', async () => {
    const { task, maxActive } = tracker()
    // 10 items, items 2 and 7 reject.
    const specs = Array.from({ length: 10 }, (_, i) => (i === 2 || i === 7 ? task(5, true) : task(5)))

    const outcomes = await runWithLimit(specs, 3, (fn, index) => fn(index))

    // In-flight ceiling: never above the cap...
    expect(maxActive()).toBeLessThanOrEqual(3)
    // ...and the pool really uses its capacity (a serial runner would cap at
    // 1 and also pass the ceiling check).
    expect(maxActive()).toBe(3)

    // All 10 settle, in input order, with values/reasons in their slots.
    expect(outcomes).toHaveLength(10)
    outcomes.forEach((outcome, index) => {
      expect(outcome.index).toBe(index)
      if (index === 2 || index === 7) {
        expect(outcome.ok).toBe(false)
        if (!outcome.ok) expect((outcome.error as Error).message).toBe(`boom-${index}`)
      } else {
        expect(outcome.ok).toBe(true)
        if (outcome.ok) expect(outcome.value).toBe(index * 10)
      }
    })
    expect(outcomes.filter((o) => o.ok)).toHaveLength(8)
    expect(outcomes.filter((o) => !o.ok)).toHaveLength(2)
  })

  it('a failure does not abort the batch: items after a rejected one still run', async () => {
    const { task } = tracker()
    const specs = [
      task(1, true),
      task(1, true),
      task(1),
      task(1),
      task(1),
    ]
    const outcomes = await runWithLimit(specs, 1, (fn, index) => fn(index))
    // The two failures land first under a serial runner; the three items
    // queued behind them still ran to completion.
    expect(outcomes.map((o) => o.ok)).toEqual([false, false, true, true, true])
  })

  it('a limit above the item count simply runs everything, and an empty batch settles empty', async () => {
    const { task } = tracker()
    const outcomes = await runWithLimit([task(1), task(1)], 10, (fn, index) => fn(index))
    expect(outcomes.map((o) => o.ok)).toEqual([true, true])

    expect(await runWithLimit([], 3, async () => {})).toEqual([])
  })

  it('treats a sub-1 limit as 1 rather than throwing', async () => {
    const { task, maxActive } = tracker()
    const outcomes = await runWithLimit([task(1), task(1), task(1)], 0, (fn, index) => fn(index))
    expect(maxActive()).toBe(1)
    expect(outcomes.every((o) => o.ok)).toBe(true)
  })
})
