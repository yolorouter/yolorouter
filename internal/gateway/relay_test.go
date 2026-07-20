package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	ycrypto "github.com/yolorouter/yolorouter-ce/pkg/crypto"

	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/testutil"
)

// newRelaySvc builds a RelayService and swaps in a plain transport so the
// test can dial a loopback-bound httptest server — safehttp.NewTransport()
// (the production transport) deliberately refuses loopback (SSRF defense),
// which would block every test here. Same pattern as provider_client_test's
// newTestClient.
func newRelaySvc(t *testing.T, db *gorm.DB) *RelayService {
	t.Helper()
	masterKey := bytes.Repeat([]byte{0x42}, 32)
	svc := NewRelayService(db, masterKey)
	svc.client.httpClient.Transport = &http.Transport{}
	return svc
}

func newCtx(body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

func createProvider(t *testing.T, db *gorm.DB, name, baseURL string) *model.Provider {
	t.Helper()
	now := time.Now().UTC()
	p := &model.Provider{
		Name: name, ProviderType: "openai", BaseURL: baseURL,
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	return p
}

func createProviderKey(t *testing.T, db *gorm.DB, masterKey []byte, providerID uint, plaintext, label string, order int, enabled bool) {
	t.Helper()
	now := time.Now().UTC()
	enc, err := ycrypto.Encrypt(masterKey, plaintext)
	if err != nil {
		t.Fatalf("encrypt upstream key: %v", err)
	}
	status := model.ProviderKeyStatusEnabled
	if !enabled {
		status = model.ProviderKeyStatusDisabled
	}
	pk := &model.ProviderKey{
		ProviderID: providerID, Label: label, EncryptedKey: enc, KeyPrefix: plaintext,
		SortOrder: order, TestModel: "m", ManagementStatus: status,
		VerificationStatus:           model.VerificationStatusPassed,
		AuthorizedDestinationVersion: 1, ConfigVersion: 1, TestGeneration: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(pk).Error; err != nil {
		t.Fatalf("seed provider key: %v", err)
	}
}

func createModelAndCandidate(t *testing.T, db *gorm.DB, provider *model.Provider, externalName, providerModelName string, stream, fn bool, order int) *model.Model {
	t.Helper()
	now := time.Now().UTC()
	m := &model.Model{Name: externalName, ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	cand := &model.ModelCandidate{
		ModelID: m.ID, ProviderID: provider.ID, ProviderModelName: providerModelName,
		InputPrice: 1.0, OutputPrice: 2.0, MaxOutput: 4096,
		SupportsStreaming: stream, SupportsFunctionCalling: fn,
		ManagementStatus: model.ModelCandidateStatusEnabled, SortOrder: order,
		VerificationStatus: model.ModelVerificationStatusPassed,
		CreatedAt:          now, UpdatedAt: now,
	}
	if err := db.Create(cand).Error; err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	return m
}

func createAPIKey(t *testing.T, db *gorm.DB, status int, modelIDs []uint) *model.APIKey {
	t.Helper()
	now := time.Now().UTC()
	k := &model.APIKey{
		KeyHash: ycrypto.HashToken("sk-yr-test"), KeyPrefix: "sk-yr-test------",
		OwnerLabel: "tester", Status: status, CreatedAt: now, UpdatedAt: now,
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

// TestRelayNonStreamSuccess: a healthy upstream returns 200; the gateway
// rewrites the request model to the provider name, rewrites the response
// model back to the external name, extracts usage, and writes one log row.
func TestRelayNonStreamSuccess(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var sawAuth, sawUpstreamModel bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") == "Bearer sk-upstream-1"
		body, _ := io.ReadAll(r.Body)
		sawUpstreamModel = bytes.Contains(body, []byte(`"model":"gpt-4o-real"`))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	defer upstream.Close()

	svc := newRelaySvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-upstream-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !sawAuth {
		t.Error("upstream did not receive the decrypted bearer key")
	}
	if !sawUpstreamModel {
		t.Error("upstream body model was not rewritten to the provider name")
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"model":"gpt-4o"`)) {
		t.Errorf("response model not rewritten back to external name: %s", w.Body.String())
	}
	var logCount int64
	db.Model(&model.RequestLog{}).Count(&logCount)
	if logCount != 1 {
		t.Fatalf("expected 1 request_log row, got %d", logCount)
	}
}

// TestRelayKeyRotation (GATE-09): the first key gets a 401, so the gateway
// rotates to the second key on the same provider and succeeds.
func TestRelayKeyRotation(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer sk-bad" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	svc := newRelaySvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-bad", "bad", 1, true)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-good", "good", 2, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after rotation; body = %s", w.Code, w.Body.String())
	}
	if len(calls) != 2 || calls[0] != "Bearer sk-bad" || calls[1] != "Bearer sk-good" {
		t.Fatalf("expected calls [sk-bad, sk-good], got %v", calls)
	}
}

// TestRelayCandidateFailover (GATE-10): the first candidate's provider
// returns 500; the gateway fails over to the second candidate and succeeds.
// Each attempt must use its own candidate's provider_model_name.
func TestRelayCandidateFailover(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var seenModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenModels = append(seenModels, extractModelFromJSON(t, body))
		if bytes.Contains(body, []byte(`"model":"c1-model"`)) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"c2-model","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	svc := newRelaySvc(t, db)
	p1 := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p1.ID, "sk-1", "k1", 1, true)
	p2 := createProvider(t, db, "p2", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p2.ID, "sk-2", "k1", 1, true)
	// Both candidates back the same external model, different provider names.
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
			SupportsStreaming: true, SupportsFunctionCalling: true,
			ManagementStatus: model.ModelCandidateStatusEnabled, SortOrder: i + 1,
			VerificationStatus: model.ModelVerificationStatusPassed,
			CreatedAt:          now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatalf("seed candidate %d: %v", i, err)
		}
	}
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover; body = %s", w.Code, w.Body.String())
	}
	if len(seenModels) != 2 || seenModels[0] != "c1-model" || seenModels[1] != "c2-model" {
		t.Fatalf("expected attempts with [c1-model, c2-model], got %v", seenModels)
	}
	// GATE-14: each attempt used the current candidate's provider name.
	if !bytes.Contains(w.Body.Bytes(), []byte(`"model":"gpt-4o"`)) {
		t.Errorf("final response model not rewritten back to external: %s", w.Body.String())
	}
}

// TestRelayClientErrorNoSwitch (GATE-11): a 400 from the upstream is the
// caller's problem — no rotation, no failover, surfaced as-is.
func TestRelayClientErrorNoSwitch(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer upstream.Close()

	svc := newRelaySvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-1", "k1", 1, true)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-2", "k2", 2, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (no switch on client error)", w.Code)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 upstream call (no switch), got %d", calls)
	}
}

// TestRelayModelNotAllowed (GATE-02): a model outside the key's allowlist is
// rejected with 403 and never reaches the upstream.
func TestRelayModelNotAllowed(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstreamHit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
	}))
	defer upstream.Close()

	svc := newRelaySvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-1", "k1", 1, true)
	createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	// Key has an EMPTY allowlist — no model is permitted.
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, nil)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (model not in allowlist)", w.Code)
	}
	if upstreamHit {
		t.Error("upstream must not be called when the model is not in the allowlist")
	}
}

// TestRelayRevokedKey (GATE-02): a revoked key is rejected with 401 and
// never reaches the upstream.
func TestRelayRevokedKey(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstreamHit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
	}))
	defer upstream.Close()

	svc := newRelaySvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusRevoked, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (revoked key)", w.Code)
	}
	if upstreamHit {
		t.Error("upstream must not be called when the API key is revoked")
	}
}

// TestRelayAllCandidatesFailed: every candidate fails with 5xx; the gateway
// returns 502 and the log records the exhaustion.
func TestRelayAllCandidatesFailed(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	svc := newRelaySvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (all candidates failed)", w.Code)
	}
	var log model.RequestLog
	if err := db.First(&log).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if log.StatusCode != http.StatusBadGateway {
		t.Errorf("log status_code = %d, want 502", log.StatusCode)
	}
	if log.FailReason == nil || *log.FailReason == "" {
		t.Error("expected non-empty fail_reason for an all-candidates-failed request")
	}
	// GATE-13: attempts_detail records every candidate try as JSON.
	if log.AttemptsDetail == nil {
		t.Error("expected attempts_detail to be populated (GATE-13)")
	}
}

// TestRelayBudgetExceededReturnsInsufficientQuota: budget exhaustion maps
// to OpenAI's insufficient_quota type (distinct from rate_limit_error), and
// never reaches the upstream.
func TestRelayBudgetExceededReturnsInsufficientQuota(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstreamHit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
	}))
	defer upstream.Close()

	svc := newRelaySvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)

	// Key whose budget is already fully spent (>= limit).
	now := time.Now().UTC()
	limit := int64(100)
	apiKey := &model.APIKey{
		KeyHash: ycrypto.HashToken("sk-yr-test"), KeyPrefix: "sk-yr-test------",
		OwnerLabel: "tester", Status: model.APIKeyStatusActive,
		BudgetLimitCents: &limit, BudgetSpentCents: 100,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(apiKey).Error; err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	if err := db.Create(&model.APIKeyModel{APIKeyID: apiKey.ID, ModelID: m.ID, CreatedAt: now}).Error; err != nil {
		t.Fatalf("seed allowlist: %v", err)
	}

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (budget exceeded)", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"insufficient_quota"`)) {
		t.Errorf("expected insufficient_quota error type, got %s", w.Body.String())
	}
	if upstreamHit {
		t.Error("upstream must not be called when the key's budget is exhausted")
	}
}

// TestRelayStreamSuccess: a healthy streaming upstream forwards SSE chunks
// with the model field rewritten to the external name in EVERY chunk
// (provider name never leaks), terminates with [DONE], and records the
// final-usage tokens for cost (cost_known=true). Covers handleStream +
// StreamUpstreamToClient end-to-end.
func TestRelayStreamSuccess(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte(s))
			if flusher != nil {
				flusher.Flush()
			}
		}
		write("data: {\"model\":\"gpt-4o-real\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		write("data: {\"model\":\"gpt-4o-real\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
		write("data: [DONE]\n\n")
	}))
	defer upstream.Close()

	svc := newRelaySvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	body := w.Body.Bytes()
	if bytes.Contains(body, []byte("gpt-4o-real")) {
		t.Errorf("upstream provider model name leaked into the stream: %s", body)
	}
	if !bytes.Contains(body, []byte(`"model":"gpt-4o"`)) {
		t.Errorf("external model name not present in every chunk: %s", body)
	}
	if !bytes.Contains(body, []byte("data: [DONE]")) {
		t.Errorf("[DONE] terminator not forwarded: %s", body)
	}
	var log model.RequestLog
	if err := db.First(&log).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if log.InputTokens != 3 || log.OutputTokens != 2 {
		t.Errorf("stream usage not recorded: input=%d output=%d", log.InputTokens, log.OutputTokens)
	}
	if !log.CostKnown {
		t.Error("expected cost_known=true (usage was received from the final chunk)")
	}
}

// extractModelFromJSON pulls just the "model" string out of a JSON body for
// the failover test's assertion — keeping it inline avoids pulling
// encoding/json into the assertion line for what is a one-field check.
func extractModelFromJSON(t *testing.T, body []byte) string {
	t.Helper()
	var p struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	return p.Model
}
