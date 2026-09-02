package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/internal/service/requestlog"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// The candidate provider every helper in this file describes: one address, one
// credential, one model. Shared so that a preview and a create in the same test
// are talking about the same candidate — the parity those tests assert is only
// meaningful if the request bodies agree field for field.
const (
	testProviderBaseURL      = "https://api.example.com/v1"
	testProviderKeyPlaintext = "sk-abcdefghijklmnopqrstuvwxyz1234"
	testProviderTestModel    = "gpt-4o-mini"
)

func newProviderTestRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	return newProviderTestRouterWithClient(t, &alwaysSuccessClient{})
}

// newProviderTestRouterWithClient lets a test swap in a fake ProviderClient
// (e.g. one that always fails verification, or one whose TestChatCompletion
// itself errors) to exercise service-layer branches that alwaysSuccessClient
// can never reach.
func newProviderTestRouterWithClient(t *testing.T, client providerclient.ProviderClient) (*gin.Engine, *gorm.DB) {
	t.Helper()
	if err := RegisterValidators(); err != nil {
		t.Fatalf("RegisterValidators failed: %v", err)
	}
	db := testutil.NewSQLiteDB(t)
	svc := provider.NewProviderService(db, crypto.NewSecretBox(testutil.ProviderMasterKey()), client)

	r := gin.New()
	admin := r.Group("/api/admin")
	admin.GET("/providers", GetProviders(svc))
	admin.POST("/providers", PostProvider(svc))
	admin.POST("/providers/test-key", PostProviderTestKey(svc))
	admin.POST("/providers/list-models", PostProviderListModels(svc))
	admin.GET("/providers/:id", GetProvider(svc))
	admin.GET("/providers/:id/models", GetProviderListModels(svc))
	admin.PATCH("/providers/:id", PatchProvider(svc))
	admin.PATCH("/providers/:id/status", PatchProviderStatus(svc))
	admin.POST("/providers/:id/keys", PostProviderKey(svc))
	admin.PATCH("/providers/:id/keys/:keyId", PatchProviderKey(svc))
	admin.PATCH("/providers/:id/keys/:keyId/order", PatchProviderKeyOrder(svc))
	admin.PATCH("/providers/:id/keys/:keyId/status", PatchProviderKeyStatus(svc))
	admin.DELETE("/providers/:id/keys/:keyId", DeleteProviderKey(svc))
	admin.DELETE("/providers/:id", DeleteProvider(svc))
	admin.POST("/providers/:id/keys/:keyId/test", PostProviderKeyTest(svc))
	admin.POST("/providers/:id/keys/test-all", PostProviderKeysTestAll(svc))
	return r, db
}

// alwaysSuccessClient is provider_handler_test.go's own fake (kept separate
// from the service packages' own test fakes — handler tests must not depend
// on service package test-only symbols).
type alwaysSuccessClient struct{}

func (alwaysSuccessClient) TestChatCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}, nil
}

func (alwaysSuccessClient) TestImageGeneration(ctx context.Context, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}, nil
}

func (alwaysSuccessClient) TestStreamingCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}, nil
}

func (alwaysSuccessClient) TestFunctionCalling(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}, nil
}

func (alwaysSuccessClient) ListModels(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey string) (providerclient.ListModelsResult, error) {
	return providerclient.ListModelsResult{Models: []string{"model-a", "model-b"}, Outcome: providerclient.TestSuccess, DurationMs: 5}, nil
}

// catalogueRefusingClient passes every credential test — so a provider can be
// created with an enabled, verified key — but has the upstream refuse the
// model-catalogue fetch itself, the one shape that reaches the by-id
// endpoint's genuine-failure branch with a usable key in place.
type catalogueRefusingClient struct{ alwaysSuccessClient }

func (catalogueRefusingClient) ListModels(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey string) (providerclient.ListModelsResult, error) {
	return providerclient.ListModelsResult{
		Outcome:    providerclient.TestUpstreamError,
		DurationMs: 7,
		Detail:     "HTTP 400: this endpoint requires an OpenAI API-key account",
	}, nil
}

// modelNotFoundClient always classifies as TestModelNotFound, which
// classifyTestResult (internal/service/provider_service.go) never
// overwrites verification_status for — a freshly created key stays
// "untested" instead of becoming verified, letting tests exercise the
// "cannot enable an unverified key" (ErrProviderKeyNotVerified) branch that
// alwaysSuccessClient can never reach.
type modelNotFoundClient struct{}

func (modelNotFoundClient) TestImageGeneration(ctx context.Context, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}, nil
}

func (modelNotFoundClient) TestChatCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestModelNotFound, DurationMs: 3}, nil
}

func (modelNotFoundClient) TestStreamingCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestModelNotFound, DurationMs: 3}, nil
}

func (modelNotFoundClient) TestFunctionCalling(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestModelNotFound, DurationMs: 3}, nil
}

func (modelNotFoundClient) ListModels(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey string) (providerclient.ListModelsResult, error) {
	return providerclient.ListModelsResult{Outcome: providerclient.TestModelNotFound, DurationMs: 3, Detail: "HTTP 404"}, nil
}

// erroringClient always returns an error from the client call itself (e.g.
// a concurrency cap rejection), never a TestResult outcome — exercises
// PostProviderTestKey's ProviderTestFailed mapping.
type erroringClient struct{}

func (erroringClient) TestImageGeneration(ctx context.Context, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}, nil
}

func (erroringClient) TestChatCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{}, errors.New("client refused the call")
}

func (erroringClient) TestStreamingCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{}, errors.New("client refused the call")
}

func (erroringClient) TestFunctionCalling(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{}, errors.New("client refused the call")
}

func (erroringClient) ListModels(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey string) (providerclient.ListModelsResult, error) {
	return providerclient.ListModelsResult{}, errors.New("client refused the call")
}

// createProviderViewForTest creates a provider through the API — the shared
// candidate fields plus whatever extra request fields the caller names — and
// returns its decoded view. A create always makes exactly one key, so any
// other count means the setup itself did not do what the test assumes.
func createProviderViewForTest(t *testing.T, r *gin.Engine, name string, extra map[string]interface{}) providerViewJSON {
	t.Helper()
	body := map[string]interface{}{
		"name": name, "base_url": testProviderBaseURL,
		"key_label": "primary", "key_plaintext": testProviderKeyPlaintext, "test_model": testProviderTestModel,
	}
	for field, value := range extra {
		body[field] = value
	}
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: create provider %q failed: %d, body: %s", name, w.Code, w.Body.String())
	}
	var view providerViewJSON
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("unmarshal provider view: %v", err)
	}
	if len(view.Keys) != 1 {
		t.Fatalf("expected exactly one key in created provider %q, got %d", name, len(view.Keys))
	}
	return view
}

// createProviderForTest creates a provider (with alwaysSuccessClient's
// automatic first-key verification, unless r was built with a different
// client) and returns its ID and its first key's ID for tests that need an
// existing provider/key to act on.
func createProviderForTest(t *testing.T, r *gin.Engine, name string) (providerID uint, keyID uint) {
	t.Helper()
	view := createProviderViewForTest(t, r, name, nil)
	return view.ID, view.Keys[0].ID
}

func TestPostProviderCreatesProviderWithFirstKey(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	body, _ := json.Marshal(map[string]interface{}{
		"name": "openai-main", "base_url": "https://api.example.com/v1",
		"key_label": "primary", "key_plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini",
		"management_status": model.ProviderStatusEnabled,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/providers", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Code != 0 {
		t.Fatalf("expected code=0, got %d", env.Code)
	}
}

func TestGetProvidersReturnsCreatedProvider(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	body, _ := json.Marshal(map[string]interface{}{
		"name": "openai-main", "base_url": "https://api.example.com/v1",
		"key_label": "primary", "key_plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/providers", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(httptest.NewRecorder(), req)

	listReq := httptest.NewRequest(http.MethodGet, "/api/admin/providers", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, listReq)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("openai-main")) {
		t.Fatalf("expected the created provider in the list response, got: %s", w.Body.String())
	}
}

func TestPostProviderRejectsDuplicateNameWith400(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	body, _ := json.Marshal(map[string]interface{}{
		"name": "dup", "base_url": "https://a.example.com", "key_label": "k1", "key_plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini",
	})
	req1 := httptest.NewRequest(http.MethodPost, "/api/admin/providers", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(httptest.NewRecorder(), req1)

	req2 := httptest.NewRequest(http.MethodPost, "/api/admin/providers", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req2)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPostProviderAcceptsExplicitProviderTypeAndEchoesInView(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	body := map[string]interface{}{
		"name": "anthropic-main", "base_url": "https://a.example.com", "key_label": "k1",
		"key_plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "claude-3",
		"provider_type":      "anthropic",
		"protocol_endpoints": `{"responses":"https://gw.example.com/v1"}`,
	}
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var view struct {
		ProviderType      string `json:"provider_type"`
		ProtocolEndpoints string `json:"protocol_endpoints"`
	}
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("unmarshal provider view: %v", err)
	}
	if view.ProviderType != "anthropic" {
		t.Fatalf("expected provider_type=anthropic, got %q", view.ProviderType)
	}
	if view.ProtocolEndpoints != `{"responses":"https://gw.example.com/v1"}` {
		t.Fatalf("expected protocol_endpoints to round-trip, got %q", view.ProtocolEndpoints)
	}
}

func TestPostProviderDefaultsProviderTypeToOpenAIWhenOmitted(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	body := map[string]interface{}{
		"name": "no-type-main", "base_url": "https://a.example.com", "key_label": "k1",
		"key_plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini",
	}
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var view struct {
		ProviderType string `json:"provider_type"`
	}
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("unmarshal provider view: %v", err)
	}
	if view.ProviderType != "openai" {
		t.Fatalf("expected default provider_type=openai for backward compatibility, got %q", view.ProviderType)
	}
}

func TestPostProviderRejectsInvalidProviderTypeWith400(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	body := map[string]interface{}{
		"name": "bad-type-main", "base_url": "https://a.example.com", "key_label": "k1",
		"key_plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini",
		"provider_type": "claude",
	}
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderProtocolInvalid {
		t.Fatalf("expected code %d, got %d", errcode.ProviderProtocolInvalid, env.Code)
	}
}

func TestPostProviderRejectsMalformedProtocolEndpointsWith400(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	body := map[string]interface{}{
		"name": "bad-endpoints-main", "base_url": "https://a.example.com", "key_label": "k1",
		"key_plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini",
		"protocol_endpoints": "{not-json",
	}
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderProtocolInvalid {
		t.Fatalf("expected code %d, got %d", errcode.ProviderProtocolInvalid, env.Code)
	}
}

// TestProviderHandlersRejectNonNumericIDParams exercises parseUintParam's
// failure branch (0.0% covered before this test) across every route
// handler that parses an :id or :keyId path parameter.
func TestProviderHandlersRejectNonNumericIDParams(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "id-param-provider")

	cases := []struct {
		name, method, path string
		body               interface{}
	}{
		{"GetProvider", http.MethodGet, "/api/admin/providers/abc", nil},
		{"PatchProvider", http.MethodPatch, "/api/admin/providers/abc",
			map[string]interface{}{"name": "x-name", "base_url": "https://a.example.com"}},
		{"PatchProviderStatus", http.MethodPatch, "/api/admin/providers/abc/status",
			map[string]interface{}{"enabled": true}},
		{"PostProviderKey", http.MethodPost, "/api/admin/providers/abc/keys",
			map[string]interface{}{"label": "k2", "plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini"}},
		{"PatchProviderKey", http.MethodPatch, "/api/admin/providers/1/keys/abc",
			map[string]interface{}{"label": "k1", "test_model": "gpt-4o-mini"}},
		{"PatchProviderKeyOrder_BadProviderID", http.MethodPatch, "/api/admin/providers/abc/keys/1/order",
			map[string]interface{}{"direction": "up"}},
		{"PatchProviderKeyOrder_BadKeyID", http.MethodPatch,
			fmt.Sprintf("/api/admin/providers/%d/keys/abc/order", providerID),
			map[string]interface{}{"direction": "up"}},
		{"PatchProviderKeyStatus", http.MethodPatch, "/api/admin/providers/1/keys/abc/status",
			map[string]interface{}{"enabled": false}},
		{"PostProviderKeyTest", http.MethodPost, "/api/admin/providers/1/keys/abc/test", nil},
		{"PostProviderKeysTestAll", http.MethodPost, "/api/admin/providers/abc/keys/test-all", nil},
		{"DeleteProviderKey_BadProviderID", http.MethodDelete, "/api/admin/providers/abc/keys/1", nil},
		{"DeleteProviderKey_BadKeyID", http.MethodDelete,
			fmt.Sprintf("/api/admin/providers/%d/keys/abc", providerID), nil},
		{"DeleteProvider_BadProviderID", http.MethodDelete, "/api/admin/providers/abc", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, env := doJSON(t, r, tc.method, tc.path, tc.body, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
			}
			if env.Code != errcode.InvalidParam {
				t.Fatalf("expected code %d, got %d", errcode.InvalidParam, env.Code)
			}
		})
	}
}

// TestProviderHandlersRejectInvalidRequestBody exercises each handler's
// bindJSON failure branch via a struct-tag validation failure (as opposed
// to a malformed-JSON body, already covered by auth_handler_test.go for the
// shared bindJSON helper itself).
func TestProviderHandlersRejectInvalidRequestBody(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, keyID := createProviderForTest(t, r, "body-validation-provider")

	cases := []struct {
		name, method, path string
		body               interface{}
	}{
		{"PatchProvider_MissingName", http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d", providerID),
			map[string]interface{}{"base_url": "https://a.example.com"}},
		{"PostProviderTestKey_MissingAPIKey", http.MethodPost, "/api/admin/providers/test-key",
			map[string]interface{}{"base_url": "https://a.example.com", "model": "gpt-4o-mini"}},
		{"PostProviderKey_LabelTooShort", http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/keys", providerID),
			map[string]interface{}{"label": "a", "plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini"}},
		{"PatchProviderKey_PlaintextTooShort", http.MethodPatch,
			fmt.Sprintf("/api/admin/providers/%d/keys/%d", providerID, keyID),
			map[string]interface{}{"label": "primary", "plaintext": "short", "test_model": "gpt-4o-mini"}},
		{"PatchProviderKeyOrder_BadDirection", http.MethodPatch,
			fmt.Sprintf("/api/admin/providers/%d/keys/%d/order", providerID, keyID),
			map[string]interface{}{"direction": "sideways"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, env := doJSON(t, r, tc.method, tc.path, tc.body, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
			}
			if env.Code != errcode.InvalidParam {
				t.Fatalf("expected code %d, got %d", errcode.InvalidParam, env.Code)
			}
		})
	}
}

// TestPatchProviderStatusRejectsMalformedJSON and
// TestPatchProviderKeyStatusRejectsMalformedJSON guard setStatusRequest's
// bindJSON call specifically: Enabled bool has no validator tags, so a
// map-based body (as used everywhere else in this file) always binds
// successfully regardless of content — only a body that fails to parse as
// JSON at all can exercise these two handlers' "if !bindJSON" branch.
func TestPatchProviderStatusRejectsMalformedJSON(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "malformed-status-provider")

	req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d/status", providerID),
		strings.NewReader(`{"enabled":`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchProviderKeyStatusRejectsMalformedJSON(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	_, keyID := createProviderForTest(t, r, "malformed-key-status-provider")

	req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/providers/1/keys/%d/status", keyID),
		strings.NewReader(`{"enabled":`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestGetProviderReturns400WhenNotFound(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/providers/999999", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ProviderNotFound, env.Code)
	}
}

func TestGetProviderReturnsDetailOnSuccess(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "get-detail-provider")

	w, env := doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/providers/%d", providerID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(env.Data, []byte("get-detail-provider")) {
		t.Fatalf("expected the provider's own name in its detail response, got: %s", env.Data)
	}
}

func TestPatchProviderSucceeds(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "patch-provider")

	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d", providerID),
		map[string]interface{}{"name": "patch-provider-renamed", "base_url": "https://renamed.example.com/v1"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchProviderReturns400WhenNotFound(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	w, env := doJSON(t, r, http.MethodPatch, "/api/admin/providers/999999",
		map[string]interface{}{"name": "whatever-name", "base_url": "https://a.example.com"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ProviderNotFound, env.Code)
	}
}

func TestPatchProviderReturns400WhenNameTaken(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	createProviderForTest(t, r, "taken-name")
	providerID2, _ := createProviderForTest(t, r, "other-name")

	w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d", providerID2),
		map[string]interface{}{"name": "taken-name", "base_url": "https://a.example.com"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderNameTaken {
		t.Fatalf("expected code %d, got %d", errcode.ProviderNameTaken, env.Code)
	}
}

func TestPatchProviderStatusSucceeds(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "status-provider")

	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d/status", providerID),
		map[string]interface{}{"enabled": false}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPostProviderTestKeySucceeds(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers/test-key",
		map[string]interface{}{"base_url": "https://api.example.com/v1", "api_key": "sk-abcdefghijklmnopqrstuvwxyz1234", "model": "gpt-4o-mini"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var data struct {
		Outcome int `json:"outcome"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal outcome: %v", err)
	}
	if data.Outcome != int(providerclient.TestSuccess) {
		t.Fatalf("expected outcome %d, got %d", providerclient.TestSuccess, data.Outcome)
	}
}

func TestPostProviderTestKeyReturns400WhenClientErrors(t *testing.T) {
	r, _ := newProviderTestRouterWithClient(t, erroringClient{})
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers/test-key",
		map[string]interface{}{"base_url": "https://api.example.com/v1", "api_key": "sk-abcdefghijklmnopqrstuvwxyz1234", "model": "gpt-4o-mini"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderTestFailed {
		t.Fatalf("expected code %d, got %d", errcode.ProviderTestFailed, env.Code)
	}
}

func TestPostProviderListModelsReturnsCatalogue(t *testing.T) {
	r, _ := newProviderTestRouterWithClient(t, alwaysSuccessClient{})
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers/list-models",
		map[string]interface{}{"base_url": "https://api.example.com/v1", "api_key": "sk-abcdefghijklmnopqrstuvwxyz1234"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var data struct {
		Models  []string `json:"models"`
		Outcome int      `json:"outcome"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if data.Outcome != int(providerclient.TestSuccess) {
		t.Fatalf("expected outcome %d, got %d", providerclient.TestSuccess, data.Outcome)
	}
	if len(data.Models) != 2 || data.Models[0] != "model-a" {
		t.Fatalf("expected [model-a model-b], got %v", data.Models)
	}
}

func TestPostProviderListModelsMissingAPIKeyReturns400(t *testing.T) {
	r, _ := newProviderTestRouterWithClient(t, alwaysSuccessClient{})
	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/providers/list-models",
		map[string]interface{}{"base_url": "https://api.example.com/v1"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPostProviderListModelsReturns400WhenClientErrors(t *testing.T) {
	r, _ := newProviderTestRouterWithClient(t, erroringClient{})
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers/list-models",
		map[string]interface{}{"base_url": "https://api.example.com/v1", "api_key": "sk-abcdefghijklmnopqrstuvwxyz1234"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderTestFailed {
		t.Fatalf("expected code %d, got %d", errcode.ProviderTestFailed, env.Code)
	}
}

// modelCatalogueBody is what both catalogue endpoints answer with; outcome is
// a pointer so "no outcome at all" (the no-usable-key case) is distinguishable
// from the success value zero.
type modelCatalogueBodyJSON struct {
	Models      []string `json:"models"`
	Outcome     *int     `json:"outcome"`
	Detail      string   `json:"detail"`
	NoUsableKey bool     `json:"no_usable_key"`
}

// enableKeyForCatalogueFetch forces a created key into the enabled, verified,
// current state a catalogue fetch requires — createProviderForTest leaves its
// key unpromoted, and an unpromoted key is itself the no-usable-key case.
func enableKeyForCatalogueFetch(t *testing.T, db *gorm.DB, providerID, keyID uint) {
	t.Helper()
	var prov model.Provider
	if err := db.First(&prov, providerID).Error; err != nil {
		t.Fatalf("load provider failed: %v", err)
	}
	if err := db.Model(&model.ProviderKey{}).Where("id = ?", keyID).Updates(map[string]any{
		"management_status":              model.ProviderKeyStatusEnabled,
		"verification_status":            model.VerificationStatusPassed,
		"authorized_destination_version": prov.DestinationVersion,
	}).Error; err != nil {
		t.Fatalf("promote key failed: %v", err)
	}
}

func getModelCatalogue(t *testing.T, r *gin.Engine, providerID uint) modelCatalogueBodyJSON {
	t.Helper()
	w, env := doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/providers/%d/models", providerID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body modelCatalogueBodyJSON
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal catalogue body: %v", err)
	}
	return body
}

func TestGetProviderListModelsReturnsCatalogue(t *testing.T) {
	r, db := newProviderTestRouter(t)
	providerID, keyID := createProviderForTest(t, r, "catalogue-provider")
	enableKeyForCatalogueFetch(t, db, providerID, keyID)

	body := getModelCatalogue(t, r, providerID)
	if body.Outcome == nil || *body.Outcome != int(providerclient.TestSuccess) {
		t.Fatalf("expected outcome %d, got %v", providerclient.TestSuccess, body.Outcome)
	}
	if len(body.Models) != 2 || body.Models[0] != "model-a" {
		t.Fatalf("expected [model-a model-b], got %v", body.Models)
	}
	if body.NoUsableKey {
		t.Fatalf("expected no_usable_key=false for a provider with an enabled key")
	}
}

// A provider whose keys are all disabled — the shape an operator lands in
// after a key fails verification — must be told exactly that, and must not be
// handed a credential-test verdict for a fetch that never left the process.
// This case used to answer TestAuthFailed, which sent admins to re-check a key
// and a base URL that were not the problem.
func TestGetProviderListModelsReportsNoUsableKeyWithoutBlamingCredentials(t *testing.T) {
	r, db := newProviderTestRouter(t)
	providerID, keyID := createProviderForTest(t, r, "disabled-key-provider")
	enableKeyForCatalogueFetch(t, db, providerID, keyID)
	if err := db.Model(&model.ProviderKey{}).Where("id = ?", keyID).
		Update("management_status", model.ProviderKeyStatusDisabled).Error; err != nil {
		t.Fatalf("disable key failed: %v", err)
	}

	body := getModelCatalogue(t, r, providerID)
	if !body.NoUsableKey {
		t.Fatalf("expected no_usable_key=true when every key is disabled")
	}
	if body.Outcome != nil {
		t.Fatalf("expected no outcome when no fetch was attempted, got %d", *body.Outcome)
	}
	if len(body.Models) != 0 || body.Detail != "" {
		t.Fatalf("expected an empty catalogue with no detail, got %v / %q", body.Models, body.Detail)
	}
}

// The other half of the split: a usable key IS present and the upstream is the
// one refusing, so the response carries that category plus the upstream's own
// words, verbatim — the text an operator needs to tell a rejected credential
// apart from an endpoint the upstream does not serve.
func TestGetProviderListModelsSurfacesUpstreamRefusal(t *testing.T) {
	r, db := newProviderTestRouterWithClient(t, catalogueRefusingClient{})
	providerID, keyID := createProviderForTest(t, r, "refusing-upstream-provider")
	enableKeyForCatalogueFetch(t, db, providerID, keyID)

	body := getModelCatalogue(t, r, providerID)
	if body.NoUsableKey {
		t.Fatalf("expected no_usable_key=false when a key was used to make the call")
	}
	if body.Outcome == nil || *body.Outcome != int(providerclient.TestUpstreamError) {
		t.Fatalf("expected outcome %d, got %v", providerclient.TestUpstreamError, body.Outcome)
	}
	if body.Detail != "HTTP 400: this endpoint requires an OpenAI API-key account" {
		t.Fatalf("expected the upstream detail verbatim, got %q", body.Detail)
	}
}

// The stateless preview always carries a plaintext key, so it has no
// no-usable-key case; it gains the same verbatim detail.
func TestPostProviderListModelsSurfacesDetail(t *testing.T) {
	r, _ := newProviderTestRouterWithClient(t, modelNotFoundClient{})
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers/list-models",
		map[string]interface{}{"base_url": testProviderBaseURL, "api_key": testProviderKeyPlaintext}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body modelCatalogueBodyJSON
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal catalogue body: %v", err)
	}
	if body.Outcome == nil || *body.Outcome != int(providerclient.TestModelNotFound) {
		t.Fatalf("expected outcome %d, got %v", providerclient.TestModelNotFound, body.Outcome)
	}
	if body.Detail != "HTTP 404" {
		t.Fatalf("expected the upstream detail verbatim, got %q", body.Detail)
	}
}

func TestPostProviderKeyReturns400WhenProviderNotFound(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers/999999/keys",
		map[string]interface{}{"label": "k2", "plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ProviderNotFound, env.Code)
	}
}

func TestPostProviderKeyReturns400WhenLabelTaken(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "label-taken-provider")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/keys", providerID),
		map[string]interface{}{"label": "primary", "plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderKeyLabelTaken {
		t.Fatalf("expected code %d, got %d", errcode.ProviderKeyLabelTaken, env.Code)
	}
}

func TestPostProviderKeySucceeds(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "second-key-provider")

	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/keys", providerID),
		map[string]interface{}{"label": "secondary", "plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchProviderKeyReturns400WhenKeyNotFound(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "key-not-found-provider")

	w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d/keys/999999", providerID),
		map[string]interface{}{"label": "primary", "test_model": "gpt-4o-mini"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderKeyNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ProviderKeyNotFound, env.Code)
	}
}

func TestPatchProviderKeyReturns400WhenLabelTaken(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "key-label-taken-provider")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/keys", providerID),
		map[string]interface{}{"label": "secondary", "plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: create second key failed: %d, body: %s", w.Code, w.Body.String())
	}
	var view struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("unmarshal key view: %v", err)
	}

	w, env = doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d/keys/%d", providerID, view.ID),
		map[string]interface{}{"label": "primary", "test_model": "gpt-4o-mini"}, nil) // "primary" already used by the first key
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderKeyLabelTaken {
		t.Fatalf("expected code %d, got %d", errcode.ProviderKeyLabelTaken, env.Code)
	}
}

func TestPatchProviderKeySucceedsWithLabelOnlyEdit(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, keyID := createProviderForTest(t, r, "key-label-edit-provider")

	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d/keys/%d", providerID, keyID),
		map[string]interface{}{"label": "renamed-primary", "test_model": "gpt-4o-mini"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchProviderKeySucceedsWithNewPlaintext(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, keyID := createProviderForTest(t, r, "key-plaintext-edit-provider")

	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d/keys/%d", providerID, keyID),
		map[string]interface{}{"label": "primary", "plaintext": "sk-zyxwvutsrqponmlkjihgfedcba9876", "test_model": "gpt-4o-mini"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchProviderKeyStatusReturns400WhenKeyNotFound(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	w, env := doJSON(t, r, http.MethodPatch, "/api/admin/providers/1/keys/999999/status",
		map[string]interface{}{"enabled": true}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderKeyNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ProviderKeyNotFound, env.Code)
	}
}

func TestPatchProviderKeyStatusReturns400WhenNotVerified(t *testing.T) {
	r, _ := newProviderTestRouterWithClient(t, modelNotFoundClient{})
	_, keyID := createProviderForTest(t, r, "not-verified-provider")

	w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/1/keys/%d/status", keyID),
		map[string]interface{}{"enabled": true}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderKeyNotVerified {
		t.Fatalf("expected code %d, got %d", errcode.ProviderKeyNotVerified, env.Code)
	}
}

func TestPatchProviderKeyStatusSucceedsWhenVerified(t *testing.T) {
	r, _ := newProviderTestRouter(t) // alwaysSuccessClient -> the key comes back already verified
	_, keyID := createProviderForTest(t, r, "verified-provider")

	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/1/keys/%d/status", keyID),
		map[string]interface{}{"enabled": true}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchProviderKeyOrderMovesKeyAndReturns200(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "order-provider")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/keys", providerID),
		map[string]interface{}{"label": "secondary", "plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: create second key failed: %d, body: %s", w.Code, w.Body.String())
	}
	var view struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("unmarshal key view: %v", err)
	}

	w, _ = doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d/keys/%d/order", providerID, view.ID),
		map[string]interface{}{"direction": "up"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
}

// TestPatchProviderKeyOrderReturns400ForUnknownKey is the direct
// regression test: reordering a
// nonexistent key used to hit repository.SwapProviderKeySortOrder's plain
// gorm.ErrRecordNotFound untranslated, giving a 500 InternalError instead
// of the 400 ProviderKeyNotFound every sibling key-lookup endpoint in this
// package returns for the identical condition.
func TestPatchProviderKeyOrderReturns400ForUnknownKey(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "order-unknown-key-provider")

	w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d/keys/999999/order", providerID),
		map[string]interface{}{"direction": "up"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderKeyNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ProviderKeyNotFound, env.Code)
	}
}

func TestPostProviderKeyTestReturns400WhenKeyNotFound(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers/1/keys/999999/test", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderKeyNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ProviderKeyNotFound, env.Code)
	}
}

func TestPostProviderKeyTestSucceeds(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, keyID := createProviderForTest(t, r, "key-test-provider")

	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/keys/%d/test", providerID, keyID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPostProviderKeyTestReturns400WhenNeedsReentry(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, keyID := createProviderForTest(t, r, "needs-reentry-provider")

	// Changing base_url bumps destination_version, leaving the existing
	// key's authorized_destination_version stale.
	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d", providerID),
		map[string]interface{}{"name": "needs-reentry-provider", "base_url": "https://changed.example.com/v1"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: change base_url failed: %d, body: %s", w.Code, w.Body.String())
	}

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/keys/%d/test", providerID, keyID), nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderKeyNeedsReentry {
		t.Fatalf("expected code %d, got %d", errcode.ProviderKeyNeedsReentry, env.Code)
	}
}

func TestPostProviderKeysTestAllReturns400WhenProviderNotFound(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers/999999/keys/test-all", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ProviderNotFound, env.Code)
	}
}

func TestPostProviderRejectsMalformedJSON(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/providers", strings.NewReader(`{"name":`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestGetProvidersReturns500WhenListFails(t *testing.T) {
	r, db := newProviderTestRouter(t)
	createProviderForTest(t, r, "before-db-closed-provider")
	testutil.CloseDB(t, db)

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/providers", nil, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.InternalError {
		t.Fatalf("expected code %d, got %d", errcode.InternalError, env.Code)
	}
}

func TestPatchProviderStatusReturns500WhenUpdateFails(t *testing.T) {
	r, db := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "status-db-error-provider")
	testutil.CloseDB(t, db)

	w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d/status", providerID),
		map[string]interface{}{"enabled": true}, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.InternalError {
		t.Fatalf("expected code %d, got %d", errcode.InternalError, env.Code)
	}
}

func TestPostProviderKeysTestAllSucceeds(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, keyID := createProviderForTest(t, r, "test-all-provider")
	// Enable the key so it's actually included among the tested keys
	// (TestAllProviderKeys skips any key that isn't management-enabled).
	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/1/keys/%d/status", keyID),
		map[string]interface{}{"enabled": true}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: enable key failed: %d, body: %s", w.Code, w.Body.String())
	}

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/keys/test-all", providerID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var data struct {
		Results []struct {
			KeyID uint `json:"key_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal results: %v", err)
	}
	if len(data.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(data.Results))
	}
}

// perProtocolClient answers differently per wire protocol: the anthropic
// destination rejects the credential and says why, every other protocol
// passes. It reproduces the configuration that motivated the per-destination
// breakdown — a provider declaring an extra endpoint its upstream does not
// actually serve, whose aggregate verdict alone tells the admin nothing about
// which of the two destinations went wrong.
type perProtocolClient struct{}

// perProtocolAnthropicDetail is the upstream diagnostic the anthropic
// destination returns, quoted verbatim through storage into the response.
const perProtocolAnthropicDetail = "HTTP 401: invalid api key for this endpoint"

const (
	perProtocolPrimaryDurationMs = 5
	perProtocolExtraDurationMs   = 7
)

func (perProtocolClient) resultFor(proto protocols.ProtocolID) providerclient.TestResult {
	if proto == protocols.ProtocolClaude {
		return providerclient.TestResult{
			Outcome:    providerclient.TestAuthFailed,
			DurationMs: perProtocolExtraDurationMs,
			Detail:     perProtocolAnthropicDetail,
		}
	}
	return providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: perProtocolPrimaryDurationMs}
}

func (c perProtocolClient) TestImageGeneration(ctx context.Context, _, _, _ string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess}, nil
}

func (c perProtocolClient) TestVideoGeneration(ctx context.Context, _, _, _ string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess}, nil
}

func (c perProtocolClient) TestChatCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return c.resultFor(proto), nil
}

func (c perProtocolClient) TestStreamingCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return c.resultFor(proto), nil
}

func (c perProtocolClient) TestFunctionCalling(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return c.resultFor(proto), nil
}

func (perProtocolClient) ListModels(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey string) (providerclient.ListModelsResult, error) {
	return providerclient.ListModelsResult{Outcome: providerclient.TestSuccess}, nil
}

// keyTestTargetJSON mirrors one entry of a key's last_test_targets array,
// decoded from the HTTP body so these tests assert the wire contract rather
// than the service's Go types.
type keyTestTargetJSON struct {
	Proto      string `json:"proto"`
	Outcome    int    `json:"outcome"`
	DurationMs int64  `json:"duration_ms"`
	Detail     string `json:"detail"`
}

// providerKeyJSON is the part of a key view these tests read. LastTestTargets
// stays raw so a JSON null (no breakdown recorded) can be told apart from an
// empty or absent array.
type providerKeyJSON struct {
	ID                 uint            `json:"id"`
	ManagementStatus   int             `json:"management_status"`
	VerificationStatus int             `json:"verification_status"`
	LastTestResult     *int            `json:"last_test_result"`
	LastTestTargets    json.RawMessage `json:"last_test_targets"`
}

type providerViewJSON struct {
	ID   uint              `json:"id"`
	Keys []providerKeyJSON `json:"keys"`
}

// createDualProtocolProviderForTest creates a provider that declares an extra
// anthropic endpoint (empty value = reuse base_url) beside its openai
// primary, so verification probes two destinations, and asks for the key to
// be enabled so the gate's refusal is observable.
func createDualProtocolProviderForTest(t *testing.T, r *gin.Engine, name string) providerViewJSON {
	t.Helper()
	return createProviderViewForTest(t, r, name, map[string]interface{}{
		"protocol_endpoints": `{"anthropic":""}`,
		"management_status":  model.ProviderStatusEnabled,
	})
}

// decodeTestTargets decodes a last_test_targets value that is expected to be
// a populated array.
func decodeTestTargets(t *testing.T, raw json.RawMessage) []keyTestTargetJSON {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("expected a last_test_targets field in the response, got none")
	}
	var targets []keyTestTargetJSON
	if err := json.Unmarshal(raw, &targets); err != nil {
		t.Fatalf("unmarshal last_test_targets %s: %v", raw, err)
	}
	return targets
}

// assertIncidentBreakdown checks the two-destination breakdown both probed
// entry points produce with perProtocolClient: the primary passed, the extra
// anthropic endpoint rejected the credential, and the upstream's own words
// came through unaltered.
func assertIncidentBreakdown(t *testing.T, raw json.RawMessage) {
	t.Helper()
	targets := decodeTestTargets(t, raw)
	want := []keyTestTargetJSON{
		{Proto: "openai", Outcome: int(providerclient.TestSuccess), DurationMs: perProtocolPrimaryDurationMs},
		{Proto: "anthropic", Outcome: int(providerclient.TestAuthFailed), DurationMs: perProtocolExtraDurationMs, Detail: perProtocolAnthropicDetail},
	}
	if len(targets) != len(want) {
		t.Fatalf("expected one entry per probed destination %+v, got %+v", want, targets)
	}
	for i := range want {
		if targets[i] != want[i] {
			t.Fatalf("destination %d: expected %+v, got %+v", i, want[i], targets[i])
		}
	}
}

// readStoredTestTargets reads the last_test_targets column straight from the
// database, so a test can tell "the response computed it" apart from "the run
// persisted it".
func readStoredTestTargets(t *testing.T, db *gorm.DB, keyID uint) *string {
	t.Helper()
	var stored *string
	if err := db.Raw("SELECT last_test_targets FROM provider_keys WHERE id = ?", keyID).Scan(&stored).Error; err != nil {
		t.Fatalf("read last_test_targets for key %d: %v", keyID, err)
	}
	return stored
}

// TestCreateProviderResponseCarriesPerProtocolBreakdown covers the creation
// path end to end: the server-side verification probes both destinations, the
// key view names each one's result, the same breakdown is in the database, and
// the aggregate gate still reports only the worst destination and refuses to
// enable the key.
func TestCreateProviderResponseCarriesPerProtocolBreakdown(t *testing.T) {
	r, db := newProviderTestRouterWithClient(t, perProtocolClient{})
	view := createDualProtocolProviderForTest(t, r, "dual-protocol-provider")
	key := view.Keys[0]

	assertIncidentBreakdown(t, key.LastTestTargets)

	// The aggregate keeps reporting the worst destination, and enabling
	// stays refused — the breakdown adds visibility, never permission.
	if key.VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("expected verification_status=failed from the rejected anthropic destination, got %d", key.VerificationStatus)
	}
	if key.LastTestResult == nil || *key.LastTestResult != int(providerclient.TestAuthFailed) {
		t.Fatalf("expected the aggregate last_test_result to stay the worst destination's, got %v", key.LastTestResult)
	}
	if key.ManagementStatus != model.ProviderKeyStatusDisabled {
		t.Fatalf("expected the key to stay disabled while one destination rejects it, got %d", key.ManagementStatus)
	}

	stored := readStoredTestTargets(t, db, key.ID)
	if stored == nil {
		t.Fatalf("expected the run's breakdown persisted, got NULL")
	}
	assertIncidentBreakdown(t, json.RawMessage(*stored))

	// Reloading the provider must show the same breakdown: an admin who
	// refreshes the page, or opens it on another device, still sees why.
	w, env := doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/providers/%d", view.ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 refetching the provider, got %d, body: %s", w.Code, w.Body.String())
	}
	var reloaded providerViewJSON
	if err := json.Unmarshal(env.Data, &reloaded); err != nil {
		t.Fatalf("unmarshal reloaded provider view: %v", err)
	}
	if len(reloaded.Keys) != 1 {
		t.Fatalf("expected one key on refetch, got %d", len(reloaded.Keys))
	}
	assertIncidentBreakdown(t, reloaded.Keys[0].LastTestTargets)
}

// TestPostProviderKeyTestResponseCarriesPerProtocolBreakdown is the single-key
// retest counterpart: its own response carries the breakdown of the run it
// just did, without a second request.
func TestPostProviderKeyTestResponseCarriesPerProtocolBreakdown(t *testing.T) {
	r, db := newProviderTestRouterWithClient(t, perProtocolClient{})
	view := createDualProtocolProviderForTest(t, r, "retest-breakdown-provider")

	w, env := doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/admin/providers/%d/keys/%d/test", view.ID, view.Keys[0].ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var key providerKeyJSON
	if err := json.Unmarshal(env.Data, &key); err != nil {
		t.Fatalf("unmarshal key view: %v", err)
	}
	assertIncidentBreakdown(t, key.LastTestTargets)
	if key.VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("expected the retest aggregate to stay failed, got %d", key.VerificationStatus)
	}

	stored := readStoredTestTargets(t, db, view.Keys[0].ID)
	if stored == nil {
		t.Fatalf("expected the retest's breakdown persisted, got NULL")
	}
	assertIncidentBreakdown(t, json.RawMessage(*stored))
}

// TestPostProviderKeysTestAllResponseCarriesPerProtocolBreakdown covers the
// batch path: every key it actually probed reports its own breakdown, and a
// key it skipped reports none rather than a stale or empty one.
func TestPostProviderKeysTestAllResponseCarriesPerProtocolBreakdown(t *testing.T) {
	r, _ := newProviderTestRouterWithClient(t, perProtocolClient{})
	view := createDualProtocolProviderForTest(t, r, "batch-breakdown-provider")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/keys/test-all", view.ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var data struct {
		Results []struct {
			KeyID           uint            `json:"key_id"`
			Skipped         bool            `json:"skipped"`
			Outcome         *int            `json:"outcome"`
			LastTestTargets json.RawMessage `json:"last_test_targets"`
		} `json:"results"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal results: %v", err)
	}
	if len(data.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(data.Results))
	}
	if data.Results[0].Outcome == nil || *data.Results[0].Outcome != int(providerclient.TestAuthFailed) {
		t.Fatalf("expected the batch aggregate to stay the worst destination's, got %v", data.Results[0].Outcome)
	}
	assertIncidentBreakdown(t, data.Results[0].LastTestTargets)

	// Changing the address invalidates the key's authorization, so the next
	// batch run skips it without probing anything — and a row nothing was
	// probed for must not claim a breakdown.
	w, _ = doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/providers/%d", view.ID),
		map[string]interface{}{"name": "batch-breakdown-provider", "base_url": "https://moved.example.com/v1"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: moving the provider address failed: %d, body: %s", w.Code, w.Body.String())
	}
	w, env = doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/keys/test-all", view.ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal results after the address change: %v", err)
	}
	if len(data.Results) != 1 || !data.Results[0].Skipped {
		t.Fatalf("expected the key to be skipped after the address change, got %+v", data.Results)
	}
	if string(data.Results[0].LastTestTargets) != "null" {
		t.Fatalf("expected no breakdown for a key nothing was probed for, got %s", data.Results[0].LastTestTargets)
	}
}

// TestProviderKeyViewReportsNoBreakdownWhenUnreadable covers the two rows that
// have no usable breakdown: one last tested by a build that predates the
// column, and one whose stored text this build cannot read. Both must render
// as they did before the field existed — null, never a partially decoded array
// that would present an unread destination as a passing one.
func TestProviderKeyViewReportsNoBreakdownWhenUnreadable(t *testing.T) {
	r, db := newProviderTestRouter(t)
	providerID, keyID := createProviderForTest(t, r, "legacy-breakdown-provider")

	cases := []struct {
		name   string
		stored interface{}
	}{
		{"tested before the column existed", nil},
		{"stored text is not JSON at all", "}not json{"},
		{"stored entry has a field of the wrong type", `[{"proto":"openai","outcome":"denied"}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := db.Exec("UPDATE provider_keys SET last_test_targets = ? WHERE id = ?", c.stored, keyID).Error; err != nil {
				t.Fatalf("seed last_test_targets: %v", err)
			}
			w, env := doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/providers/%d", providerID), nil, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
			}
			var view providerViewJSON
			if err := json.Unmarshal(env.Data, &view); err != nil {
				t.Fatalf("unmarshal provider view: %v", err)
			}
			if len(view.Keys) != 1 {
				t.Fatalf("expected one key, got %d", len(view.Keys))
			}
			if string(view.Keys[0].LastTestTargets) != "null" {
				t.Fatalf("expected last_test_targets null, got %s", view.Keys[0].LastTestTargets)
			}
		})
	}
}

// previewJSON is the stateless preview endpoint's response: the aggregate
// fields it has always returned, plus this run's per-destination breakdown
// under the same field name a stored key's view carries it. LastTestTargets
// stays raw so an absent array can be told apart from an empty one.
type previewJSON struct {
	Outcome         int             `json:"outcome"`
	DurationMs      int64           `json:"duration_ms"`
	Detail          string          `json:"detail"`
	LastTestTargets json.RawMessage `json:"last_test_targets"`
}

// postTestKeyPreview runs the create form's "test" button against the given
// protocol configuration. Every other field matches
// createDualProtocolProviderForTest's, so a preview and a create in the same
// test describe the same candidate provider.
func postTestKeyPreview(t *testing.T, r *gin.Engine, protocolEndpoints string) (*httptest.ResponseRecorder, previewJSON) {
	t.Helper()
	body := map[string]interface{}{
		"base_url": testProviderBaseURL, "api_key": testProviderKeyPlaintext,
		"model": testProviderTestModel,
	}
	if protocolEndpoints != "" {
		body["protocol_endpoints"] = protocolEndpoints
	}
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers/test-key", body, nil)
	if w.Code != http.StatusOK {
		return w, previewJSON{}
	}
	var preview previewJSON
	if err := json.Unmarshal(env.Data, &preview); err != nil {
		t.Fatalf("unmarshal preview: %v", err)
	}
	return w, preview
}

// TestPostProviderTestKeyPreviewCoversEveryConfiguredDestination is the
// preview's half of the incident the breakdown exists for. Filling in the
// create form with an extra anthropic endpoint the upstream does not serve
// used to preview as a pass — the preview probed the primary protocol alone —
// and the create that followed a second later failed at the endpoint the
// preview never touched. Against the same fake upstream and the same
// configuration, the preview must now find what the create finds: the same
// destinations, the same per-destination results, the same aggregate verdict.
func TestPostProviderTestKeyPreviewCoversEveryConfiguredDestination(t *testing.T) {
	r, _ := newProviderTestRouterWithClient(t, perProtocolClient{})

	w, preview := postTestKeyPreview(t, r, `{"anthropic":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	assertIncidentBreakdown(t, preview.LastTestTargets)

	// The aggregate fields keep their old meaning — the worst destination —
	// so a caller that reads only them is not broken by the added breakdown.
	if preview.Outcome != int(providerclient.TestAuthFailed) {
		t.Fatalf("expected the aggregate outcome to be the rejecting destination's, got %d", preview.Outcome)
	}
	if preview.Detail != perProtocolAnthropicDetail {
		t.Fatalf("expected the aggregate detail to quote the rejecting destination, got %q", preview.Detail)
	}
	if preview.DurationMs != perProtocolExtraDurationMs {
		t.Fatalf("expected the aggregate duration to be the rejecting destination's, got %d", preview.DurationMs)
	}

	// Now actually create that provider. Its server-side verification is the
	// authority the preview is supposed to predict, so the two must agree
	// destination for destination, not merely both report a failure.
	view := createDualProtocolProviderForTest(t, r, "preview-parity-provider")
	created := view.Keys[0]
	if !slices.Equal(decodeTestTargets(t, preview.LastTestTargets), decodeTestTargets(t, created.LastTestTargets)) {
		t.Fatalf("preview probed %s, creating the same provider verified %s", preview.LastTestTargets, created.LastTestTargets)
	}
	if created.LastTestResult == nil || *created.LastTestResult != preview.Outcome {
		t.Fatalf("preview reported outcome %d, the create recorded %v", preview.Outcome, created.LastTestResult)
	}
}

// TestPostProviderTestKeyPreviewWithoutExtraEndpointsProbesOnlyThePrimary
// pins the backward-compatible half: a request that names no extra endpoints —
// the only shape any caller sent before the field existed — still probes the
// primary protocol and nothing else. The fake upstream rejects anthropic, so
// a preview that invented destinations of its own would report a failure here.
func TestPostProviderTestKeyPreviewWithoutExtraEndpointsProbesOnlyThePrimary(t *testing.T) {
	r, _ := newProviderTestRouterWithClient(t, perProtocolClient{})

	w, preview := postTestKeyPreview(t, r, "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if preview.Outcome != int(providerclient.TestSuccess) || preview.Detail != "" {
		t.Fatalf("expected a plain pass for a single-destination candidate, got outcome %d detail %q", preview.Outcome, preview.Detail)
	}
	want := []keyTestTargetJSON{{Proto: "openai", Outcome: int(providerclient.TestSuccess), DurationMs: perProtocolPrimaryDurationMs}}
	if got := decodeTestTargets(t, preview.LastTestTargets); !slices.Equal(got, want) {
		t.Fatalf("expected only the primary destination %+v, got %+v", want, got)
	}
}

// TestPostProviderTestKeyPreviewRejectsWhatTheCreateWouldReject covers the
// other way the two could disagree: an endpoint URL the create refuses to
// store. Reading it leniently — the read path's rule for a value already
// validated once — would drop that endpoint and preview a pass for a
// configuration that cannot be saved at all, so the preview validates it and
// answers exactly as the create does.
func TestPostProviderTestKeyPreviewRejectsWhatTheCreateWouldReject(t *testing.T) {
	r, _ := newProviderTestRouterWithClient(t, perProtocolClient{})
	const badEndpoints = `{"anthropic":"not-an-absolute-url"}`

	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers/test-key",
		map[string]interface{}{
			"base_url": testProviderBaseURL, "api_key": testProviderKeyPlaintext,
			"model": testProviderTestModel, "protocol_endpoints": badEndpoints,
		}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderProtocolInvalid {
		t.Fatalf("expected code %d, got %d, body: %s", errcode.ProviderProtocolInvalid, env.Code, w.Body.String())
	}

	createW, createEnv := doJSON(t, r, http.MethodPost, "/api/admin/providers", map[string]interface{}{
		"name": "rejected-endpoints-provider", "base_url": testProviderBaseURL,
		"key_label": "primary", "key_plaintext": testProviderKeyPlaintext, "test_model": testProviderTestModel,
		"protocol_endpoints": badEndpoints,
	}, nil)
	if createW.Code != w.Code || createEnv.Code != env.Code {
		t.Fatalf("preview answered %d/%d for the same value the create answered %d/%d with",
			w.Code, env.Code, createW.Code, createEnv.Code)
	}
}

// TestPostProviderTestKeyPreviewRejectsProviderTypeTheCreateWouldReject is the
// same disagreement one field over. provider_type reaches the preview through
// the read path's lenient mapping, which normalizes anything unrecognized to
// openai — so a typo'd protocol previewed as a pass against a destination the
// create would never build, and the create a second later answered 400. The
// preview validates it on the write path instead, so both answer identically.
func TestPostProviderTestKeyPreviewRejectsProviderTypeTheCreateWouldReject(t *testing.T) {
	r, _ := newProviderTestRouterWithClient(t, perProtocolClient{})
	const badProviderType = "antropic" // a plausible typo, not a supported protocol

	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers/test-key",
		map[string]interface{}{
			"base_url": testProviderBaseURL, "api_key": testProviderKeyPlaintext,
			"model": testProviderTestModel, "provider_type": badProviderType,
		}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderProtocolInvalid {
		t.Fatalf("expected code %d, got %d, body: %s", errcode.ProviderProtocolInvalid, env.Code, w.Body.String())
	}

	createW, createEnv := doJSON(t, r, http.MethodPost, "/api/admin/providers", map[string]interface{}{
		"name": "rejected-provider-type-provider", "base_url": testProviderBaseURL,
		"key_label": "primary", "key_plaintext": testProviderKeyPlaintext, "test_model": testProviderTestModel,
		"provider_type": badProviderType,
	}, nil)
	if createW.Code != w.Code || createEnv.Code != env.Code {
		t.Fatalf("preview answered %d/%d for the same value the create answered %d/%d with",
			w.Code, env.Code, createW.Code, createEnv.Code)
	}
}

// fetchProviderKeys reloads the provider detail and returns its keys — the
// external observation every deletion test below asserts against.
func fetchProviderKeys(t *testing.T, r *gin.Engine, providerID uint) []providerKeyJSON {
	t.Helper()
	w, env := doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/providers/%d", providerID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reload provider %d: %d, body: %s", providerID, w.Code, w.Body.String())
	}
	var view providerViewJSON
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("unmarshal provider view: %v", err)
	}
	return view.Keys
}

func TestDeleteProviderKeyRemovesOnlyThatKey(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, firstKeyID := createProviderForTest(t, r, "key-delete-provider")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/keys", providerID),
		map[string]interface{}{"label": "second", "plaintext": testProviderKeyPlaintext, "test_model": testProviderTestModel}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: add second key failed: %d, body: %s", w.Code, w.Body.String())
	}
	var second providerKeyJSON
	if err := json.Unmarshal(env.Data, &second); err != nil {
		t.Fatalf("unmarshal second key: %v", err)
	}

	w, _ = doJSON(t, r, http.MethodDelete, fmt.Sprintf("/api/admin/providers/%d/keys/%d", providerID, second.ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete key: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}

	keys := fetchProviderKeys(t, r, providerID)
	if len(keys) != 1 || keys[0].ID != firstKeyID {
		t.Fatalf("expected only the first key %d to remain, got %+v", firstKeyID, keys)
	}
}

func TestDeleteProviderKeyReturns400WhenKeyUnknownOrCrossProvider(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, keyID := createProviderForTest(t, r, "key-delete-owner")
	otherID, _ := createProviderForTest(t, r, "key-delete-other")

	for name, path := range map[string]string{
		"unknown key":        fmt.Sprintf("/api/admin/providers/%d/keys/99999", providerID),
		"cross-provider key": fmt.Sprintf("/api/admin/providers/%d/keys/%d", otherID, keyID),
	} {
		w, env := doJSON(t, r, http.MethodDelete, path, nil, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d, body: %s", name, w.Code, w.Body.String())
		}
		if env.Code != errcode.ProviderKeyNotFound {
			t.Fatalf("%s: expected code %d, got %d", name, errcode.ProviderKeyNotFound, env.Code)
		}
	}

	// The cross-provider attempt must not have deleted the key it named.
	if keys := fetchProviderKeys(t, r, providerID); len(keys) != 1 {
		t.Fatalf("cross-provider delete attempt removed the key: %+v", keys)
	}
}

// Deleting the last key is allowed by design: the provider simply has
// nothing to serve until a new key is added, exactly like the disabled
// state, and history rows reference keys only through their own snapshot.
func TestDeleteProviderKeyAllowsRemovingTheLastKey(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	providerID, keyID := createProviderForTest(t, r, "key-delete-last")

	w, _ := doJSON(t, r, http.MethodDelete, fmt.Sprintf("/api/admin/providers/%d/keys/%d", providerID, keyID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete last key: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if keys := fetchProviderKeys(t, r, providerID); len(keys) != 0 {
		t.Fatalf("expected zero keys after deleting the last one, got %+v", keys)
	}
}

// seedCandidateAndLog gives a provider the two kinds of dependent rows the
// delete contract distinguishes: a model mapping (config — must die with the
// provider) and a request log (history — must survive it).
func seedCandidateAndLog(t *testing.T, db *gorm.DB, providerID uint, modelName, requestID string) {
	t.Helper()
	m := model.Model{Name: modelName, ManagementStatus: 1, SchedulingMode: model.ModelSchedulingModeFailover}
	if err := db.Create(&m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	c := model.ModelCandidate{ModelID: m.ID, ProviderID: providerID, ProviderModelName: modelName, SortOrder: 1, ManagementStatus: 1}
	if err := db.Create(&c).Error; err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	testutil.SeedRequestLog(t, db, requestID, time.Now().UTC(), func(r *model.RequestLog) {
		r.ModelName = modelName
		r.ProviderID = &providerID
	})
}

func countWhere(t *testing.T, db *gorm.DB, table, where string, args ...interface{}) int64 {
	t.Helper()
	var n int64
	if err := db.Table(table).Where(where, args...).Count(&n).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestDeleteProviderCascadesConfigAndKeepsHistory(t *testing.T) {
	r, db := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, r, "provider-to-delete")
	seedCandidateAndLog(t, db, providerID, "cascade-model", "req-cascade")

	w, _ := doJSON(t, r, http.MethodDelete, fmt.Sprintf("/api/admin/providers/%d", providerID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete provider: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}

	if n := countWhere(t, db, "providers", "id = ?", providerID); n != 0 {
		t.Fatalf("provider row still present after delete")
	}
	if n := countWhere(t, db, "provider_keys", "provider_id = ?", providerID); n != 0 {
		t.Fatalf("expected cascade to remove keys, %d remain", n)
	}
	if n := countWhere(t, db, "model_candidates", "provider_id = ?", providerID); n != 0 {
		t.Fatalf("expected cascade to remove candidates, %d remain", n)
	}
	// History must survive with its provider_id intact...
	if n := countWhere(t, db, "request_logs", "provider_id = ?", providerID); n != 1 {
		t.Fatalf("expected the request log to survive the delete, found %d", n)
	}
	// ...and the name lookup answers with an absent entry, which every list
	// and report view renders as an empty name rather than an error.
	names, err := repository.FindProviderNamesByIDs(db, []uint{providerID})
	if err != nil {
		t.Fatalf("name lookup after delete: %v", err)
	}
	if _, present := names[providerID]; present {
		t.Fatalf("expected the deleted provider to be absent from the name lookup, got %v", names)
	}

	// The request-log list view itself must keep the row: same id, empty
	// provider name, no error — the surface an admin actually reads.
	items, total, err := requestlog.NewRequestLogService(db).ListRequestLogs(&repository.RequestLogFilter{ProviderID: &providerID})
	if err != nil {
		t.Fatalf("request log list after delete: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("expected the deleted provider's log row to stay listed, got total=%d items=%d", total, len(items))
	}
	if items[0].ProviderID == nil || *items[0].ProviderID != providerID {
		t.Fatalf("listed row lost its provider id: %+v", items[0])
	}
	if items[0].ProviderName != "" {
		t.Fatalf("expected an empty provider name for the deleted provider, got %q", items[0].ProviderName)
	}

	// The per-provider aggregate report keeps the row too: same id, empty
	// name, totals untouched — history money must not move on a delete.
	reportRows, err := repository.AggregateByProvider(context.Background(), db, &repository.RequestLogFilter{})
	if err != nil {
		t.Fatalf("aggregate by provider after delete: %v", err)
	}
	var reportRow *repository.ProviderReportRow
	for i := range reportRows {
		if reportRows[i].ProviderID != nil && *reportRows[i].ProviderID == providerID {
			reportRow = &reportRows[i]
		}
	}
	if reportRow == nil {
		t.Fatalf("deleted provider vanished from the aggregate report: %+v", reportRows)
	}
	if reportRow.ProviderName != "" {
		t.Fatalf("expected an empty provider name in the aggregate report, got %q", reportRow.ProviderName)
	}
	if reportRow.Calls != 1 || reportRow.InputTokens != 10 || reportRow.OutputTokens != 20 {
		t.Fatalf("aggregate totals changed after delete: %+v", reportRow)
	}
}

func TestDeleteProviderReturns400WhenNotFound(t *testing.T) {
	r, _ := newProviderTestRouter(t)
	w, env := doJSON(t, r, http.MethodDelete, "/api/admin/providers/99999", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ProviderNotFound, env.Code)
	}
}

// Deleting frees the unique name; re-creating under it makes a NEW identity.
// Old history keeps the old id, so per-provider aggregates never mix the two.
func TestDeleteProviderFreesNameWithoutAdoptingOldHistory(t *testing.T) {
	r, db := newProviderTestRouter(t)
	oldID, _ := createProviderForTest(t, r, "reborn-provider")
	seedCandidateAndLog(t, db, oldID, "reborn-model", "req-reborn")

	w, _ := doJSON(t, r, http.MethodDelete, fmt.Sprintf("/api/admin/providers/%d", oldID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete provider: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}

	newID, _ := createProviderForTest(t, r, "reborn-provider")
	if newID == oldID {
		t.Fatalf("recreated provider reused the deleted id %d", oldID)
	}
	if n := countWhere(t, db, "request_logs", "provider_id = ?", oldID); n != 1 {
		t.Fatalf("old history no longer under the old id")
	}
	if n := countWhere(t, db, "request_logs", "provider_id = ?", newID); n != 0 {
		t.Fatalf("old history was adopted by the recreated provider")
	}
}

func (alwaysSuccessClient) TestVideoGeneration(ctx context.Context, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}, nil
}

func (modelNotFoundClient) TestVideoGeneration(ctx context.Context, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}, nil
}

func (erroringClient) TestVideoGeneration(ctx context.Context, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}, nil
}
