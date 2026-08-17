package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolorouter/yolorouter/internal/decision"
	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// TestFoldUpstreamDecision is the table for the pure fold: every row is one
// combination of reported opinions and repair state, and the assertions pin
// the four layers of rules the fold encodes — what the observers' verdict
// outranks, when a repair may execute, when the kernel's status-line reading
// joins, and what the routing/sticky/relabel come out as. Zero DB, zero HTTP:
// the inputs are Resolved verdicts built the only way tests can build them
// (decision.ResolveBatch over facts) plus the two repair booleans and the
// status, and the whole machine the old code needed to reach these branches
// is gone.
func TestFoldUpstreamDecision(t *testing.T) {
	refused := decision.ResolveBatch([]fact.Fact{{Kind: fact.KindPayloadRefused, Status: 400}})
	repairedVerdict := decision.ResolveBatch([]fact.Fact{{Kind: fact.KindPayloadRepairedRetrySame, Status: 400}})
	noOpinion := decision.ResolveBatch(nil)
	truncated := decision.ResolveBatch([]fact.Fact{{Kind: fact.KindUpstreamStreamTruncated, Status: 200}})

	cases := []struct {
		name string

		observed      decision.Resolved
		status        int
		hasRepair     bool
		repairAllowed bool

		wantRoute        attemptResult
		wantBaselineFold bool
		wantWarn         bool
		wantOutcome      string // "" = only assert it is NOT the relabel
		wantNote         string // "" = don't check
		wantStickyStatus int    // 0 = expect no sticky verdict held
	}{
		{
			// A body-informed verdict beats a status-informed one outright:
			// the refusal steers, the baseline never joins, and the attempt
			// row is relabelled to say the payload was judged.
			name:     "an observer verdict out-ranks the status-line baseline",
			observed: refused, status: 400, hasRepair: false, repairAllowed: true,
			wantRoute: attemptNextCandidate, wantBaselineFold: false, wantWarn: false,
			wantOutcome: AttemptContentFiltered, wantNote: "upstream 400 content inspection",
			wantStickyStatus: 400,
		},
		{
			// The historical bug this row exists for: a retry verdict with no
			// repaired body behind it degrades to the baseline — routing AND
			// sticky AND status together, not the routing alone — so a chain
			// exhausted on rate limits answers with the 429, not a 502.
			name:     "an unexecutable repair degrades to the baseline, sticky included",
			observed: repairedVerdict, status: 429, hasRepair: false, repairAllowed: true,
			wantRoute: attemptRotateKey, wantBaselineFold: true, wantWarn: true,
			wantStickyStatus: 429,
		},
		{
			name:     "no opinion folds the kernel baseline in",
			observed: noOpinion, status: 500, hasRepair: false, repairAllowed: true,
			wantRoute: attemptNextCandidate, wantBaselineFold: true, wantWarn: false,
		},
		{
			// The repair executes only when everything it needs is present;
			// the retry record carries the unrelabelled class — the
			// provider's own answer for a 400 is a client error, NOT the
			// content-filtered relabel only a refusal earns — and the plain
			// note, because the repair path describes the provider's own
			// answer, not a payload judgement.
			name:     "an executable repair retries the same candidate unrelabelled",
			observed: repairedVerdict, status: 400, hasRepair: true, repairAllowed: true,
			wantRoute: attemptRetrySame, wantBaselineFold: false, wantWarn: false,
			wantOutcome: AttemptClientError, wantNote: "upstream 400",
		},
		{
			// And a repair already spent is an answer already given: the
			// allowance is gone, so the verdict is unexecutable and the
			// baseline of a 400 terminates the chain.
			name:     "a spent repair allowance is an answer already given",
			observed: repairedVerdict, status: 400, hasRepair: true, repairAllowed: false,
			wantRoute: attemptTerminal, wantBaselineFold: true, wantWarn: true,
		},
		{
			// No body change can address a provider fault, so even a repair
			// with everything behind it does not execute on a 500.
			name:     "a non-repairable status cannot execute a repair",
			observed: repairedVerdict, status: 500, hasRepair: true, repairAllowed: true,
			wantRoute: attemptNextCandidate, wantBaselineFold: true, wantWarn: true,
		},
		{
			// Terminate is a floor: LoopCommitted (bytes already reached the
			// caller) sits above it and must land in the same bucket, so the
			// fold must never compare with ==.
			name:     "LoopCommitted lands in the terminate bucket",
			observed: truncated, status: 200, hasRepair: false, repairAllowed: true,
			wantRoute: attemptTerminal, wantBaselineFold: false, wantWarn: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := foldUpstreamDecision(tc.observed, tc.status, tc.hasRepair, tc.repairAllowed)

			if d.routing() != tc.wantRoute {
				t.Errorf("routing() = %v, want %v", d.routing(), tc.wantRoute)
			}
			if d.baselineFolded() != tc.wantBaselineFold {
				t.Errorf("baselineFolded() = %v, want %v", d.baselineFolded(), tc.wantBaselineFold)
			}
			if d.warnUnexecuted() != tc.wantWarn {
				t.Errorf("warnUnexecuted() = %v, want %v", d.warnUnexecuted(), tc.wantWarn)
			}
			if tc.wantOutcome != "" {
				if d.class().Outcome != tc.wantOutcome {
					t.Errorf("class().Outcome = %q, want %q", d.class().Outcome, tc.wantOutcome)
				}
			} else if tc.wantRoute != attemptRetrySame && d.class().Outcome == AttemptContentFiltered {
				t.Errorf("class().Outcome = %q on a non-refusal path, want no relabel", d.class().Outcome)
			}
			if tc.wantNote != "" && d.note() != tc.wantNote {
				t.Errorf("note() = %q, want %q", d.note(), tc.wantNote)
			}
			sticky, ok := d.sticky()
			if tc.wantStickyStatus == 0 {
				if ok {
					t.Errorf("sticky() held %d, want none", sticky.Status)
				}
			} else if !ok || sticky.Status != tc.wantStickyStatus {
				t.Errorf("sticky() = (%d, %v), want held status %d", sticky.Status, ok, tc.wantStickyStatus)
			}
		})
	}
}

// TestKernelBaselineTimelineEntries is the executor half of the baseline
// fold's proof. The table test above can only assert a boolean; this test
// runs the real machinery and asserts the timeline entry itself — the fold's
// joining is an audit fact with provenance, and neither the count nor the
// Reporter may drift:
//
//   - when an observer steered, the kernel files NO reading of its own (the
//     asymmetry: a body-informed verdict beats three digits outright);
//   - when nothing steered, every failed attempt carries exactly ONE kernel
//     entry, reported by "kernel" — not an empty Reporter — and filed with
//     that attempt's full provenance: its number, and the candidate and
//     provider it ran on.
func TestKernelBaselineTimelineEntries(t *testing.T) {
	run := func(t *testing.T, observe bool) (kernelEntries []fact.Entry, rc *Exchange) {
		t.Helper()
		db := testutil.NewSQLiteDB(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
		}))
		defer upstream.Close()

		svc := newSvc(t, db)
		if observe {
			// A refusal reported on a 429 steers the chain to the next
			// candidate — a body-informed verdict the baseline must lose to.
			registerVerdictObserver(svc, fact.KindPayloadRefused)
		}
		apiKey := seedTwoCandidateModel(t, svc, db, upstream.URL)

		var captured *Exchange
		testHookHandleDone = func(rc *Exchange) { captured = rc }
		t.Cleanup(func() { testHookHandleDone = nil })

		c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		svc.Handle(c, apiKey)
		if captured == nil {
			t.Fatalf("exchange was never concluded (status %d, body %s)", w.Code, w.Body.String())
		}
		for _, e := range captured.timeline.All() {
			if e.Reporter == kernelReporter && e.Fact != nil && e.Fact.Kind == fact.KindUpstreamRateLimited {
				kernelEntries = append(kernelEntries, e)
			}
		}
		return kernelEntries, captured
	}

	t.Run("a steering observer suppresses the kernel's own reading", func(t *testing.T) {
		entries, _ := run(t, true)
		if len(entries) != 0 {
			t.Fatalf("kernel baseline entries = %d, want 0 — an observer that read the body outranks the status line", len(entries))
		}
	})

	t.Run("an unsteered attempt files exactly one kernel entry with provenance", func(t *testing.T) {
		entries, rc := run(t, false)
		attempts := len(rc.attempts)
		if attempts == 0 {
			t.Fatal("no attempts were recorded; the fixture is broken")
		}
		if len(entries) != attempts {
			t.Fatalf("kernel baseline entries = %d, want one per failed attempt (%d)", len(entries), attempts)
		}
		// The entries arrive in attempt order (each is appended just before
		// its own attempt record), so entry i belongs to attempt i — and its
		// provenance must say so: the attempt number AND the candidate and
		// provider that attempt ran on, not just any non-zero values.
		for i, e := range entries {
			if e.Reporter != kernelReporter {
				t.Errorf("entry %d reporter = %q, want %q", i, e.Reporter, kernelReporter)
			}
			if e.Attempt != i {
				t.Errorf("entry %d reports attempt %d, want %d — one kernel reading per attempt, filed on that attempt", i, e.Attempt, i)
			}
			rec := rc.attempts[i]
			if e.Candidate != rec.CandidateID || e.Provider != rec.ProviderID {
				t.Errorf("entry %d provenance = (candidate %d, provider %d), want the attempt's own (candidate %d, provider %d)",
					i, e.Candidate, e.Provider, rec.CandidateID, rec.ProviderID)
			}
		}
	})
}
