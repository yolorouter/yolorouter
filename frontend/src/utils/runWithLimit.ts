// frontend/src/utils/runWithLimit.ts
//
// A bounded-concurrency runner: fire one async operation per item, never more
// than `limit` in flight at a time, and settle EVERY item individually — the
// batch-delete sibling of importRows' transport batching (IMPORT_BATCH_CAP /
// chunkByCap). Page components call this and summarize; they never inline the
// limiting logic.

// The models tab's batch delete runs one DELETE per selected mapping row.
// The cap bounds the fan-out: a provider carrying 100+ stale mappings must not
// open 100 simultaneous requests (the browser would queue them anyway, but on
// the shared connection pool — stalling every other call the console makes).
// 3 sits in a deliberately low band: steady progress without crowding the pool.
export const BATCH_DELETE_CONCURRENCY = 3

// How each item settled. `ok: false` carries the rejection reason; the runner
// swallows nothing silently — the caller decides what a failure means.
export type RunOutcome<T, R> =
  | { item: T; index: number; ok: true; value: R }
  | { item: T; index: number; ok: false; error: unknown }

// runWithLimit runs fn over every item with at most `limit` calls in flight.
// Every item settles: a rejection marks that item and the pool keeps pulling,
// so one bad row never aborts the batch (a partial-failure summary is the
// whole point). Results come back in input order — index i of the array is
// always items[i]'s outcome — so callers can line summaries up with rows.
// A limit below 1 is normalized to 1 rather than throwing: the caller-facing
// contract is "bounded", not "validated".
export async function runWithLimit<T, R>(
  items: readonly T[],
  limit: number,
  fn: (item: T, index: number) => Promise<R>,
): Promise<RunOutcome<T, R>[]> {
  const results = new Array<RunOutcome<T, R>>(items.length)
  // A pool of min(limit, items.length) workers shares one cursor; each worker
  // holds exactly one call in flight while it awaits, so the pool size IS the
  // in-flight ceiling.
  const workerCount = Math.min(Math.max(Math.floor(limit), 1), items.length)
  let next = 0
  const worker = async (): Promise<void> => {
    for (;;) {
      const index = next++
      if (index >= items.length) return
      const item = items[index]
      try {
        results[index] = { item, index, ok: true, value: await fn(item, index) }
      } catch (error) {
        results[index] = { item, index, ok: false, error }
      }
    }
  }
  await Promise.all(Array.from({ length: workerCount }, () => worker()))
  return results
}
