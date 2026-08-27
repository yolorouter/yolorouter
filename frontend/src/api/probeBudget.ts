// Browser request budgets for the endpoints that probe an upstream provider.
//
// Every value here is DERIVED from the server's own per-call bound instead of
// being written out as a number, because the two are not independent: whoever
// gives up first decides what the operator sees. When the browser gives up
// first, the server keeps running to completion — so a probe that the server
// classified perfectly (including the timeout category, which exists to
// explain exactly this kind of slow upstream) never reaches the UI, and any
// endpoint that also WRITES has already written by then. The operator reads a
// failure, retries, and hits a duplicate.

// SERVER_PROBE_BUDGET_MS mirrors providerClientTimeout in
// internal/service/provider_client.go. Change it there and it must change
// here.
export const SERVER_PROBE_BUDGET_MS = 60_000

// CLIENT_MARGIN_MS covers what the server does around the probe itself
// (decrypting the key, the CAS commit) plus request/response transit. It is
// deliberately generous: waiting too long costs an operator some patience,
// while aborting too early on a mutating endpoint costs them a phantom
// failure on work that actually succeeded.
const CLIENT_MARGIN_MS = 15_000

// SINGLE_PROBE_BUDGET_MS is for stateless preview calls that probe exactly
// one destination and persist nothing (the model-catalogue previews; the
// key-test preview probes every configured destination and sizes itself with
// keyTestBudgetMs below instead).
export const SINGLE_PROBE_BUDGET_MS = SERVER_PROBE_BUDGET_MS + CLIENT_MARGIN_MS

// keyTestBudgetMs sizes a single key's test. verifyKeyAllDestinations walks
// the key's destinations SEQUENTIALLY, so the cost is one full server budget
// per destination — see verificationDestinationCount for where the count
// comes from.
export function keyTestBudgetMs(destinationCount: number): number {
  return Math.max(1, destinationCount) * SERVER_PROBE_BUDGET_MS + CLIENT_MARGIN_MS
}

// SERVER_BATCH_TEST_BUDGET_MS mirrors providerBatchTestBudget in
// internal/service/provider_service.go. The batch endpoint bounds itself
// rather than running for keys × destinations × the per-probe cap, so this
// budget is a flat number: whatever the server could not reach comes back
// marked not_run, and it never runs longer than this regardless of how many
// keys or endpoints the provider has.
export const SERVER_BATCH_TEST_BUDGET_MS = 5 * 60_000

// BATCH_TEST_BUDGET_MS is what the browser waits for a batch test. It no
// longer depends on key or destination counts — the server's own bound is
// what makes that possible.
export const BATCH_TEST_BUDGET_MS = SERVER_BATCH_TEST_BUDGET_MS + CLIENT_MARGIN_MS

// CANDIDATE_PROBE_BUDGET_MS sizes a candidate save/retest, whose server side
// is two sequential rounds: the basic mapping probe, then the two capability
// probes concurrently. Aborting this one is the worst case of all — the
// candidate row is already stored by the time probing starts, so a premature
// abort makes the operator's retry fail on the duplicate-mapping constraint.
export const CANDIDATE_PROBE_BUDGET_MS = 2 * SERVER_PROBE_BUDGET_MS + CLIENT_MARGIN_MS
