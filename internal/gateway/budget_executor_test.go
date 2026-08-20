package gateway

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/config"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"

	"gorm.io/gorm"
)

// budgetGatewayConfig is the default gateway config with the count budgets
// overridden, so a test can exhaust them without dozens of candidates.
func budgetGatewayConfig(maxAttempts, maxProbes int) config.GatewayConfig {
	g := config.DefaultGatewayConfig()
	g.MaxUpstreamAttempts = maxAttempts
	g.MaxCandidateProbes = maxProbes
	return g
}

// seedNCandidates wires one external model onto n providers (one key each),
// all pointing at upstreamURL, in sort order c1..cn.
func seedNCandidates(t *testing.T, svc *Service, db *gorm.DB, upstreamURL string, n int) *model.APIKey {
	t.Helper()
	now := time.Now().UTC()
	m := &model.Model{Name: "gpt-4o", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	for i := 0; i < n; i++ {
		p := createProvider(t, db, "p"+string(rune('1'+i)), upstreamURL)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-"+string(rune('1'+i)), "k1", 1, true)
		if err := db.Create(&model.ModelCandidate{
			ModelID: m.ID, ProviderID: p.ID, ProviderModelName: "c" + string(rune('1'+i)) + "-model",
			InputPrice: 0, OutputPrice: 0, MaxOutput: 4096,
			SupportsStreaming: boolPtr(true), SupportsFunctionCalling: boolPtr(true),
			ManagementStatus: model.ModelCandidateStatusEnabled, SortOrder: i + 1,
			VerificationStatus: model.ModelVerificationStatusPassed,
			CreatedAt:          now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatalf("seed candidate %d: %v", i, err)
		}
	}
	return createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})
}

// The attempt budget bounds upstream dispatches across candidates: with a
// budget of 2 and three failing candidates, the third is never dispatched and
// the caller is told the request ran out of allowance, not that every
// upstream failed.
func TestAttemptBudgetStopsTheCandidateWalk(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, budgetGatewayConfig(2, 20))
	apiKey := seedNCandidates(t, svc, db, upstream.URL, 3)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (attempt budget exhausted); body = %s", w.Code, w.Body.String())
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2 (budget of 2)", got)
	}
	var log model.RequestLog
	if err := db.First(&log).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if log.FailReason == nil || *log.FailReason != "request_budget_exhausted" {
		t.Errorf("fail_reason = %v, want request_budget_exhausted", strOrNil(log.FailReason))
	}
}

// The attempt budget spans key rotations inside one candidate too, and a
// sticky verdict still beats the budget verdict: two rate-limited keys spend
// the whole budget, the third key is never dispatched, and the caller sees
// the 429 — the answer they can act on — not the 504.
func TestAttemptBudgetBoundsKeyRotationAndStickyStillWins(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, budgetGatewayConfig(2, 20))
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-2", "k2", 2, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-3", "k3", 3, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (sticky beats the budget verdict); body = %s", w.Code, w.Body.String())
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2 (budget of 2 spans rotations)", got)
	}
}

// The probe budget bounds candidates abandoned before dispatch: with a probe
// budget of 2 and three candidates all refused by a rewriter, the third is
// never probed and nothing ever reaches an upstream.
func TestProbeBudgetStopsTheCandidateWalk(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var hit sync.Once
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Do(func() { upstreamCalled = true })
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, budgetGatewayConfig(3, 2))
	// A stage of its own: the harness already registers the system-prompt
	// rewriter at StageCustomPrompt, and stages are exclusive.
	RegisterEgressRewriter(svc, refusingRewriter{}, StageCustomPrompt+1,
		func(*Exchange) struct{} { return struct{}{} })
	apiKey := seedNCandidates(t, svc, db, upstream.URL, 3)

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (probe budget exhausted); body = %s", w.Code, w.Body.String())
	}
	if upstreamCalled {
		t.Error("upstream was called - every candidate should have been refused pre-dispatch")
	}
	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if captured.probesSpent != 2 {
		t.Errorf("probesSpent = %d, want 2 (third candidate never probed)", captured.probesSpent)
	}
}

// A transport failure reached for the wire, so it spends an attempt: two
// dead-host candidates with a budget of 1 make exactly one dial, and the
// second candidate is never tried.
func TestTransportFailureSpendsAnAttempt(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	// A server started and immediately closed: connections are refused fast.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	svc := newSvcWithGateway(t, db, budgetGatewayConfig(1, 20))
	apiKey := seedNCandidates(t, svc, db, deadURL, 2)

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (attempt budget spent on the dial); body = %s", w.Code, w.Body.String())
	}
	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if len(captured.attempts) != 1 {
		t.Errorf("attempts recorded = %d, want 1 (second candidate never dialled)", len(captured.attempts))
	}
}

// A 2xx whose delivery then fails still spent a dispatch. The charge lives at
// the wire, so a chain of upstreams answering 200 with unusable bodies is
// bounded exactly like a chain of 5xx.
func TestFailedDeliverySpendsAnAttempt(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`this is not json`))
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, budgetGatewayConfig(2, 20))
	apiKey := seedNCandidates(t, svc, db, upstream.URL, 3)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (attempt budget spent on failed deliveries); body = %s", w.Code, w.Body.String())
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2 (budget of 2)", got)
	}
}

// A budget spent by the final candidate answers the same as one spent with
// candidates still waiting: the terminal recomputes exhaustion instead of
// relying on another loop iteration to notice it, so configuring one more
// unused candidate cannot change the verdict.
func TestBudgetSpentOnTheFinalCandidateStillAnswersExhaustion(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, budgetGatewayConfig(2, 20))
	apiKey := seedNCandidates(t, svc, db, upstream.URL, 2)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (budget spent on the final candidate); body = %s", w.Code, w.Body.String())
	}
}

// A candidate whose every key dies before dispatch — here, keys that no
// longer decrypt — still spends a probe: attempts are charged at the wire, so
// an unchanged attempt ledger after the key loop marks the candidate as a
// pre-dispatch abandonment. Without the charge, a pool full of stale keys
// repeats database and decryption work bounded by nothing but the wall clock.
func TestUndispatchableKeysSpendAProbe(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var dispatched sync.Once
	upstreamHit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dispatched.Do(func() { upstreamHit = true })
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, budgetGatewayConfig(3, 2))
	apiKey := seedNCandidates(t, svc, db, upstream.URL, 3)
	// Corrupt every stored key so decryption fails and no key ever dispatches.
	if err := db.Model(&model.ProviderKey{}).Where("1 = 1").
		Update("encrypted_key", "not-a-ciphertext").Error; err != nil {
		t.Fatalf("corrupt keys: %v", err)
	}

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (probe budget exhausted by stale keys); body = %s", w.Code, w.Body.String())
	}
	if upstreamHit {
		t.Error("upstream was dispatched - corrupted keys must never produce a request")
	}
	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if captured.probesSpent != 2 {
		t.Errorf("probesSpent = %d, want 2 (third candidate never walked)", captured.probesSpent)
	}
}

// Pool exhaustion is not budget exhaustion: one failing candidate inside a
// roomy budget still answers the generic "all candidates failed", because the
// chain ended by running out of candidates, not allowance.
func TestPoolExhaustionStillAnswersAllCandidatesFailed(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, budgetGatewayConfig(3, 20))
	apiKey := seedNCandidates(t, svc, db, upstream.URL, 1)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (pool exhausted, budget not); body = %s", w.Code, w.Body.String())
	}
	var log model.RequestLog
	if err := db.First(&log).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if log.FailReason == nil || *log.FailReason != "all_candidates_failed" {
		t.Errorf("fail_reason = %v, want all_candidates_failed", strOrNil(log.FailReason))
	}
}
