package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// seedCandidate adds one enabled candidate for an existing model onto the
// given provider — the shape every test in this file routes through, so the
// only knobs are the ones the tests vary.
func seedCandidate(t *testing.T, db *gorm.DB, modelID, providerID uint, providerModelName string, order, verification int) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.Create(&model.ModelCandidate{
		ModelID: modelID, ProviderID: providerID, ProviderModelName: providerModelName,
		InputPrice: 1.0, OutputPrice: 2.0, MaxOutput: 4096,
		SupportsStreaming: boolPtr(true), SupportsFunctionCalling: boolPtr(true),
		ManagementStatus:   model.ModelCandidateStatusEnabled,
		SortOrder:          order,
		VerificationStatus: verification,
		CreatedAt:          now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed candidate (provider %d, order %d): %v", providerID, order, err)
	}
}

// TestDisabledProviderCandidatesDoNotEnterTheChain pins the selection-side
// provider gate. A provider the operator switched off is configuration that
// was deliberately turned down; its candidates must not enter the chain at
// all, because a chain position is not free: each one is walked at relay
// time, spending a probe of the request budget and writing a "provider
// disabled" attempt row. That is noise in the audit trail, and with enough
// such candidates sorted ahead of a live provider it can spend the whole
// budget before the request ever reaches the provider that would serve it.
//
// The pinned shape: three disabled providers sorted ahead of the live one.
// Without the selection-side gate such a request records three skip rows and
// only then succeeds — the disabled providers effectively participate in
// routing. The pin is one attempt and zero arrivals at the disabled
// providers' upstream.
func TestDisabledProviderCandidatesDoNotEnterTheChain(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	var disabledHits atomic.Int32
	disabledUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		disabledHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer disabledUpstream.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	now := time.Now().UTC()
	m := &model.Model{Name: "gpt-4o", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	// Three providers the operator switched off, sorted ahead of the live
	// one, each with a key — so the only thing that can keep them out of the
	// chain is the provider switch itself.
	for i := 1; i <= 3; i++ {
		p := createProvider(t, db, fmt.Sprintf("disabled-%d", i), disabledUpstream.URL)
		disableProvider(t, db, p.ID)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-disabled", fmt.Sprintf("k-%d", i), 1, true)
		seedCandidate(t, db, m.ID, p.ID, "real-model", i, model.ModelVerificationStatusPassed)
	}
	live := createProvider(t, db, "live", upstream.URL)
	createProviderKey(t, db, svc.secrets, live.ID, "sk-live", "k-live", 1, true)
	seedCandidate(t, db, m.ID, live.ID, "real-model", 4, model.ModelVerificationStatusPassed)

	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 served by the live provider; body = %s", w.Code, w.Body.String())
	}
	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if len(captured.attempts) != 1 {
		t.Fatalf("attempts = %d, want 1 — disabled providers' candidates must not enter the chain: %+v",
			len(captured.attempts), captured.attempts)
	}
	if n := disabledHits.Load(); n != 0 {
		t.Fatalf("disabled providers received %d upstream requests, want 0", n)
	}
}

// TestAllRoutesOnDisabledProvidersSaysNoEnabledRoute pins the rejection
// answer when every candidate sits on a switched-off provider: the same 503
// the gateway gives when the candidates' own switches are off ("no enabled
// route" — configuration an operator turned down), not the 502 "all upstream
// candidates failed" that reports an upstream outage nobody had.
func TestAllRoutesOnDisabledProvidersSaysNoEnabledRoute(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	p := createProvider(t, db, "switched-off", "http://127.0.0.1:0")
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	disableProvider(t, db, p.ID)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no enabled route") {
		t.Fatalf("body = %s, want the switched-off-configuration message", w.Body.String())
	}
}

// TestDisabledProviderRoutePlusUnverifiedSaysNotVerifiedYet pins which of the
// two 503 answers wins when both causes are present at once: one candidate on
// a switched-off provider (its own switches on and verified — but the
// provider gate counts it toward nothing) and one on an enabled provider that
// has not passed verification (which does set anyEnabled). The unverified
// route is the actionable state — running or waiting out the probe can still
// turn this model up — so that is the answer the 503 must give, not the
// switched-off one that tells the operator to re-enable what is already on.
func TestDisabledProviderRoutePlusUnverifiedSaysNotVerifiedYet(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	off := createProvider(t, db, "switched-off", "http://127.0.0.1:0")
	createProviderKey(t, db, svc.secrets, off.ID, "sk-off", "k-off", 1, true)
	m := createModelAndCandidate(t, db, off, "gpt-4o", "gpt-4o-real", true, true, 1)
	disableProvider(t, db, off.ID)

	live := createProvider(t, db, "pending-probe", "http://127.0.0.1:0")
	createProviderKey(t, db, svc.secrets, live.ID, "sk-live", "k-live", 1, true)
	seedCandidate(t, db, m.ID, live.ID, "gpt-4o-real", 2, model.ModelVerificationStatusUntested)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "routes not verified yet") {
		t.Fatalf("body = %s, want the pending-verification message the live route can still clear", w.Body.String())
	}
}

// TestRelayCandidatesGuardsUnfilteredSlice hands relayCandidates a slice
// filterCandidates never produces — one candidate with no preloaded provider,
// one on a switched-off provider — and pins the loop's own guards: each
// candidate is skipped with its own attempt row and nothing is dispatched.
// Production always filters first, so these rows are unreachable there; this
// direct call is what keeps the guards honest for any caller that skips the
// filter.
func TestRelayCandidatesGuardsUnfilteredSlice(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	off := createProvider(t, db, "switched-off", "http://127.0.0.1:0")
	createProviderKey(t, db, svc.secrets, off.ID, "sk-off", "k-off", 1, true)
	m := createModelAndCandidate(t, db, off, "gpt-4o", "gpt-4o-real", true, true, 1)
	disableProvider(t, db, off.ID)

	var onDisabled model.ModelCandidate
	if err := db.Where("model_id = ?", m.ID).First(&onDisabled).Error; err != nil {
		t.Fatalf("load seeded candidate: %v", err)
	}
	off.ManagementStatus = model.ProviderStatusDisabled
	onDisabled.Provider = off
	noProvider := onDisabled
	noProvider.Provider = nil

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	rc := &Exchange{
		requestID:       "test-req-id",
		requestCtx:      c.Request.Context(),
		requestDeadline: time.Now().Add(time.Hour),
		originalModel:   "gpt-4o",
	}
	payload, rej := NewTextModality().Admit(context.Background(), Ingress{
		Protocol: protocols.ProtocolOpenAI, Path: "/v1/chat/completions",
		Body: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
	})
	if rej != nil {
		t.Fatalf("Admit refused a valid body: %+v", rej)
	}
	svc.relayCandidates(c, rc, admitted{payload: newOrderedPayload(payload, rc.requestID)},
		[]model.ModelCandidate{noProvider, onDisabled}, time.Now())

	if len(rc.attempts) != 2 {
		t.Fatalf("attempts = %+v, want exactly the two guard skip rows", rc.attempts)
	}
	for i, want := range []string{"provider missing (preload)", "provider disabled"} {
		if rc.attempts[i].Outcome != AttemptBadStatus || rc.attempts[i].FailReason != want {
			t.Fatalf("attempt %d = %+v, want outcome %q with reason %q",
				i, rc.attempts[i], AttemptBadStatus, want)
		}
	}
}
