package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/decision"
	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// TestReportKernelFactStampsProvenanceAndFolds pins the one-call contract of
// reportKernelFact: the entry it appends carries kernel provenance and the
// exchange's current position, and the verdict it returns is the decision
// table's reading of the reported fact. A site that reports through it
// cannot file a kernel judgement under an empty — or someone else's — name.
func TestReportKernelFactStampsProvenanceAndFolds(t *testing.T) {
	rc := &Exchange{attempts: make([]AttemptRecord, 2)}
	rc.attempt.BeginCandidate(&model.ModelCandidate{ID: 77})
	rc.attempt.BindProvider(&model.Provider{ID: 42})

	got := reportKernelFact(rc, fact.Fact{Kind: fact.KindUpstreamTransportFailure})

	entries := rc.timeline.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 timeline entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Reporter != kernelReporter {
		t.Errorf("reporter = %q, want %q — the stamp is what the helper exists for", e.Reporter, kernelReporter)
	}
	if e.Attempt != 2 {
		t.Errorf("attempt = %d, want 2 (the attempt about to be recorded)", e.Attempt)
	}
	if e.Candidate != 77 || e.Provider != 42 {
		t.Errorf("candidate/provider = %d/%d, want 77/42", e.Candidate, e.Provider)
	}

	// A fact that arrives pre-attributed must still be filed as the
	// kernel's: Report only fills an empty Reporter, so without the
	// helper's overwrite the stray name would survive onto the entry.
	rcPre := &Exchange{}
	reportKernelFact(rcPre, fact.Fact{Kind: fact.KindUpstreamTransportFailure, Reporter: "someone-else"})
	pre := rcPre.timeline.All()
	if len(pre) != 1 {
		t.Fatalf("want 1 timeline entry for the pre-attributed fact, got %d", len(pre))
	}
	if pre[0].Reporter != kernelReporter {
		t.Errorf("pre-attributed fact filed under %q, want %q", pre[0].Reporter, kernelReporter)
	}

	// The verdict must match the fold of the reported fact on EVERY field —
	// a helper that resolved the wrong sink, or rebuilt the verdict keeping
	// only the fields this test happens to name, must not stay green. The
	// oracle is the decision package's own single-batch fold, not a
	// re-assembly of the sink's internals.
	fold := func(f fact.Fact) decision.Resolved {
		f.Reporter = kernelReporter
		return decision.Combine(decision.Resolved{}, decision.ResolveBatch([]fact.Fact{f}))
	}
	if want := fold(fact.Fact{Kind: fact.KindUpstreamTransportFailure}); got != want {
		t.Errorf("transport verdict = %+v, want %+v", got, want)
	}

	// A second fact whose row carries caller-facing fields (a fixed status
	// and a persisted reason): the budget-exhausted verdict is turned into
	// the caller's status and fail reason at the end of the candidate
	// chain, so a dropped field here reaches the wire.
	budget := fact.Fact{Kind: fact.KindRequestBudgetExhausted, Reason: "request_budget_exhausted"}
	if gotBudget, want := reportKernelFact(&Exchange{}, budget), fold(budget); gotBudget != want {
		t.Errorf("budget verdict = %+v, want %+v", gotBudget, want)
	}
}

// TestSettlementNotesCarryKernelProvenance pins the settlement half of the
// same contract: the DeliveryObserved entry a settlement notes is the
// kernel's own record of how the request ended, so it must carry the kernel's
// name — not the empty reporter an unstamped sink would leave. Both paths
// that note it are covered: a settlement with no delivery attempt (settle),
// and a delivered response (recordAndSettle), driven end to end.
func TestSettlementNotesCarryKernelProvenance(t *testing.T) {
	observedReporter := func(t *testing.T, rc *Exchange) string {
		t.Helper()
		reporter := ""
		found := false
		for _, e := range rc.timeline.All() {
			if _, ok := e.Record.(fact.DeliveryObserved); ok {
				reporter = e.Reporter
				found = true
			}
		}
		if !found {
			t.Fatal("no DeliveryObserved entry on the timeline; nothing settled the exchange")
		}
		return reporter
	}

	t.Run("settle path", func(t *testing.T) {
		rc := &Exchange{requestID: "req-provenance"}
		captureWarnings(t, func() {
			(&Service{}).settle(rc, callerGone("client_disconnected"), time.Now(), settleOptions{})
		})
		if got := observedReporter(t, rc); got != kernelReporter {
			t.Errorf("settle path reporter = %q, want %q", got, kernelReporter)
		}
	})

	t.Run("delivery path", func(t *testing.T) {
		db := testutil.NewSQLiteDB(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
		}))
		defer upstream.Close()

		svc := newSvc(t, db)
		p := createProvider(t, db, "p1", upstream.URL)
		createProviderKey(t, db, svc.masterKey, p.ID, "sk-1", "k1", 1, true)
		m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
		apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

		var captured *Exchange
		testHookHandleDone = func(rc *Exchange) { captured = rc }
		defer func() { testHookHandleDone = nil }()

		c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		svc.Handle(c, apiKey)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		if captured == nil {
			t.Fatal("testHookHandleDone was never invoked")
		}
		if got := observedReporter(t, captured); got != kernelReporter {
			t.Errorf("delivery path reporter = %q, want %q", got, kernelReporter)
		}
	})
}
