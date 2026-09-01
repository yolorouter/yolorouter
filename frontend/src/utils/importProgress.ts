import { CANDIDATE_STATUS_ENABLED, VERIFICATION_FAILED, VERIFICATION_PASSED, VERIFICATION_UNTESTED } from '../api/candidateStatus'
import type { ImportItemResult, ProviderCandidate } from '../api/models'

// How one mapping reads on a progress/status view. 'pending' means the queue
// still owes it a probe (queued or in flight — the row's queue_state tells
// which) or nothing has ever probed it. 'inconclusive' means a probe DID run
// but produced no verdict — auth, rate-limit, upstream errors — recognizable
// by last_tested_at being stamped while the verdict stays untested AND the
// queue holding nothing for it. Inconclusive is terminal for progress
// purposes: waiting on it would wait forever. The queue check comes first
// because a re-import can requeue an inconclusive mapping, whose stale
// last_tested_at must not make the view call it done mid-probe.
// 'idle' is the fifth, terminal answer for a row NO ONE owes anything: never
// probed, not queued anywhere, and without the auto-enable promise — a manual
// save-as-disabled row, or one whose promise a retarget revoked. Waiting on
// it would poll forever; it renders with the caller's untested fallback.
export type CandidateProgressState = 'passed' | 'failed' | 'pending' | 'inconclusive' | 'idle'

export function candidateProgressState(
  // queue_state is optional so single-candidate API results (which carry no
  // queue stamp) can be classified too; absent means "queue holds nothing".
  // auto_enable_on_pass is optional the same way; absent means "no standing
  // promise" ON PURPOSE — unlike report_applied, an absent promise is NOT
  // normalized to true for older servers: a server without the field also
  // has nothing that would ever settle such a row, so assuming a promise
  // would poll it forever. Rows an old server is actively working still show
  // through queue_state, which is checked first.
  c: Pick<ProviderCandidate, 'verification_status' | 'management_status' | 'last_tested_at'> &
    Partial<Pick<ProviderCandidate, 'queue_state' | 'auto_enable_on_pass'>>,
): CandidateProgressState {
  // A row a worker is ON is never done, whatever its columns say right now:
  // the worker commits the verdict and the enablement in separate writes, and
  // a poll between them would otherwise freeze the view on "Passed · Disabled"
  // moments before the enable lands.
  if (c.queue_state === 'probing') return 'pending'
  // A passed ENABLED row is done. Passed but DISABLED is not done while
  // something still owes it the enable: the local queue holding it (a
  // re-import during an active probe records the pass and defers the enable
  // to the queued rerun), or the standing auto-enable promise — which a queue
  // on ANOTHER instance can fulfill, invisible to this response's
  // queue_state. Settling here would freeze the view on "Passed · Disabled"
  // moments before that enable lands.
  if (c.verification_status === VERIFICATION_PASSED) {
    if (c.management_status !== CANDIDATE_STATUS_ENABLED && (c.queue_state === 'queued' || c.auto_enable_on_pass === true)) return 'pending'
    return 'passed'
  }
  // The queue outranks a failure: the worker skips ONLY passed rows, so a
  // queued mapping a manual retest just failed will still be probed — and may
  // pass and enable. Settling on 'failed' now would stop the progress view on
  // a verdict the queue is about to revisit.
  if (c.queue_state === 'queued') return 'pending'
  if (c.verification_status === VERIFICATION_FAILED) return 'failed'
  if (c.last_tested_at !== null) return 'inconclusive'
  // Never probed and not queued here: pending only while the auto-enable
  // promise stands (some instance's queue owes the probe). Without it, no one
  // does — the row was saved unprobed on purpose, or a retarget revoked the
  // promise — and waiting would poll forever.
  return c.auto_enable_on_pass === true ? 'pending' : 'idle'
}

// The shared pace for every candidate-progress poller (the import dialog and
// the detail page takeover): one definition, so the two loops genuinely
// cannot drift apart.
export const PROGRESS_POLL_BASE_MS = 1500
export const PROGRESS_POLL_BACKOFF_CAP_MS = 30_000

// candidateIsOwedWork reports whether some queue still owes this row a probe
// or an enable: it is visibly held by the local queue, or carries the armed
// promise (which any instance's queue — or startup recovery — will fulfill).
// The shared definition of "worth watching": the detail page adopts these
// rows for polling, and the import dialog uses it to reconcile batches whose
// HTTP response was lost after the server committed them.
export function candidateIsOwedWork(
  c: Pick<ProviderCandidate, 'verification_status' | 'management_status' | 'last_tested_at'> &
    Partial<Pick<ProviderCandidate, 'queue_state' | 'auto_enable_on_pass'>>,
): boolean {
  if (c.queue_state === 'queued' || c.queue_state === 'probing') return true
  if (c.auto_enable_on_pass !== true) return false
  // Armed: owed a probe while untested-unstamped, owed the enable while
  // Passed but still off.
  if (c.verification_status === VERIFICATION_UNTESTED && c.last_tested_at === null) return true
  return c.verification_status === VERIFICATION_PASSED && c.management_status !== CANDIDATE_STATUS_ENABLED
}

// Which badge a mapping with no verdict yet gets: its live queue position when
// the queue holds it ("queued" / "probing"), else the caller's context label
// (an import in flight says "pending probe", a settled list says "untested").
export function pendingStateBadge(
  c: Pick<ProviderCandidate, 'queue_state'>,
  fallback: { labelKey: string; type: 'info' | 'default' },
): { labelKey: string; type: 'info' | 'default' } {
  if (c.queue_state === 'probing') return { labelKey: 'models.importStateProbing', type: 'info' }
  if (c.queue_state === 'queued') return { labelKey: 'models.importStateQueued', type: 'default' }
  return fallback
}

// What a status badge may honestly claim. 'passed' implies "enabled and
// routable", so a mapping an admin switched off after it passed must demote to
// 'passed_disabled' — the verdict stands, the routing promise does not.
export type CandidateDisplayState = CandidateProgressState | 'passed_disabled'

export function candidateDisplayState(
  c: Pick<ProviderCandidate, 'verification_status' | 'management_status' | 'last_tested_at' | 'queue_state'> &
    Partial<Pick<ProviderCandidate, 'auto_enable_on_pass'>>,
): CandidateDisplayState {
  const state = candidateProgressState(c)
  if (state === 'passed' && c.management_status !== CANDIDATE_STATUS_ENABLED) return 'passed_disabled'
  return state
}

// A mapping with every price slot zero or empty gets the "unpriced" badge —
// informational only (cost accounting bills it as 0), never a routing gate.
export function isUnpriced(
  c: Pick<ProviderCandidate, 'input_price' | 'output_price' | 'cache_write_price' | 'cache_read_price'>,
): boolean {
  return !c.input_price && !c.output_price && !c.cache_write_price && !c.cache_read_price
}

// The skipped import rows worth reading in the progress view: an "exists"
// skip is routine (the mapping is already there), but "invalid" and
// "modality_mismatch" name something the admin must fix for the row to ever
// import. Rows that came away with a candidate are not skips at all — they
// are being probed like any other mapping.
export function skipsWorthReading(items: ImportItemResult[]): ImportItemResult[] {
  return items.filter((it) => it.status === 'skipped' && !it.candidate_id && it.reason !== 'exists')
}

export interface ImportProgress {
  total: number
  passed: number
  failed: number
  // Probes that ran without reaching a verdict (see CandidateProgressState).
  // Counted as completed — their diagnostics are on the rows, and a manual
  // retest is the only thing that can move them.
  inconclusive: number
  pending: number
  done: boolean
}

// summarizeImportProgress narrows a provider's full candidate list to the
// mappings one import stored (in the order they were imported) and tallies
// their verification states. Ids that vanished from the list (deleted while
// probing) simply drop out of the tally rather than pinning done at false.
export function summarizeImportProgress(
  list: ProviderCandidate[],
  importedIds: number[],
): { progress: ImportProgress; rows: ProviderCandidate[] } {
  const byID = new Map(list.map((c) => [c.candidate_id, c]))
  const rows: ProviderCandidate[] = []
  for (const id of importedIds) {
    const c = byID.get(id)
    if (c) rows.push(c)
  }
  const progress: ImportProgress = { total: rows.length, passed: 0, failed: 0, inconclusive: 0, pending: 0, done: false }
  for (const c of rows) {
    const state = candidateProgressState(c)
    if (state === 'passed') progress.passed++
    else if (state === 'failed') progress.failed++
    // idle joins the inconclusive bucket: both are terminal without a
    // verdict, and neither may hold the batch open — an idle row's promise
    // was revoked, so nothing will ever move it on its own.
    else if (state === 'inconclusive' || state === 'idle') progress.inconclusive++
    else progress.pending++
  }
  progress.done = progress.pending === 0
  return { progress, rows }
}
