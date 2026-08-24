package gateway

// Relay-level wiring tests for balanced scheduling: through the real Handle
// path, a balanced model's first dispatch follows the sticky binding (spread
// across keys, stable per key), reassigns when the bound provider's breaker
// opens, and — the nail that must never move — a failover model never
// consults the registry at all. The registry's own rules are pinned in
// binding_test.go; these tests pin that the relay actually routes by them.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	ycrypto "github.com/yolorouter/yolorouter/pkg/crypto"

	"github.com/yolorouter/yolorouter/internal/gateway/circuit"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

const balancedOKBody = `{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`

// countingUpstream is a fake upstream that always succeeds and counts its
// hits, so a test can assert which provider served which request.
type countingUpstream struct {
	server *httptest.Server
	hits   atomic.Int64
}

func newCountingUpstream(t *testing.T) *countingUpstream {
	t.Helper()
	u := &countingUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(balancedOKBody))
	}))
	t.Cleanup(u.server.Close)
	return u
}

// failingUpstream always answers 500 — enough consecutive faults to open a
// one-fault breaker, i.e. a provider that is down.
type failingUpstream struct {
	server *httptest.Server
	hits   atomic.Int64
}

func newFailingUpstream(t *testing.T) *failingUpstream {
	t.Helper()
	u := &failingUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream fell over"}}`))
	}))
	t.Cleanup(u.server.Close)
	return u
}

// addCandidateToModel attaches a second (third, ...) provider's mapping to an
// existing model — the shape balanced scheduling exists for, which the
// createModelAndCandidate helper (one model, one candidate) cannot express.
func addCandidateToModel(t *testing.T, db *gorm.DB, m *model.Model, provider *model.Provider, providerModelName string, order int) {
	t.Helper()
	now := time.Now().UTC()
	cand := &model.ModelCandidate{
		ModelID: m.ID, ProviderID: provider.ID, ProviderModelName: providerModelName,
		InputPrice: 1.0, OutputPrice: 2.0, MaxOutput: 4096,
		SupportsStreaming: boolPtr(false), SupportsFunctionCalling: boolPtr(false),
		ManagementStatus: model.ModelCandidateStatusEnabled, SortOrder: order,
		VerificationStatus: model.ModelVerificationStatusPassed,
		CreatedAt:          now, UpdatedAt: now,
	}
	if err := db.Create(cand).Error; err != nil {
		t.Fatalf("seed extra candidate: %v", err)
	}
}

func setModelSchedulingMode(t *testing.T, db *gorm.DB, modelID uint, mode model.SchedulingMode) {
	t.Helper()
	if err := db.Model(&model.Model{}).Where("id = ?", modelID).Update("scheduling_mode", mode).Error; err != nil {
		t.Fatalf("set scheduling mode %q: %v", mode, err)
	}
}

// createDistinctAPIKey is createAPIKey with a caller-chosen token, so one
// test can hold several keys side by side (the shared helper's fixed hash
// trips the key_hash UNIQUE constraint on the second call).
func createDistinctAPIKey(t *testing.T, db *gorm.DB, status int, modelIDs []uint, token string) *model.APIKey {
	t.Helper()
	now := time.Now().UTC()
	k := &model.APIKey{
		KeyHash: ycrypto.HashToken(token), KeyPrefix: token + "------", Status: status, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(k).Error; err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	for _, mid := range modelIDs {
		if err := db.Create(&model.APIKeyModel{APIKeyID: k.ID, ModelID: mid, CreatedAt: now}).Error; err != nil {
			t.Fatalf("seed allowlist: %v", err)
		}
	}
	return k
}

func relayOK(t *testing.T, svc *Service, apiKey *model.APIKey, modelName string) {
	t.Helper()
	c, w := newCtx([]byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, apiKey)
	if w.Code != http.StatusOK {
		t.Fatalf("relay status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
}

// Two keys on a balanced two-provider model must land on different
// providers, and stay there — spread on first assignment, stickiness after.
func TestBalancedModelSpreadsKeysAndSticks(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upA, upB := newCountingUpstream(t), newCountingUpstream(t)
	svc := newSvc(t, db)

	pA := createProvider(t, db, "pa", upA.server.URL)
	pB := createProvider(t, db, "pb", upB.server.URL)
	createProviderKey(t, db, svc.secrets, pA.ID, "sk-a", "ka", 1, true)
	createProviderKey(t, db, svc.secrets, pB.ID, "sk-b", "kb", 1, true)
	m := createModelAndCandidate(t, db, pA, "gpt-4o", "gpt-4o-real", false, false, 1)
	addCandidateToModel(t, db, m, pB, "gpt-4o-real", 2)
	setModelSchedulingMode(t, db, m.ID, model.ModelSchedulingModeBalanced)

	key1 := createDistinctAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID}, "sk-balanced-key-1")
	key2 := createDistinctAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID}, "sk-balanced-key-2")

	relayOK(t, svc, key1, "gpt-4o")
	a1, b1 := upA.hits.Load(), upB.hits.Load()
	if a1+b1 != 1 {
		t.Fatalf("first relay dispatched %d times, want exactly 1", a1+b1)
	}

	relayOK(t, svc, key2, "gpt-4o")
	a2, b2 := upA.hits.Load(), upB.hits.Load()
	if a2+b2 != 2 {
		t.Fatalf("second relay dispatched %d times total, want 2", a2+b2)
	}
	if (a2 - a1) == (b2 - b1) {
		t.Fatalf("both keys landed on the same provider (A hits %d, B hits %d) — no spread", a2, b2)
	}

	// Stickiness: repeating each key's request must not move either one.
	// key1 started on the provider that took request 1, key2 on the other;
	// after two more key1 requests and one more key2 request, each provider's
	// growth must match exactly that ownership.
	key1OnA := a1 == 1
	relayOK(t, svc, key1, "gpt-4o")
	relayOK(t, svc, key1, "gpt-4o")
	relayOK(t, svc, key2, "gpt-4o")
	a3, b3 := upA.hits.Load(), upB.hits.Load()
	if a3+b3 != 5 {
		t.Fatalf("five relays produced %d dispatches, want 5", a3+b3)
	}
	var wantA, wantB int64
	if key1OnA {
		wantA, wantB = a2+2, b2+1
	} else {
		wantA, wantB = a2+1, b2+2
	}
	if a3 != wantA || b3 != wantB {
		t.Fatalf("a bound key changed providers (A %d→%d want %d, B %d→%d want %d)", a2, a3, wantA, b2, b3, wantB)
	}
}

// The bound provider dies (breaker opens): the next request from the bound
// key must reassign to the healthy provider and never dispatch to the dead
// one again — while still succeeding, and while the binding counts reflect
// the move.
func TestBalancedModelReassignsWhenBoundProviderDies(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upDead, upLive := newFailingUpstream(t), newCountingUpstream(t)

	g := testGatewayConfig()
	g.CircuitFailureThreshold = 1 // one hard fault opens the breaker
	svc := newSvcWithGateway(t, db, g)

	pDead := createProvider(t, db, "pdead", upDead.server.URL)
	pLive := createProvider(t, db, "plive", upLive.server.URL)
	createProviderKey(t, db, svc.secrets, pDead.ID, "sk-dead", "kd", 1, true)
	createProviderKey(t, db, svc.secrets, pLive.ID, "sk-live", "kl", 1, true)
	m := createModelAndCandidate(t, db, pDead, "gpt-4o", "gpt-4o-real", false, false, 1)
	addCandidateToModel(t, db, m, pLive, "gpt-4o-real", 2)
	setModelSchedulingMode(t, db, m.ID, model.ModelSchedulingModeBalanced)
	key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	// First request: binding lands on the chain head (the soon-to-die
	// provider), which answers 500, opening the breaker; the chain fails
	// over to the live provider and the caller still gets 200.
	relayOK(t, svc, key, "gpt-4o")
	if got := upDead.hits.Load(); got != 1 {
		t.Fatalf("dead provider hit %d times on the first relay, want 1", got)
	}
	if !svc.breaker.IsOpen(pDead.ID, pDead.DestinationVersion) {
		t.Fatal("breaker did not open after the configured one-fault threshold")
	}

	// Second request from the same key: the binding must be gone from the
	// dead provider — no dispatch to it, success straight from the live one.
	// The attempt ledger is inspected too: reordering changed which candidate
	// the chain ENTERS, not how attempts are booked — a request served by the
	// bound head records exactly one attempt and no skip rows for the rest.
	var lastAttempts []AttemptRecord
	prevHook := testHookHandleDone
	testHookHandleDone = func(rc *Exchange) { lastAttempts = append([]AttemptRecord(nil), rc.attempts...) }
	defer func() { testHookHandleDone = prevHook }()

	relayOK(t, svc, key, "gpt-4o")
	if got := upDead.hits.Load(); got != 1 {
		t.Fatalf("dead provider was dispatched to again after its breaker opened (%d hits)", got)
	}
	if len(lastAttempts) != 1 {
		t.Fatalf("reassigned request recorded %d attempts, want exactly 1 (the live head): %+v", len(lastAttempts), lastAttempts)
	}
	relayOK(t, svc, key, "gpt-4o")
	if got := upDead.hits.Load(); got != 1 {
		t.Fatalf("dead provider was dispatched to again after its breaker opened (%d hits)", got)
	}
	if got := upLive.hits.Load(); got != 3 {
		t.Fatalf("live provider served %d of 3 requests, want all 3", got)
	}

	counts := svc.Bindings().BindingCounts(m.ID)
	if counts[pDead.ID] != 0 {
		t.Fatalf("dead provider still holds %d bindings", counts[pDead.ID])
	}
	if counts[pLive.ID] != 1 {
		t.Fatalf("live provider holds %d bindings, want the reassigned 1", counts[pLive.ID])
	}
}

// The failover nail: a model that never opted into balancing must not touch
// the registry — every key walks sort_order from the head, and the binding
// counts stay empty. If balanced scheduling ever leaked into the default
// path, this is the test that says so.
func TestFailoverModelNeverConsultsBindings(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upA, upB := newCountingUpstream(t), newCountingUpstream(t)
	svc := newSvc(t, db)

	pA := createProvider(t, db, "pa", upA.server.URL)
	pB := createProvider(t, db, "pb", upB.server.URL)
	createProviderKey(t, db, svc.secrets, pA.ID, "sk-a", "ka", 1, true)
	createProviderKey(t, db, svc.secrets, pB.ID, "sk-b", "kb", 1, true)
	// Default mode: scheduling_mode stays empty (the pre-migration value).
	m := createModelAndCandidate(t, db, pA, "gpt-4o", "gpt-4o-real", false, false, 1)
	addCandidateToModel(t, db, m, pB, "gpt-4o-real", 2)

	key1 := createDistinctAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID}, "sk-balanced-key-1")
	key2 := createDistinctAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID}, "sk-balanced-key-2")
	for i := 0; i < 3; i++ {
		relayOK(t, svc, key1, "gpt-4o")
		relayOK(t, svc, key2, "gpt-4o")
	}

	if got := upA.hits.Load(); got != 6 {
		t.Fatalf("chain head served %d of 6 requests, want all 6 (failover = head takes everything)", got)
	}
	if got := upB.hits.Load(); got != 0 {
		t.Fatalf("healthy chain tail was dispatched to %d times under failover, want 0", got)
	}
	if counts := svc.Bindings().BindingCounts(m.ID); len(counts) != 0 {
		t.Fatalf("failover model acquired bindings %v; the registry must not be consulted", counts)
	}
}

// A reordered chain keeps the walk's attempt accounting: when the bound head
// fails and the walk passes a breaker-open candidate, that candidate still
// books its skip row (and the probe charge the same helper spends) exactly as
// a failover chain would. Reordering changes only where the walk enters —
// if it ever FILTERED the chain instead, the skip row would vanish and this
// test goes red.
func TestBalancedReorderKeepsSkipRowAccounting(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upA := newFailingUpstream(t)
	// upB flips to failing mid-test: healthy while the binding forms, dead for
	// the request whose ledger is inspected.
	var bFails atomic.Bool
	upB := &countingUpstream{}
	upB.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upB.hits.Add(1)
		if bFails.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream fell over"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(balancedOKBody))
	}))
	t.Cleanup(upB.server.Close)

	g := testGatewayConfig()
	g.CircuitFailureThreshold = 1
	svc := newSvcWithGateway(t, db, g)

	pA := createProvider(t, db, "pa", upA.server.URL)
	pB := createProvider(t, db, "pb", upB.server.URL)
	createProviderKey(t, db, svc.secrets, pA.ID, "sk-a", "ka", 1, true)
	createProviderKey(t, db, svc.secrets, pB.ID, "sk-b", "kb", 1, true)
	m := createModelAndCandidate(t, db, pA, "gpt-4o", "gpt-4o-real", false, false, 1)
	addCandidateToModel(t, db, m, pB, "gpt-4o-real", 2)
	setModelSchedulingMode(t, db, m.ID, model.ModelSchedulingModeBalanced)

	// First request binds the chain head A, which answers 500 and opens its
	// one-fault breaker; the walk fails over to B and the caller gets 200.
	key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})
	relayOK(t, svc, key, "gpt-4o")
	if !svc.breaker.IsOpen(pA.ID, pA.DestinationVersion) {
		t.Fatal("breaker did not open after the configured one-fault threshold")
	}
	// Second request reassigns the binding to B (A is dead), so the key's
	// chain now ENTERS at B with A demoted to second.
	relayOK(t, svc, key, "gpt-4o")

	// B dies too. The reordered walk must attempt B (a real dispatch), then
	// pass A's open breaker WITH a skip row — not silently drop it — and
	// exhaust the chain.
	bFails.Store(true)
	var lastAttempts []AttemptRecord
	prevHook := testHookHandleDone
	testHookHandleDone = func(rc *Exchange) { lastAttempts = append([]AttemptRecord(nil), rc.attempts...) }
	defer func() { testHookHandleDone = prevHook }()

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, key)
	if w.Code == http.StatusOK {
		t.Fatalf("relay succeeded with every provider failing; body = %s", w.Body.String())
	}
	if len(lastAttempts) != 2 {
		t.Fatalf("reordered exhausted walk recorded %d attempt rows, want 2 (B's dispatch + A's skip): %+v", len(lastAttempts), lastAttempts)
	}
	if lastAttempts[0].ProviderID != pB.ID || lastAttempts[0].UpstreamURL == "" {
		t.Fatalf("first row should be B's dispatched attempt, got %+v", lastAttempts[0])
	}
	if lastAttempts[1].ProviderID != pA.ID || lastAttempts[1].FailReason != skipReasonCircuitRefused || lastAttempts[1].UpstreamURL != "" {
		t.Fatalf("second row should be A's circuit-open skip, got %+v", lastAttempts[1])
	}
}

// The bound provider keeps its candidate enabled and verified but loses its
// only usable upstream key — a dead end the circuit breaker never sees, so
// the sticky check alone would keep the binding forever while every request
// burns a probe on it and the counts credit a provider serving nothing. The
// pre-dispatch skip must release the binding.
func TestBalancedBoundKeylessProviderMovesBinding(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upA, upB := newCountingUpstream(t), newCountingUpstream(t)
	svc := newSvc(t, db)

	pA := createProvider(t, db, "pa", upA.server.URL)
	pB := createProvider(t, db, "pb", upB.server.URL)
	createProviderKey(t, db, svc.secrets, pA.ID, "sk-a", "ka", 1, true)
	createProviderKey(t, db, svc.secrets, pB.ID, "sk-b", "kb", 1, true)
	m := createModelAndCandidate(t, db, pA, "gpt-4o", "gpt-4o-real", false, false, 1)
	addCandidateToModel(t, db, m, pB, "gpt-4o-real", 2)
	setModelSchedulingMode(t, db, m.ID, model.ModelSchedulingModeBalanced)
	key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	// First request binds the chain head A and is served by it.
	relayOK(t, svc, key, "gpt-4o")
	if got := upA.hits.Load(); got != 1 {
		t.Fatalf("first relay hit A %d times, want 1", got)
	}

	// The admin disables A's only upstream key. Candidate and provider stay
	// enabled+verified, so the routable filter and the breaker both still
	// treat A as a valid target.
	if err := db.Model(&model.ProviderKey{}).Where("provider_id = ?", pA.ID).
		Update("management_status", model.ProviderKeyStatusDisabled).Error; err != nil {
		t.Fatalf("disable A's key: %v", err)
	}

	// Second request: A is skipped before dispatch, B serves, and the
	// binding must MOVE — released from the keyless provider and re-made on
	// the one that actually served, or the released binding would leave the
	// counts tied and the next request would tie-break straight back onto
	// the dead end.
	relayOK(t, svc, key, "gpt-4o")
	if got := upA.hits.Load(); got != 1 {
		t.Fatalf("keyless provider was dispatched to (%d hits)", got)
	}
	if got := upB.hits.Load(); got != 1 {
		t.Fatalf("healthy provider served %d requests, want 1", got)
	}
	counts := svc.Bindings().BindingCounts(m.ID)
	if counts[pA.ID] != 0 {
		t.Fatalf("keyless provider still credited with %d bindings", counts[pA.ID])
	}
	if counts[pB.ID] != 1 {
		t.Fatalf("serving provider credited with %d bindings, want the rebound 1", counts[pB.ID])
	}

	// Third request: sticky on the rebound provider — it enters at B with no
	// dead-end skip of A, so the probe burn does not repeat per request.
	var lastAttempts []AttemptRecord
	prevHook := testHookHandleDone
	testHookHandleDone = func(rc *Exchange) { lastAttempts = append([]AttemptRecord(nil), rc.attempts...) }
	defer func() { testHookHandleDone = prevHook }()
	relayOK(t, svc, key, "gpt-4o")
	if len(lastAttempts) != 1 || lastAttempts[0].ProviderID != pB.ID {
		t.Fatalf("request after rebind should enter at the rebound provider without re-probing the dead end, got %+v", lastAttempts)
	}

	// A second, distinct caller arriving inside the dead end's quarantine
	// window must not be fed into it either — its FIRST assignment goes
	// straight to the serving provider, no probe burned rediscovering the
	// dead end the first caller already proved.
	key2 := createDistinctAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID}, "sk-balanced-keyless-2")
	relayOK(t, svc, key2, "gpt-4o")
	if len(lastAttempts) != 1 || lastAttempts[0].ProviderID != pB.ID {
		t.Fatalf("a new key was fed into the quarantined dead end, attempts: %+v", lastAttempts)
	}
}

// A terminal upstream answer (a 4xx that is the caller's own to act on) still
// proves the serving provider reachable: a request whose binding was released
// as a dead end re-binds to that provider even though the caller saw an
// error — otherwise callers whose traffic consistently ends that way would
// re-probe the dead end on every request.
func TestBalancedRebindHappensOnTerminalAnswerToo(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upA := newCountingUpstream(t)
	// upB answers every request with a terminal 400.
	upB := &countingUpstream{}
	upB.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upB.hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	t.Cleanup(upB.server.Close)

	svc := newSvc(t, db)
	pA := createProvider(t, db, "pa", upA.server.URL)
	pB := createProvider(t, db, "pb", upB.server.URL)
	createProviderKey(t, db, svc.secrets, pA.ID, "sk-a", "ka", 1, true)
	createProviderKey(t, db, svc.secrets, pB.ID, "sk-b", "kb", 1, true)
	m := createModelAndCandidate(t, db, pA, "gpt-4o", "gpt-4o-real", false, false, 1)
	addCandidateToModel(t, db, m, pB, "gpt-4o-real", 2)
	setModelSchedulingMode(t, db, m.ID, model.ModelSchedulingModeBalanced)
	key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	// Bind A, then take its only key away.
	relayOK(t, svc, key, "gpt-4o")
	if err := db.Model(&model.ProviderKey{}).Where("provider_id = ?", pA.ID).
		Update("management_status", model.ProviderKeyStatusDisabled).Error; err != nil {
		t.Fatalf("disable A's key: %v", err)
	}

	// A is a dead end, B answers with the terminal 400. The caller gets the
	// 400 — and the binding still moves to B.
	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, key)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("terminal answer status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	counts := svc.Bindings().BindingCounts(m.ID)
	if counts[pA.ID] != 0 || counts[pB.ID] != 1 {
		t.Fatalf("terminal answer did not move the binding (counts %v, want B holding 1)", counts)
	}
}

// EVERY dead end observed on a walk quarantines its provider, not just the
// caller's bound one: with A and B both keyless and C healthy, the first
// caller's walk marks both, and the next distinct key's first assignment
// goes straight to C — no probe burned rediscovering either dead end.
func TestBalancedWalkQuarantinesEveryDeadEnd(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upA, upB, upC := newCountingUpstream(t), newCountingUpstream(t), newCountingUpstream(t)
	svc := newSvc(t, db)

	pA := createProvider(t, db, "pa", upA.server.URL)
	pB := createProvider(t, db, "pb", upB.server.URL)
	pC := createProvider(t, db, "pc", upC.server.URL)
	createProviderKey(t, db, svc.secrets, pA.ID, "sk-a", "ka", 1, true)
	createProviderKey(t, db, svc.secrets, pB.ID, "sk-b", "kb", 1, true)
	createProviderKey(t, db, svc.secrets, pC.ID, "sk-c", "kc", 1, true)
	m := createModelAndCandidate(t, db, pA, "gpt-4o", "gpt-4o-real", false, false, 1)
	addCandidateToModel(t, db, m, pB, "gpt-4o-real", 2)
	addCandidateToModel(t, db, m, pC, "gpt-4o-real", 3)
	setModelSchedulingMode(t, db, m.ID, model.ModelSchedulingModeBalanced)

	// Both A and B lose their only keys before any binding exists.
	for _, pid := range []uint{pA.ID, pB.ID} {
		if err := db.Model(&model.ProviderKey{}).Where("provider_id = ?", pid).
			Update("management_status", model.ProviderKeyStatusDisabled).Error; err != nil {
			t.Fatalf("disable keys for provider %d: %v", pid, err)
		}
	}

	// First caller walks past both dead ends and is served by C.
	key1 := createDistinctAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID}, "sk-balanced-deadends-1")
	relayOK(t, svc, key1, "gpt-4o")
	if got := upC.hits.Load(); got != 1 {
		t.Fatalf("healthy tail served %d requests, want 1", got)
	}

	// Second caller must not be fed into either quarantined dead end.
	var lastAttempts []AttemptRecord
	prevHook := testHookHandleDone
	testHookHandleDone = func(rc *Exchange) { lastAttempts = append([]AttemptRecord(nil), rc.attempts...) }
	defer func() { testHookHandleDone = prevHook }()
	key2 := createDistinctAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID}, "sk-balanced-deadends-2")
	relayOK(t, svc, key2, "gpt-4o")
	if len(lastAttempts) != 1 || lastAttempts[0].ProviderID != pC.ID {
		t.Fatalf("second key should enter straight at the healthy provider, got %+v", lastAttempts)
	}
}

// A walk that finds NO server — the bound candidate is a dead end and every
// alternative is breaker-refused — must leave the caller's binding intact:
// the replacement is deferred to a candidate actually serving, and deleting
// the affinity on a total failure would strip the caller of its spot (and
// its upstream cache) for nothing.
func TestBalancedFailedWalkKeepsBinding(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upA, upB := newCountingUpstream(t), newCountingUpstream(t)
	g := testGatewayConfig()
	g.CircuitFailureThreshold = 1
	svc := newSvcWithGateway(t, db, g)

	pA := createProvider(t, db, "pa", upA.server.URL)
	pB := createProvider(t, db, "pb", upB.server.URL)
	createProviderKey(t, db, svc.secrets, pA.ID, "sk-a", "ka", 1, true)
	createProviderKey(t, db, svc.secrets, pB.ID, "sk-b", "kb", 1, true)
	m := createModelAndCandidate(t, db, pA, "gpt-4o", "gpt-4o-real", false, false, 1)
	addCandidateToModel(t, db, m, pB, "gpt-4o-real", 2)
	setModelSchedulingMode(t, db, m.ID, model.ModelSchedulingModeBalanced)
	key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	// Bind A, then make A keyless and open B's breaker directly.
	relayOK(t, svc, key, "gpt-4o")
	if err := db.Model(&model.ProviderKey{}).Where("provider_id = ?", pA.ID).
		Update("management_status", model.ProviderKeyStatusDisabled).Error; err != nil {
		t.Fatalf("disable A's key: %v", err)
	}
	_, gen := svc.breaker.Allow(pB.ID, pB.DestinationVersion)
	svc.breaker.RecordFailure(pB.ID, gen)

	// The walk exhausts: A dead-end skipped, B circuit-refused. The caller
	// gets the failure — and keeps its binding.
	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, key)
	if w.Code == http.StatusOK {
		t.Fatalf("walk with no usable candidate succeeded; body = %s", w.Body.String())
	}
	counts := svc.Bindings().BindingCounts(m.ID)
	if counts[pA.ID] != 1 {
		t.Fatalf("failed walk dropped the binding (counts %v, want A still holding 1)", counts)
	}
}

// A walk that ends because the CLIENT hung up proves nothing about the
// fallback candidate: the binding must stay where it was, not migrate to a
// provider whose only evidence is a request nobody waited for.
func TestBalancedClientDisconnectDoesNotRebind(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upA := newCountingUpstream(t)
	// upB cancels the caller's context while serving — the client hangs up
	// mid-dispatch on the fallback.
	var cancelClient atomic.Value
	upB := &countingUpstream{}
	upB.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upB.hits.Add(1)
		if cf, ok := cancelClient.Load().(context.CancelFunc); ok && cf != nil {
			cf()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(balancedOKBody))
	}))
	t.Cleanup(upB.server.Close)

	svc := newSvc(t, db)
	pA := createProvider(t, db, "pa", upA.server.URL)
	pB := createProvider(t, db, "pb", upB.server.URL)
	createProviderKey(t, db, svc.secrets, pA.ID, "sk-a", "ka", 1, true)
	createProviderKey(t, db, svc.secrets, pB.ID, "sk-b", "kb", 1, true)
	m := createModelAndCandidate(t, db, pA, "gpt-4o", "gpt-4o-real", false, false, 1)
	addCandidateToModel(t, db, m, pB, "gpt-4o-real", 2)
	setModelSchedulingMode(t, db, m.ID, model.ModelSchedulingModeBalanced)
	key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	// Bind A, then take its key away so the next walk flags the binding.
	relayOK(t, svc, key, "gpt-4o")
	if err := db.Model(&model.ProviderKey{}).Where("provider_id = ?", pA.ID).
		Update("management_status", model.ProviderKeyStatusDisabled).Error; err != nil {
		t.Fatalf("disable A's key: %v", err)
	}

	// The caller hangs up while B serves: whatever the attempt is classified
	// as, by the time the walk concludes the client is gone — no rebind.
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	cancelClient.Store(context.CancelFunc(cancel))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	svc.Handle(c, key)

	counts := svc.Bindings().BindingCounts(m.ID)
	if counts[pA.ID] != 1 || counts[pB.ID] != 0 {
		t.Fatalf("client disconnect migrated the binding (counts %v, want A still holding 1)", counts)
	}
}

// A half-open provider turns away all but one probe per interval by design.
// That refusal must NOT release the caller's binding: the recovering
// provider would be stripped of every binding it just attracted, and keys
// would thrash on and off it for the whole recovery window. Only dead ends
// the breaker cannot see release bindings.
func TestBalancedHalfOpenRefusalKeepsBinding(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	// upA fails until flipped healthy — the provider that dies and recovers.
	var aFails atomic.Bool
	aFails.Store(true)
	upA := &countingUpstream{}
	upA.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upA.hits.Add(1)
		if aFails.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream fell over"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(balancedOKBody))
	}))
	t.Cleanup(upA.server.Close)
	upB := newCountingUpstream(t)

	svc := newSvc(t, db)
	// A movable clock lets the test cross the open window without sleeping.
	cur := time.Now()
	svc.breaker = circuit.NewWithClock(
		circuit.Config{FailureThreshold: 1, SuccessThreshold: 2, OpenTimeout: time.Minute},
		func() time.Time { return cur },
	)

	pA := createProvider(t, db, "pa", upA.server.URL)
	pB := createProvider(t, db, "pb", upB.server.URL)
	createProviderKey(t, db, svc.secrets, pA.ID, "sk-a", "ka", 1, true)
	createProviderKey(t, db, svc.secrets, pB.ID, "sk-b", "kb", 1, true)
	m := createModelAndCandidate(t, db, pA, "gpt-4o", "gpt-4o-real", false, false, 1)
	addCandidateToModel(t, db, m, pB, "gpt-4o-real", 2)
	setModelSchedulingMode(t, db, m.ID, model.ModelSchedulingModeBalanced)
	key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	// Bind A; its failure opens the one-fault breaker and B serves.
	relayOK(t, svc, key, "gpt-4o")
	if got := upA.hits.Load(); got != 1 {
		t.Fatalf("first relay hit A %d times, want 1", got)
	}

	// A recovers; the open window elapses. The sticky binding is still A, and
	// the first request through is admitted as the half-open probe and served
	// by A directly.
	aFails.Store(false)
	cur = cur.Add(61 * time.Second)
	relayOK(t, svc, key, "gpt-4o")
	if got := upA.hits.Load(); got != 2 {
		t.Fatalf("recovered provider served %d dispatches, want 2 (the probe)", got)
	}

	// Within the same probe interval the breaker refuses the next admission.
	// The request must fail over to B — and the binding must survive.
	relayOK(t, svc, key, "gpt-4o")
	if got := upA.hits.Load(); got != 2 {
		t.Fatalf("half-open provider was dispatched past its probe budget (%d hits)", got)
	}
	if got := upB.hits.Load(); got != 2 {
		t.Fatalf("fallback provider served %d requests, want 2", got)
	}
	counts := svc.Bindings().BindingCounts(m.ID)
	if counts[pA.ID] != 1 {
		t.Fatalf("half-open refusal released the binding (A credited with %d, want 1)", counts[pA.ID])
	}
}
