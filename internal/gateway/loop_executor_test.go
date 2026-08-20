package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/decision"
	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"

	"gorm.io/gorm"
)

// verdictObserver reports one fixed Kind for every non-2xx response it sees.
// It stands in for any capability whose judgement should out-rank the kernel's
// own status-line reading.
type verdictObserver struct{ kind fact.Kind }

func (verdictObserver) Name() string { return "test_verdict" }

func (o verdictObserver) ObserveUpstreamError(_ context.Context, _ struct{}, up fact.Upstream, sink fact.Sink) {
	sink.Report(fact.Fact{Kind: o.kind, Status: up.StatusCode})
}

func registerVerdictObserver(svc *Service, kind fact.Kind) {
	RegisterUpstreamErrorObserver(svc, verdictObserver{kind: kind},
		func(*Exchange) struct{} { return struct{}{} })
}

// The kernel's two readings of a status — the routing fact and the label
// classification — must agree on what each status means. They are separate
// functions because they answer different questions (where the chain goes vs
// what the row is called), and this sweep is what keeps a status added to one
// from silently meaning something else in the other.
func TestKindForUpstreamStatusAgreesWithClassification(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 409, 418, 422, 429, 451, 500, 501, 502, 503, 504, 599} {
		kind := kindForUpstreamStatus(status)
		row := decision.For(kind)
		if !row.Defined {
			t.Errorf("status %d: kind %v has no decision row", status, kind)
			continue
		}
		var want decision.LoopEffect
		switch classifyUpstreamStatus(status).Category {
		case statusRotateKey:
			want = decision.LoopRotateKey
		case statusFailover:
			want = decision.LoopNextCandidate
		default:
			want = decision.LoopTerminate
		}
		if row.Loop != want {
			t.Errorf("status %d: table routes %v as Loop=%d, classification implies %d",
				status, kind, row.Loop, want)
		}
	}
}

// The kernel's baseline facts state their persisted reason codes explicitly.
// The strings happen to match the Kinds' log names today, so a behaviour test
// cannot tell the two apart — but the persisted column is a contract with
// dashboards, and a Kind rename must not silently change it. This pins the
// declaration itself.
func TestKernelUpstreamFactStatesItsPersistedReasonsExplicitly(t *testing.T) {
	if got := kernelUpstreamFact(http.StatusTooManyRequests).Reason; got != "upstream_rate_limited" {
		t.Errorf("429 reason = %q, want upstream_rate_limited", got)
	}
	if got := kernelUpstreamFact(http.StatusUnprocessableEntity).Reason; got != "upstream_client_error_422" {
		t.Errorf("422 reason = %q, want upstream_client_error_422", got)
	}
}

// A chain exhausted on rate limits must tell the caller it was rate limited.
// The kernel's own baseline fact carries the table's sticky effect, so the
// terminal quotes the 429 instead of the generic 502 that reads like an
// outage — sending the caller to back off rather than to hunt for a broken
// provider.
func TestUpstreamRateLimitExhaustionQuotesTheRateLimit(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	apiKey := seedTwoCandidateModel(t, svc, db, upstream.URL)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 quoted from the exhausted chain; body = %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "rate_limit_error") {
		t.Fatalf("body does not name the rate limit: %s", body)
	}
	var log model.RequestLog
	if err := db.First(&log).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if log.FailReason == nil || *log.FailReason != "upstream_rate_limited" {
		t.Errorf("fail_reason = %v, want upstream_rate_limited", strOrNil(log.FailReason))
	}
}

// An observer that read the body out-ranks the kernel's status-line baseline:
// a refusal reported on a 429 moves the chain to the next candidate instead of
// rotating keys inside a provider whose moderation already judged the payload.
func TestObserverVerdictOutranksTheKernelBaseline(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu   sync.Mutex
		seen []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, extractModelFromJSON(t, body))
		mu.Unlock()
		if strings.Contains(string(body), `"model":"c1-model"`) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"throttled"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"c2-model","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	registerVerdictObserver(svc, fact.KindPayloadRefused)
	apiKey := seedTwoProvidersFirstWithTwoKeys(t, svc, db, upstream.URL)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after refusal failover; body = %s", w.Code, w.Body.String())
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	// The baseline alone would rotate to the first provider's second key
	// (c1-model twice); the refusal verdict must abandon the provider after
	// one attempt.
	if len(got) != 2 || got[0] != "c1-model" || got[1] != "c2-model" {
		t.Fatalf("expected attempts [c1-model, c2-model] (no key rotation), got %v", got)
	}
}

// A refusal's attempt row must be labelled as a judgement on the payload, not
// as a provider fault: the audit record is how an operator tells "the payload
// was moderated" apart from "the provider fell over", and the two demand
// opposite responses.
func TestRefusalRelabelsTheAttemptRecord(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"data_inspection_failed","message":"Input data may contain inappropriate content."}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	apiKey := seedTwoCandidateModel(t, svc, db, upstream.URL)

	c, _ := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	var log model.RequestLog
	if err := db.First(&log).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if log.AttemptsDetail == nil || !strings.Contains(*log.AttemptsDetail, AttemptContentFiltered) {
		t.Fatalf("attempts_detail does not label the refusal as %q: %v",
			AttemptContentFiltered, strOrNil(log.AttemptsDetail))
	}
}

// A retry-same verdict has no executor on this path yet. It must not stall or
// misroute the chain: the kernel logs it and routes by its own reading of the
// status — here a 400, which is terminal.
func TestRetrySameVerdictFallsBackToTheKernelRouting(t *testing.T) {
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
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (terminal fallback)", w.Code)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 upstream call (retry-same not executed), got %d", calls)
	}
}

// When retry-same falls back it must fall back to the WHOLE baseline
// decision, sticky included: a chain rate-limited to exhaustion under a
// repair offer still owes the caller the 429, not a generic 502.
func TestRetrySameFallbackKeepsTheBaselineSticky(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	registerVerdictObserver(svc, fact.KindPayloadRepairedRetrySame)
	apiKey := seedTwoCandidateModel(t, svc, db, upstream.URL)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 quoted despite the repair offer; body = %s", w.Code, w.Body.String())
	}
	var log model.RequestLog
	if err := db.First(&log).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if log.FailReason == nil || *log.FailReason != "upstream_rate_limited" {
		t.Errorf("fail_reason = %v, want upstream_rate_limited", strOrNil(log.FailReason))
	}
}

// A verdict can end the chain without carrying a status of its own — a row
// whose status policy defers to a peer, reported with no peer to defer to.
// The terminal must then answer with what the upstream actually said, not
// with a zero.
func TestTerminalVerdictWithoutAStatusAnswersWithTheUpstreams(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	registerVerdictObserver(svc, fact.KindPricingUnavailableTerminal)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the upstream's own 503; body = %s", w.Code, w.Body.String())
	}
}

// The persisted audit code for a surfaced client error is a contract with
// whatever reads the column: it carries the status, exactly as it did before
// the routing moved onto the decision table.
func TestSurfacedClientErrorKeepsItsAuditCode(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"message":"no"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 surfaced", w.Code)
	}
	var log model.RequestLog
	if err := db.First(&log).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if log.FailReason == nil || *log.FailReason != "upstream_client_error_422" {
		t.Errorf("fail_reason = %v, want upstream_client_error_422", strOrNil(log.FailReason))
	}
}

// seedTwoProvidersFirstWithTwoKeys wires one external model onto two
// providers where the FIRST provider holds two keys — the shape that tells a
// key rotation apart from a candidate failover.
func seedTwoProvidersFirstWithTwoKeys(t *testing.T, svc *Service, db *gorm.DB, upstreamURL string) *model.APIKey {
	t.Helper()
	p1 := createProvider(t, db, "p1", upstreamURL)
	createProviderKey(t, db, svc.secrets, p1.ID, "sk-1a", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p1.ID, "sk-1b", "k2", 2, true)
	p2 := createProvider(t, db, "p2", upstreamURL)
	createProviderKey(t, db, svc.secrets, p2.ID, "sk-2", "k1", 1, true)

	now := time.Now().UTC()
	m := &model.Model{Name: "gpt-4o", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	for i, p := range []*model.Provider{p1, p2} {
		name := "c1-model"
		if i == 1 {
			name = "c2-model"
		}
		if err := db.Create(&model.ModelCandidate{
			ModelID: m.ID, ProviderID: p.ID, ProviderModelName: name,
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

func strOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
