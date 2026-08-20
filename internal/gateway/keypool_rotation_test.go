package gateway

// Integration tests for the key pool's rotation and benching, on the same
// fixtures the relay tests use (seeded sqlite, loopback httptest upstream,
// Authorization recording). The behavioural halves live in keypool_test.go;
// what these pin is the WIRING: the relay actually walks the pool's order,
// books benches on plain 429s, and releases them on the healthy-interaction
// ledger.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

const (
	rotationOKBody = `{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
	// A rate limit without a word the quota detector matches — a plain,
	// transient 429, the only kind that benches.
	plainRateLimitBody    = `{"error":{"message":"rate limited, slow down","type":"rate_limit_error"}}`
	quotaExhausted429Body = `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota","type":"insufficient_quota"}}`
)

// authRecorder segments the Authorization headers the fake upstream saw, per
// relay request: reset before a Handle, read after it (non-stream relays are
// synchronous, so no call can straddle the boundary).
type authRecorder struct {
	mu    sync.Mutex
	auths []string
}

func (r *authRecorder) record(auth string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auths = append(r.auths, auth)
}

func (r *authRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auths = nil
}

func (r *authRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.auths))
	copy(out, r.auths)
	return out
}

// benched is the test-side view of the pool's bench table — state the relay
// never exposes, asserted directly where the wiring (not the ordering) is
// the subject.
func (p *keyPool) benched(keyID uint) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.states[keyID]
	return ok && s.benched()
}

func writeRateLimited(w http.ResponseWriter, retryAfter string) {
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, plainRateLimitBody)
}

// Two keys, both healthy: consecutive requests must start on different keys
// — the whole point of the pool. Without rotation, every request would open
// on key 1 and key 2 would sit idle until key 1 failed.
func TestKeyPoolRotatesFirstDispatchAcrossRequests(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	rec := &authRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, rotationOKBody)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-b", "k2", 2, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	relay := func() []string {
		rec.reset()
		c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		return rec.snapshot()
	}

	if got := relay(); len(got) != 1 || got[0] != "Bearer sk-a" {
		t.Errorf("request 1 dispatched %v, want [Bearer sk-a]", got)
	}
	if got := relay(); len(got) != 1 || got[0] != "Bearer sk-b" {
		t.Errorf("request 2 dispatched %v, want [Bearer sk-b] — pool did not rotate", got)
	}
}

// toggledRefusingRewriter refuses the egress body only while *refuse is
// set, so a test can make individual requests die pre-dispatch on demand.
type toggledRefusingRewriter struct{ refuse *bool }

func (toggledRefusingRewriter) Name() string { return "toggled-refuser" }

func (r toggledRefusingRewriter) RewriteEgress(_ context.Context, _ struct{}, _ protocols.ProtocolID, body []byte, _ fact.Sink) ([]byte, error) {
	if *r.refuse {
		return nil, errors.New("refused for this request")
	}
	return body, nil
}

// The rotation cursor advances only when the key walk actually starts:
// negotiation, modality refusal, request build, and rewrite verdicts can
// all skip a candidate before any key is tried, and letting those consume
// turns would skew consecutive REAL dispatches onto one key. Here every
// other request is refused pre-dispatch (an egress rewriter that cannot
// produce a body); the real dispatches must still alternate.
func TestPreDispatchSkipDoesNotConsumeRotationTurn(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	rec := &authRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, rotationOKBody)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	refuse := false
	RegisterEgressRewriter(svc, toggledRefusingRewriter{refuse: &refuse}, StageCustomPrompt+1,
		func(*Exchange) struct{} { return struct{}{} })
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-b", "k2", 2, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	refusedPreDispatch := func() {
		refuse = true
		defer func() { refuse = false }()
		rec.reset()
		c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
		svc.Handle(c, apiKey)
		if w.Code == http.StatusOK || len(rec.snapshot()) != 0 {
			t.Fatalf("rewriter-refused request was expected to die before any dispatch; status=%d dispatches=%v",
				w.Code, rec.snapshot())
		}
	}
	firstAuth := func() string {
		rec.reset()
		c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		got := rec.snapshot()
		if len(got) == 0 {
			t.Fatal("no upstream dispatch recorded")
		}
		return got[0]
	}

	refusedPreDispatch()
	if got := firstAuth(); got != "Bearer sk-a" {
		t.Fatalf("dispatch 1 = %q, want A (refused request consumed a rotation turn)", got)
	}
	refusedPreDispatch()
	if got := firstAuth(); got != "Bearer sk-b" {
		t.Fatalf("dispatch 2 = %q, want B (refused request consumed a rotation turn)", got)
	}
}

// A key whose destination authorization trails the provider's must not
// occupy a rotation slot: with [stale, B, C], whole-list rotation would
// start B, B, C — handing the stale key's every turn to B — so the filter
// has to run before the cursor is applied.
func TestStaleDestinationKeyExcludedFromRotation(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	rec := &authRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, rotationOKBody)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-b", "k2", 2, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-c", "k3", 3, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	// Strand key A on a destination version the provider no longer has.
	if err := db.Model(&model.ProviderKey{}).Where("label = ?", "k1").
		UpdateColumn("authorized_destination_version", 999).Error; err != nil {
		t.Fatalf("strand key A: %v", err)
	}

	firstAuth := func() string {
		rec.reset()
		c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		got := rec.snapshot()
		if len(got) == 0 {
			t.Fatal("no upstream dispatch recorded")
		}
		return got[0]
	}

	for i, want := range []string{"Bearer sk-b", "Bearer sk-c", "Bearer sk-b", "Bearer sk-c"} {
		if got := firstAuth(); got != want {
			t.Fatalf("request %d first dispatch %q, want %q (stale key skewed the rotation)", i+1, got, want)
		}
	}
}

// An undecryptable key must not occupy a rotation slot either: with
// [bad, B, C], whole-list rotation would first-dispatch B, B, C — handing
// the bad key's every turn to B — so the decrypt check has to run in the
// pre-rotation filter, not inside the key walk.
func TestUndecryptableKeyExcludedFromRotation(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	rec := &authRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, rotationOKBody)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-b", "k2", 2, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-c", "k3", 3, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	if err := db.Model(&model.ProviderKey{}).Where("label = ?", "k1").
		Update("encrypted_key", "corrupt-ciphertext").Error; err != nil {
		t.Fatalf("corrupt key A: %v", err)
	}

	firstAuth := func() string {
		rec.reset()
		c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		got := rec.snapshot()
		if len(got) == 0 {
			t.Fatal("no upstream dispatch recorded")
		}
		return got[0]
	}

	for i, want := range []string{"Bearer sk-b", "Bearer sk-c", "Bearer sk-b", "Bearer sk-c"} {
		if got := firstAuth(); got != want {
			t.Fatalf("request %d first dispatch %q, want %q (undecryptable key skewed the rotation)", i+1, got, want)
		}
	}
}

// Three keys, key A rate-limited with Retry-After: 60 while the service's
// configured fallback is 15s. A later request whose rotation lands ON A must
// still skip it at t+31s (the fallback would have healed at 15s, so
// honouring the header is the only way this holds) and dispatch it first
// again at t+61s once the stated window elapsed.
func TestRetryAfterBenchHonouredAndHeals(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	rec := &authRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		rec.record(auth)
		if auth == "Bearer sk-a" {
			writeRateLimited(w, "60")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, rotationOKBody)
	}))
	defer upstream.Close()

	// Fallback deliberately different from the header above (15s vs 60s):
	// only then does "still cooling at t+31s" prove the header was honoured
	// rather than the configured default. Fake clock so the stated window can
	// elapse inside the test.
	gwCfg := testGatewayConfig()
	gwCfg.KeyRateLimitCooldown = 15 * time.Second
	svc := newSvcWithGateway(t, db, gwCfg)
	pool, advance := fakePool(t)
	svc.keyPool = pool

	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-b", "k2", 2, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-c", "k3", 3, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	firstAuth := func() string {
		rec.reset()
		c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		got := rec.snapshot()
		if len(got) == 0 {
			t.Fatal("no upstream dispatch recorded")
		}
		return got[0]
	}

	// t=0, start A: A is benched for 60s, rotation carries the request to B.
	rec.reset()
	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, apiKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := rec.snapshot(); len(got) != 2 || got[0] != "Bearer sk-a" || got[1] != "Bearer sk-b" {
		t.Fatalf("request 1 dispatched %v, want [sk-a then sk-b] (A throttled, rotation carries on)", got)
	}
	// t=0, start C: request 1's turn was already served by B — rotation runs
	// over the healthy subset, so B does not serve twice in a row — and A
	// stays benched in the tail, never reached.
	if got := firstAuth(); got != "Bearer sk-c" {
		t.Fatalf("request 2 first dispatch %q, want C (healthy-subset rotation, B already served)", got)
	}
	// t=31s, start B (the two-key healthy subset wrapped): had the
	// configured 15s fallback been used, A would be ready here and walk
	// first; the header said 60s, so a healthy key must still go first.
	advance(31 * time.Second)
	if got := firstAuth(); got != "Bearer sk-b" {
		t.Fatalf("request 3 first dispatch %q, want B (A's Retry-After: 60 not honoured)", got)
	}
	// t=61s, start A: the window elapsed, A is back at the front.
	advance(30 * time.Second)
	if got := firstAuth(); got != "Bearer sk-a" {
		t.Fatalf("request 4 first dispatch %q, want A (bench did not heal)", got)
	}
}

// Both keys rate-limited with a window far beyond the test: the next request
// must still dispatch — twice. A bench reorders the walk; it must never idle
// a provider out of rotation on recorded state alone.
func TestAllCoolingPoolStillDispatches(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	rec := &authRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("Authorization"))
		writeRateLimited(w, "3600")
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-b", "k2", 2, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	relay := func() (int, []string) {
		rec.reset()
		c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
		svc.Handle(c, apiKey)
		return w.Code, rec.snapshot()
	}

	if code, got := relay(); code != http.StatusTooManyRequests || len(got) != 2 {
		t.Fatalf("request 1: status = %d dispatches = %v, want 429 with both keys tried", code, got)
	}
	// Every key is now benched for an hour — the pool must still walk them.
	if code, got := relay(); code != http.StatusTooManyRequests || len(got) != 2 {
		t.Fatalf("request 2: status = %d dispatches = %v, want 429 with both keys STILL tried (bench must not idle the pool)", code, got)
	}
}

// A benched key dispatched anyway and served 2xx must come off the bench: a
// later rotation onto it dispatches it first again. Key B is disabled after
// the first request so the second request's pool is just A — the only key,
// benched, and still dispatched (the demotion-not-exclusion guarantee).
func TestSuccessfulDispatchReleasesTheBench(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	rec := &authRecorder{}
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		rec.record(auth)
		calls++
		// First relay request: A is throttled. From the second on: healthy.
		if calls == 1 && auth == "Bearer sk-a" {
			writeRateLimited(w, "3600")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, rotationOKBody)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-b", "k2", 2, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var keyA model.ProviderKey
	if err := db.Where("label = ?", "k1").First(&keyA).Error; err != nil {
		t.Fatalf("load key A: %v", err)
	}

	relay := func() int {
		rec.reset()
		c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
		svc.Handle(c, apiKey)
		return w.Code
	}

	if code := relay(); code != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", code)
	}
	if !svc.keyPool.benched(keyA.ID) {
		t.Fatal("plain 429 did not bench the key")
	}

	// Take B out of the pool, then have A serve: the only key is benched and
	// still dispatched, and the 2xx must release the bench.
	if err := db.Model(&model.ProviderKey{}).Where("label = ?", "k2").
		Update("management_status", model.ProviderKeyStatusDisabled).Error; err != nil {
		t.Fatalf("disable key B: %v", err)
	}
	if code := relay(); code != http.StatusOK {
		t.Fatalf("request 2 status = %d, want 200 (benched sole key must still dispatch)", code)
	}
	if svc.keyPool.benched(keyA.ID) {
		t.Fatal("successful dispatch did not release the key's bench")
	}
}

// A 2xx acceptance releases the key's bench IMMEDIATELY — on the status
// line, not on the delivery verdict: a long response would otherwise hold
// the release for its whole duration, and a delivery that never concludes
// cleanly would skip it entirely, keeping a key the upstream just accepted
// demoted. The upstream here holds the response body open until the test
// has observed the release.
func TestBenchReleasedOn2xxAcceptance(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	bodyGate := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // acceptance on the wire; body deliberately held
		<-bodyGate
		_, _ = io.WriteString(w, rotationOKBody)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var keyA model.ProviderKey
	if err := db.Where("label = ?", "k1").First(&keyA).Error; err != nil {
		t.Fatalf("load key A: %v", err)
	}
	// Benched before the request; the acceptance below must release it.
	svc.keyPool.coolKey(keyA.ID, keyA.ConfigVersion, svc.keyPool.stamp(), time.Hour)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
		svc.Handle(c, apiKey)
		_ = w
	}()

	deadline := time.Now().Add(2 * time.Second)
	for svc.keyPool.benched(keyA.ID) {
		if time.Now().After(deadline) {
			close(bodyGate)
			<-done
			t.Fatal("bench not released on 2xx acceptance while the body was still open")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(bodyGate)
	<-done
	if svc.keyPool.benched(keyA.ID) {
		t.Error("bench re-appeared after the accepted response completed")
	}
}

// The bench must be booked from the 429's HEADERS, not after the error body
// is drained: the body read is bounded by errorBodyTotalBudget (10s), and a
// slow-trickle upstream would otherwise keep the limited key undemoted for
// that whole window — then start the stated Retry-After late. The upstream
// here holds the body open until the test has observed the bench.
func TestPlain429BenchBookedBeforeBodyCompletes(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	bodyGate := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		w.(http.Flusher).Flush() // headers on the wire; body deliberately held
		<-bodyGate
		_, _ = io.WriteString(w, plainRateLimitBody)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var keyA model.ProviderKey
	if err := db.Where("label = ?", "k1").First(&keyA).Error; err != nil {
		t.Fatalf("load key A: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
		svc.Handle(c, apiKey)
		_ = w // sole key is rate-limited; the request outcome is not the subject
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !svc.keyPool.benched(keyA.ID) {
		if time.Now().After(deadline) {
			close(bodyGate)
			<-done
			t.Fatal("bench not booked from headers while the error body was still open")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(bodyGate)
	<-done
	if !svc.keyPool.benched(keyA.ID) {
		t.Error("bench released after the plain 429 body completed")
	}
}

// A LOST invalidation CAS must not touch the bench: "lost" can mean a
// retest already refreshed the key, and a bench present at that point can
// postdate the recovery — deleting it would put a rate-limited key back at
// the front of the walk. Only the winning invalidation clears bench state.
func TestInvalidationCASLostKeepsBench(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var bump sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Refresh the key's test generation before answering, so the
		// invalidation CAS triggered by this quota response loses.
		bump.Do(func() {
			if err := db.Model(&model.ProviderKey{}).Where("label = ?", "k1").
				UpdateColumn("test_generation", 999).Error; err != nil {
				t.Errorf("bump test generation: %v", err)
			}
		})
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, quotaExhausted429Body)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var keyA model.ProviderKey
	if err := db.Where("label = ?", "k1").First(&keyA).Error; err != nil {
		t.Fatalf("load key A: %v", err)
	}
	svc.keyPool.coolKey(keyA.ID, keyA.ConfigVersion, svc.keyPool.stamp(), time.Hour)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, apiKey)
	_ = w // sole key is quota-dead; the request outcome is not the subject

	if err := db.Where("label = ?", "k1").First(&keyA).Error; err != nil {
		t.Fatalf("reload key A: %v", err)
	}
	if keyA.VerificationStatus == model.VerificationStatusFailed {
		t.Fatal("CAS was expected to lose, but the invalidation applied")
	}
	if !svc.keyPool.benched(keyA.ID) {
		t.Error("lost invalidation CAS dropped the key's bench")
	}
}

// A quota-exhausted 429 on an ALREADY-benched key must also drop that bench:
// the retest path keeps the row's ConfigVersion, so a leftover bench would
// still match after a successful retest and demote the recovered key until
// expiry.
func TestQuotaExhausted429DropsExistingBench(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, quotaExhausted429Body)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var keyA model.ProviderKey
	if err := db.Where("label = ?", "k1").First(&keyA).Error; err != nil {
		t.Fatalf("load key A: %v", err)
	}
	// A plain rate-limit bench booked before the quota verdict arrives.
	svc.keyPool.coolKey(keyA.ID, keyA.ConfigVersion, svc.keyPool.stamp(), time.Hour)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, apiKey)
	_ = w // sole key is quota-dead; the request outcome is not the subject

	if err := db.Where("label = ?", "k1").First(&keyA).Error; err != nil {
		t.Fatalf("reload key A: %v", err)
	}
	if keyA.VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("key A verification_status = %d, want failed (retest path)", keyA.VerificationStatus)
	}
	if svc.keyPool.benched(keyA.ID) {
		t.Error("persistent invalidation left the key's earlier bench in place")
	}
}

// A quota-exhausted 429 takes the key out via the persistent retest path and
// must NOT bench it: the key is leaving rotation for a reason waiting cannot
// fix, and a bench entry for it would be state about a key nobody walks.
func TestQuotaExhausted429RemovesWithoutBenching(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, quotaExhausted429Body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, rotationOKBody)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-b", "k2", 2, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, apiKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (rotation to B); body = %s", w.Code, w.Body.String())
	}

	var keyA model.ProviderKey
	if err := db.Where("label = ?", "k1").First(&keyA).Error; err != nil {
		t.Fatalf("load key A: %v", err)
	}
	if keyA.VerificationStatus != model.VerificationStatusFailed {
		t.Errorf("key A verification_status = %d, want failed (retest path)", keyA.VerificationStatus)
	}
	if svc.keyPool.benched(keyA.ID) {
		t.Error("quota-exhausted 429 booked a bench entry for the key")
	}
}
