package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// repairingRewriter swaps the failed body for a fixed replacement and reports
// the repair, the way an image-stripping capability would after an upstream
// refused a payload for its attachments.
type repairingRewriter struct {
	replacement []byte
	fail        bool
}

func (repairingRewriter) Name() string { return "test_repairer" }

func (r repairingRewriter) RewriteAfterFailure(_ context.Context, _ struct{}, _ protocols.ProtocolID, _ []byte, up fact.Upstream, sink fact.Sink) ([]byte, error) {
	if r.fail {
		return nil, errors.New("no repair available")
	}
	sink.Report(fact.Fact{Kind: fact.KindPayloadRepairedRetrySame, Status: up.StatusCode})
	return r.replacement, nil
}

func registerRepairer(svc *Service, replacement []byte) {
	RegisterFailureRewriter(svc, repairingRewriter{replacement: replacement},
		func(*Exchange) struct{} { return struct{}{} })
}

// chainProbeRewriter records the body it was handed and returns its own,
// so a test can see whether rewriters chain.
type chainProbeRewriter struct {
	saw *[]string
	out []byte
}

func (chainProbeRewriter) Name() string { return "chain_probe" }

func (c chainProbeRewriter) RewriteAfterFailure(_ context.Context, _ struct{}, _ protocols.ProtocolID, body []byte, up fact.Upstream, sink fact.Sink) ([]byte, error) {
	*c.saw = append(*c.saw, string(body))
	sink.Report(fact.Fact{Kind: fact.KindPayloadRepairedRetrySame, Status: up.StatusCode})
	return c.out, nil
}

// Failure rewriters chain: each sees the previous one's output, so there is
// one current body at every point rather than competing repairs.
func TestFailureRewritersChainOverOneCurrentBody(t *testing.T) {
	svc := &Service{}
	var saw []string
	RegisterFailureRewriter(svc, chainProbeRewriter{saw: &saw, out: []byte("first")},
		func(*Exchange) struct{} { return struct{}{} })
	RegisterFailureRewriter(svc, chainProbeRewriter{saw: &saw, out: []byte("second")},
		func(*Exchange) struct{} { return struct{}{} })

	repaired, _ := svc.rewriteAfterFailure(context.Background(), &Exchange{}, protocols.ProtocolOpenAI, []byte("original"), fact.Upstream{})

	if len(saw) != 2 || saw[0] != "original" || saw[1] != "first" {
		t.Fatalf("bodies seen = %v, want [original first] (second rewriter sees the first's output)", saw)
	}
	if string(repaired) != "second" {
		t.Fatalf("repaired = %q, want the last rewriter's output", repaired)
	}
}

// A repaired body goes back to the SAME candidate: the upstream refuses the
// original payload, the rewriter repairs it, and the second dispatch — same
// provider, same key — succeeds.
func TestRepairedBodyRetriesTheSameCandidate(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu     sync.Mutex
		bodies []string
		auths  []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if !bytes.Contains(body, []byte("repaired")) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"cannot process this payload"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	registerRepairer(svc, []byte(`{"model":"gpt-4o-real","messages":[{"role":"user","content":"repaired"}]}`))
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after repair retry; body = %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (original then repaired)", len(bodies))
	}
	if bytes.Contains([]byte(bodies[0]), []byte("repaired")) {
		t.Error("first dispatch already carried the repaired body")
	}
	if !bytes.Contains([]byte(bodies[1]), []byte("repaired")) {
		t.Errorf("second dispatch did not carry the repaired body: %s", bodies[1])
	}
	if auths[0] != auths[1] {
		t.Errorf("retry switched keys: %q then %q, want the same key", auths[0], auths[1])
	}
}

// countingRewriter produces a DIFFERENT body on every invocation, the shape
// of repair that genuinely loops — a deterministic repairer that reproduces
// its own output is stopped one round earlier by the unchanged-output
// abstention rule instead.
type countingRewriter struct{ n *int }

func (countingRewriter) Name() string { return "counting_repairer" }

func (r countingRewriter) RewriteAfterFailure(_ context.Context, _ struct{}, _ protocols.ProtocolID, _ []byte, up fact.Upstream, sink fact.Sink) ([]byte, error) {
	*r.n++
	sink.Report(fact.Fact{Kind: fact.KindPayloadRepairedRetrySame, Status: up.StatusCode})
	return []byte(`{"model":"gpt-4o-real","messages":[{"role":"user","content":"repair ` + string(rune('0'+*r.n)) + `"}]}`), nil
}

// One repair per candidate, and a repair that did not fix the failure is an
// answer already given: the second 400 gets its full baseline handling and
// the caller sees the upstream's own status — not a repair loop burning the
// budget into a misleading 504, and not a generic 502 from a half-executed
// retry.
func TestFailedRepairSurfacesTheUpstreamStatus(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"still cannot process this"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	var repairs int
	RegisterFailureRewriter(svc, countingRewriter{n: &repairs},
		func(*Exchange) struct{} { return struct{}{} })
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the upstream's own 400 after the failed repair; body = %s", w.Code, w.Body.String())
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2 (original plus one repair)", got)
	}
}

// A rewriter that errors is an abstention, not a verdict: the failure keeps
// the handling it would have received anyway — here a 400 surfaced terminal.
func TestFailureRewriterErrorAbstains(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	RegisterFailureRewriter(svc, repairingRewriter{fail: true},
		func(*Exchange) struct{} { return struct{}{} })
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 surfaced (rewriter abstained)", w.Code)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry without a repair)", calls)
	}
}

// echoRewriter returns exactly the bytes it was handed while still reporting
// a repair — the deterministic repairer re-running over its own output.
type echoRewriter struct{}

func (echoRewriter) Name() string { return "echo_repairer" }

func (echoRewriter) RewriteAfterFailure(_ context.Context, _ struct{}, _ protocols.ProtocolID, body []byte, up fact.Upstream, sink fact.Sink) ([]byte, error) {
	sink.Report(fact.Fact{Kind: fact.KindPayloadRepairedRetrySame, Status: up.StatusCode})
	return body, nil
}

// Output that matches the input byte for byte is an abstention whatever fact
// came with it: re-dispatching the bytes that just failed is not a repair, so
// the failure keeps its normal handling instead of burning the budget on
// identical retries.
func TestUnchangedOutputAbstains(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	RegisterFailureRewriter(svc, echoRewriter{}, func(*Exchange) struct{} { return struct{}{} })
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unchanged output must not retry)", w.Code)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no identical re-dispatch)", calls)
	}
}

// reportThenFailRewriter reports a strong verdict and then errors out.
type reportThenFailRewriter struct{}

func (reportThenFailRewriter) Name() string { return "report_then_fail" }

func (reportThenFailRewriter) RewriteAfterFailure(_ context.Context, _ struct{}, _ protocols.ProtocolID, _ []byte, up fact.Upstream, sink fact.Sink) ([]byte, error) {
	sink.Report(fact.Fact{Kind: fact.KindPayloadRefused, Status: up.StatusCode})
	return nil, errors.New("changed my mind")
}

// An error is a full abstention: facts reported before the failure are
// dropped from the fold, so a half-finished rewriter cannot steer the chain —
// here a refusal that would have caused a failover is discarded and the 400
// stays terminal.
func TestErroringRewriterFactsAreDiscarded(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	RegisterFailureRewriter(svc, reportThenFailRewriter{}, func(*Exchange) struct{} { return struct{}{} })
	apiKey := seedTwoCandidateModel(t, svc, db, upstream.URL)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 terminal (discarded refusal must not fail over)", w.Code)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (the dropped verdict steered the chain)", got)
	}
}

// mutateThenFailRewriter edits its input in place and then errors out.
type mutateThenFailRewriter struct{}

func (mutateThenFailRewriter) Name() string { return "mutate_then_fail" }

func (mutateThenFailRewriter) RewriteAfterFailure(_ context.Context, _ struct{}, _ protocols.ProtocolID, body []byte, _ fact.Upstream, _ fact.Sink) ([]byte, error) {
	for i := range body {
		body[i] = 'X'
	}
	return nil, errors.New("scribbled and left")
}

// Each rewriter gets a private copy of the body: an in-place edit by an
// abstaining rewriter must not corrupt the audit capture of what was actually
// sent upstream.
func TestAbstainingRewriterCannotCorruptTheAuditCapture(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	RegisterFailureRewriter(svc, mutateThenFailRewriter{}, func(*Exchange) struct{} { return struct{}{} })
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	c, _ := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	sent := captured.bodies.UpstreamRequest()
	if bytes.Contains(sent, []byte("XXXX")) {
		t.Fatalf("audit capture was scribbled over by an abstaining rewriter: %q", sent)
	}
	if !bytes.Contains(sent, []byte("gpt-4o-real")) {
		t.Fatalf("audit capture no longer holds the dispatched body: %q", sent)
	}
}

// A 401 is a credential problem: no payload repair addresses it, so the
// repair verdict is set aside and the key rotation the kernel's own reading
// demands still happens — on the ORIGINAL body, with the key already marked
// failed left behind.
func TestRepairDoesNotBypassKeyRotationOn401(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu     sync.Mutex
		auths  []string
		bodies []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		bodies = append(bodies, string(body))
		mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer sk-bad" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	registerRepairer(svc, []byte(`{"model":"gpt-4o-real","messages":[{"role":"user","content":"repaired"}]}`))
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-bad", "bad", 1, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-good", "good", 2, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after rotation; body = %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 || auths[0] != "Bearer sk-bad" || auths[1] != "Bearer sk-good" {
		t.Fatalf("expected key rotation [sk-bad, sk-good], got %v (repair must not pin the failed key)", auths)
	}
	if bytes.Contains([]byte(bodies[1]), []byte("repaired")) {
		t.Errorf("rotation dispatched the discarded repair: %s", bodies[1])
	}
}

// A 5xx is a provider fault: the repair cannot address it either, and the
// chain fails over to the next candidate with the original body.
func TestRepairDoesNotBypassFailoverOn5xx(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu     sync.Mutex
		bodies []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		if bytes.Contains(body, []byte(`"model":"c1-model"`)) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"c2-model","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	registerRepairer(svc, []byte(`{"model":"c1-model","messages":[{"role":"user","content":"repaired"}]}`))
	apiKey := seedTwoCandidateModel(t, svc, db, upstream.URL)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the next candidate; body = %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (failover, not same-candidate retry)", len(bodies))
	}
	if bytes.Contains([]byte(bodies[1]), []byte("repaired")) {
		t.Errorf("failover dispatched the discarded repair: %s", bodies[1])
	}
}

// silentRewriter changes the body but reports nothing.
type silentRewriter struct{ out []byte }

func (silentRewriter) Name() string { return "silent_rewriter" }

func (r silentRewriter) RewriteAfterFailure(context.Context, struct{}, protocols.ProtocolID, []byte, fact.Upstream, fact.Sink) ([]byte, error) {
	return r.out, nil
}

// A changed body with no repair verdict from the SAME shape is never
// dispatched, even when a separate observer reports retry-same: an observer's
// verdict cannot adopt someone else's bytes.
func TestFactlessRepairedBodyIsNotDispatched(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	registerVerdictObserver(svc, fact.KindPayloadRepairedRetrySame)
	RegisterFailureRewriter(svc, silentRewriter{out: []byte(`{"model":"gpt-4o-real","messages":[{"role":"user","content":"unvouched"}]}`)},
		func(*Exchange) struct{} { return struct{}{} })
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unvouched body must not dispatch)", w.Code)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

// nilWithFactsRewriter reports a strong verdict but returns no body.
type nilWithFactsRewriter struct{}

func (nilWithFactsRewriter) Name() string { return "nil_with_facts" }

func (nilWithFactsRewriter) RewriteAfterFailure(_ context.Context, _ struct{}, _ protocols.ProtocolID, _ []byte, up fact.Upstream, sink fact.Sink) ([]byte, error) {
	sink.Report(fact.Fact{Kind: fact.KindPayloadRefused, Status: up.StatusCode})
	return nil, nil
}

// Abstention is atomic: a rewriter that returns nil contributes no facts
// either, so it cannot steer a failover from a shape whose licence is
// offering repairs — the 400 stays terminal.
func TestNilOutputDropsTheFactsToo(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	RegisterFailureRewriter(svc, nilWithFactsRewriter{}, func(*Exchange) struct{} { return struct{}{} })
	apiKey := seedTwoCandidateModel(t, svc, db, upstream.URL)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 terminal (dropped facts must not fail over)", w.Code)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

// revertingRewriter returns the original body its predecessor replaced,
// captured from its own first input.
type revertingRewriter struct{ original *[]byte }

func (revertingRewriter) Name() string { return "reverting_rewriter" }

func (r revertingRewriter) RewriteAfterFailure(_ context.Context, _ struct{}, _ protocols.ProtocolID, _ []byte, up fact.Upstream, sink fact.Sink) ([]byte, error) {
	sink.Report(fact.Fact{Kind: fact.KindPayloadRepairedRetrySame, Status: up.StatusCode})
	return *r.original, nil
}

type capturingRewriter struct {
	original *[]byte
	out      []byte
}

func (capturingRewriter) Name() string { return "capturing_rewriter" }

func (r capturingRewriter) RewriteAfterFailure(_ context.Context, _ struct{}, _ protocols.ProtocolID, body []byte, up fact.Upstream, sink fact.Sink) ([]byte, error) {
	if *r.original == nil {
		*r.original = append([]byte(nil), body...)
	}
	sink.Report(fact.Fact{Kind: fact.KindPayloadRepairedRetrySame, Status: up.StatusCode})
	return r.out, nil
}

// Repairs execute only for statuses that judge the BYTES. A 403 judges the
// account's permission and a 404 the route: an ever-changing repair on those
// would burn the whole budget replacing an actionable status with a 504, so
// the verdict is set aside and the failure keeps its terminal handling.
func TestRepairDoesNotRetryNonPayloadClientErrors(t *testing.T) {
	for _, status := range []int{http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnsupportedMediaType} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			var (
				mu    sync.Mutex
				calls int
			)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"no"}}`))
			}))
			defer upstream.Close()

			svc := newSvc(t, db)
			var repairs int
			RegisterFailureRewriter(svc, countingRewriter{n: &repairs},
				func(*Exchange) struct{} { return struct{}{} })
			p := createProvider(t, db, "p1", upstream.URL)
			createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
			m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
			apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

			c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
			svc.Handle(c, apiKey)

			if w.Code != status {
				t.Fatalf("status = %d, want %d surfaced terminal", w.Code, status)
			}
			mu.Lock()
			got := calls
			mu.Unlock()
			if got != 1 {
				t.Fatalf("upstream calls = %d, want 1 (no repair retry on %d)", got, status)
			}
		})
	}
}

// Acceptance is per invocation: a later rewriter that changes the body while
// reporting nothing is dropped whole, and the dispatch carries the LAST
// vouched-for body — an earlier repair verdict cannot be ridden by someone
// else's factless edit.
func TestLaterFactlessEditCannotRideAnEarlierVerdict(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu     sync.Mutex
		bodies []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		if !bytes.Contains(body, []byte("vouched")) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"cannot process this payload"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	registerRepairer(svc, []byte(`{"model":"gpt-4o-real","messages":[{"role":"user","content":"vouched"}]}`))
	RegisterFailureRewriter(svc, silentRewriter{out: []byte(`{"model":"gpt-4o-real","messages":[{"role":"user","content":"unvouched"}]}`)},
		func(*Exchange) struct{} { return struct{}{} })
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with the vouched repair; body = %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(bodies))
	}
	if bytes.Contains([]byte(bodies[1]), []byte("unvouched")) {
		t.Fatalf("the factless edit was dispatched on the earlier verdict: %s", bodies[1])
	}
}

// A chain whose net effect restores the bytes that just failed has repaired
// nothing: the final body is checked against the original dispatch, so an
// A->B->A chain does not re-send the failed bytes until the budget runs out.
func TestChainRevertedToTheOriginalIsNoRepair(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	var original []byte
	RegisterFailureRewriter(svc, capturingRewriter{original: &original, out: []byte(`{"model":"gpt-4o-real","messages":[{"role":"user","content":"intermediate"}]}`)},
		func(*Exchange) struct{} { return struct{}{} })
	RegisterFailureRewriter(svc, revertingRewriter{original: &original},
		func(*Exchange) struct{} { return struct{}{} })
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (a reverted chain is no repair)", w.Code)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (identical bytes must not re-dispatch)", calls)
	}
}

// A stronger co-reported verdict out-ranks the repair: a refusal says the
// payload was judged, so the chain moves to the next candidate with the
// ORIGINAL body rather than re-sending a repair to the provider that judged
// it.
func TestStrongerVerdictOutranksTheRepair(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu     sync.Mutex
		bodies []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		if bytes.Contains(body, []byte(`"model":"c1-model"`)) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"refused"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"c2-model","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	registerVerdictObserver(svc, fact.KindPayloadRefused)
	registerRepairer(svc, []byte(`{"model":"c1-model","messages":[{"role":"user","content":"repaired"}]}`))
	apiKey := seedTwoCandidateModel(t, svc, db, upstream.URL)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the next candidate; body = %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (refused then next candidate)", len(bodies))
	}
	if bytes.Contains([]byte(bodies[1]), []byte("repaired")) {
		t.Error("the refusing verdict was out-ranked: the repaired body was dispatched")
	}
}
