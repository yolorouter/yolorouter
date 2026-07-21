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

	ycrypto "github.com/yolorouter/yolorouter/pkg/crypto"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// newRelaySvc builds a RelayService and swaps in a plain transport so the
// test can dial a loopback-bound httptest server — safehttp.NewTransport()
// (the production transport) deliberately refuses loopback (SSRF defense),
// which would block every test here. Same pattern as provider_client_test's
// newTestClient.
func newRelaySvc(t *testing.T, db *gorm.DB) *RelayService {
	t.Helper()
	masterKey := bytes.Repeat([]byte{0x42}, 32)
	svc := NewRelayService(db, masterKey, false)
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

// TestFinalizeNonStreamCapturesBodies: a 2xx
// non-stream upstream response is captured into the RelayContext's 4 body
// fields — the caller's original request, the rewritten (provider model
// name) request actually sent upstream, the raw upstream response (provider
// model name), and the caller-facing rewritten response (external model
// name). response_body and upstream_response_body must differ (only the
// model field), proving both are recorded independently and neither is a
// copy of the other. It asserts the RelayContext fields directly via
// testHookHandleDone AND (now that finalize persists the row) that
// the same four values landed in request_log_bodies.
func TestFinalizeNonStreamCapturesBodies(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"raw upstream resp"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	defer upstream.Close()

	svc := newRelaySvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-upstream-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var captured *RelayContext
	testHookHandleDone = func(rc *RelayContext) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	c, w := newCtx(reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}

	if !bytes.Contains(captured.RequestBody, []byte(`"model":"gpt-4o"`)) {
		t.Errorf("RequestBody = %s, want it to contain the caller's original request", captured.RequestBody)
	}
	if !bytes.Contains(captured.UpstreamRequestBody, []byte(`"model":"gpt-4o-real"`)) {
		t.Errorf("UpstreamRequestBody = %s, want the rewritten (provider model name) request", captured.UpstreamRequestBody)
	}
	if !bytes.Contains(captured.UpstreamResponseBody, []byte(`"model":"gpt-4o-real"`)) {
		t.Errorf("UpstreamResponseBody = %s, want the raw upstream response (provider model name)", captured.UpstreamResponseBody)
	}
	if !bytes.Contains(captured.ResponseBody, []byte(`"model":"gpt-4o"`)) {
		t.Errorf("ResponseBody = %s, want the caller-facing rewritten response (external model name)", captured.ResponseBody)
	}
	if bytes.Equal(captured.ResponseBody, captured.UpstreamResponseBody) {
		t.Error("ResponseBody and UpstreamResponseBody must differ (post- vs pre-rewrite model field)")
	}

	// finalize must persist the same four values into
	// request_log_bodies, keyed by request_id (UPSERT, 1:1 with request_logs).
	dbBody, err := repository.GetRequestLogBodyByRequestID(db, captured.RequestID)
	if err != nil {
		t.Fatalf("GetRequestLogBodyByRequestID: %v", err)
	}
	if dbBody == nil {
		t.Fatal("expected a request_log_bodies row to be persisted by finalize")
	}
	if dbBody.RequestBody != string(captured.RequestBody) {
		t.Errorf("persisted RequestBody = %q, want %q", dbBody.RequestBody, captured.RequestBody)
	}
	if dbBody.UpstreamRequestBody != string(captured.UpstreamRequestBody) {
		t.Errorf("persisted UpstreamRequestBody = %q, want %q", dbBody.UpstreamRequestBody, captured.UpstreamRequestBody)
	}
	if dbBody.ResponseBody != string(captured.ResponseBody) {
		t.Errorf("persisted ResponseBody = %q, want %q", dbBody.ResponseBody, captured.ResponseBody)
	}
	if dbBody.UpstreamResponseBody != string(captured.UpstreamResponseBody) {
		t.Errorf("persisted UpstreamResponseBody = %q, want %q", dbBody.UpstreamResponseBody, captured.UpstreamResponseBody)
	}
}

// TestRelayKeyRotation: the first key gets a 401, so the gateway
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

// TestRelayCandidateFailover: the first candidate's provider
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
	// Each attempt used the current candidate's provider name.
	if !bytes.Contains(w.Body.Bytes(), []byte(`"model":"gpt-4o"`)) {
		t.Errorf("final response model not rewritten back to external: %s", w.Body.String())
	}
}

// TestRelayClientErrorNoSwitch: a 400 from the upstream is the
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

// TestRelayModelNotAllowed: a model outside the key's allowlist is
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

// TestRelayRevokedKey: a revoked key is rejected with 401 and
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

// TestHandleEarlyRejectionCapturesRequestBody: every
// early-rejection branch that runs before Handle's normal io.ReadAll(body)
// call — revoked/expired/budget (checkKeyStateAndLimits), concurrency, RPM —
// still records the caller's request body (bounded read) and the local
// error JSON as response_body, with upstream_* left empty (never dispatched
// to any provider).
func TestHandleEarlyRejectionCapturesRequestBody(t *testing.T) {
	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)

	cases := []struct {
		name          string
		configureKey  func(k *model.APIKey)
		preRejectHook func(svc *RelayService, apiKeyID uint)
		wantStatus    int
		wantRespSub   string
	}{
		{
			name:         "revoked",
			configureKey: func(k *model.APIKey) { k.Status = model.APIKeyStatusRevoked },
			wantStatus:   http.StatusUnauthorized,
			wantRespSub:  "API key revoked",
		},
		{
			name: "expired",
			configureKey: func(k *model.APIKey) {
				past := time.Now().UTC().Add(-time.Hour)
				k.ExpiresAt = &past
			},
			wantStatus:  http.StatusUnauthorized,
			wantRespSub: "API key expired",
		},
		{
			name: "budget_exceeded",
			configureKey: func(k *model.APIKey) {
				limit := int64(100)
				k.BudgetLimitMicros = &limit
				k.BudgetSpentMicros = 100
			},
			wantStatus:  http.StatusTooManyRequests,
			wantRespSub: "budget limit exceeded",
		},
		{
			name: "concurrency_limit",
			configureKey: func(k *model.APIKey) {
				limit := 1
				k.ConcurrencyLimit = &limit
			},
			preRejectHook: func(svc *RelayService, apiKeyID uint) {
				svc.limiter.AcquireConcurrency(apiKeyID, 1) // exhaust the only slot
			},
			wantStatus:  http.StatusTooManyRequests,
			wantRespSub: "concurrency limit exceeded",
		},
		{
			name: "rpm_exceeded",
			configureKey: func(k *model.APIKey) {
				limit := 1
				k.RPMLimit = &limit
			},
			preRejectHook: func(svc *RelayService, apiKeyID uint) {
				svc.limiter.CheckRPM(apiKeyID, 1, time.Now()) // consume the only token
			},
			wantStatus:  http.StatusTooManyRequests,
			wantRespSub: "rate limit exceeded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			svc := newRelaySvc(t, db)
			p := createProvider(t, db, "p1", "http://unused.invalid")
			m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
			apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})
			tc.configureKey(apiKey)
			if err := db.Save(apiKey).Error; err != nil {
				t.Fatalf("update api key: %v", err)
			}
			if tc.preRejectHook != nil {
				tc.preRejectHook(svc, apiKey.ID)
			}

			var captured *RelayContext
			testHookHandleDone = func(rc *RelayContext) { captured = rc }
			defer func() { testHookHandleDone = nil }()

			c, w := newCtx(reqBody)
			svc.Handle(c, apiKey)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if captured == nil {
				t.Fatal("testHookHandleDone was never invoked")
			}
			if !bytes.Contains(captured.RequestBody, []byte(`"model":"gpt-4o"`)) {
				t.Errorf("RequestBody = %s, want it to contain the caller's request", captured.RequestBody)
			}
			if len(captured.UpstreamRequestBody) != 0 || len(captured.UpstreamResponseBody) != 0 {
				t.Errorf("expected empty upstream_* for a pre-dispatch rejection, got request=%q response=%q",
					captured.UpstreamRequestBody, captured.UpstreamResponseBody)
			}

			dbBody, err := repository.GetRequestLogBodyByRequestID(db, captured.RequestID)
			if err != nil {
				t.Fatalf("GetRequestLogBodyByRequestID: %v", err)
			}
			if dbBody == nil {
				t.Fatalf("expected a request_log_bodies row for the %s rejection", tc.name)
			}
			if !bytes.Contains([]byte(dbBody.RequestBody), []byte(`"model":"gpt-4o"`)) {
				t.Errorf("persisted request_body = %q, want it to contain the caller's request", dbBody.RequestBody)
			}
			if !bytes.Contains([]byte(dbBody.ResponseBody), []byte(tc.wantRespSub)) {
				t.Errorf("persisted response_body = %q, want it to contain %q", dbBody.ResponseBody, tc.wantRespSub)
			}
			if dbBody.UpstreamRequestBody != "" || dbBody.UpstreamResponseBody != "" {
				t.Errorf("expected empty upstream_* columns, got request=%q response=%q",
					dbBody.UpstreamRequestBody, dbBody.UpstreamResponseBody)
			}
		})
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
	// attempts_detail records every candidate try as JSON.
	if log.AttemptsDetail == nil {
		t.Error("expected attempts_detail to be populated")
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
		BudgetLimitMicros: &limit, BudgetSpentMicros: 100,
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
