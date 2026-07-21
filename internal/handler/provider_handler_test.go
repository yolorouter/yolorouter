package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

func newProviderTestRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	return newProviderTestRouterWithClient(t, &alwaysSuccessClient{})
}

// newProviderTestRouterWithClient lets a test swap in a fake ProviderClient
// (e.g. one that always fails verification, or one whose TestChatCompletion
// itself errors) to exercise service-layer branches that alwaysSuccessClient
// can never reach.
func newProviderTestRouterWithClient(t *testing.T, client service.ProviderClient) (*gin.Engine, *gorm.DB) {
	t.Helper()
	if err := RegisterValidators(); err != nil {
		t.Fatalf("RegisterValidators failed: %v", err)
	}
	db := testutil.NewSQLiteDB(t)
	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(i)
	}
	svc := service.NewProviderService(db, masterKey, client)

	r := gin.New()
	admin := r.Group("/api/admin")
	admin.GET("/providers", GetProviders(svc))
	admin.POST("/providers", PostProvider(svc))
	admin.POST("/providers/test-key", PostProviderTestKey(svc))
	admin.GET("/providers/:id", GetProvider(svc))
	admin.PATCH("/providers/:id", PatchProvider(svc))
	admin.PATCH("/providers/:id/status", PatchProviderStatus(svc))
	admin.POST("/providers/:id/keys", PostProviderKey(svc))
	admin.PATCH("/providers/:id/keys/:keyId", PatchProviderKey(svc))
	admin.PATCH("/providers/:id/keys/:keyId/order", PatchProviderKeyOrder(svc))
	admin.PATCH("/providers/:id/keys/:keyId/status", PatchProviderKeyStatus(svc))
	admin.POST("/providers/:id/keys/:keyId/test", PostProviderKeyTest(svc))
	admin.POST("/providers/:id/keys/test-all", PostProviderKeysTestAll(svc))
	return r, db
}

// alwaysSuccessClient is provider_handler_test.go's own fake (kept separate
// from internal/service's own test fakes — handler tests must not depend
// on service package test-only symbols).
type alwaysSuccessClient struct{}

func (alwaysSuccessClient) TestChatCompletion(ctx context.Context, baseURL, apiKey, model string) (service.TestResult, error) {
	return service.TestResult{Outcome: service.TestSuccess, DurationMs: 5}, nil
}

func (alwaysSuccessClient) TestStreamingCompletion(ctx context.Context, baseURL, apiKey, model string) (service.TestResult, error) {
	return service.TestResult{Outcome: service.TestSuccess, DurationMs: 5}, nil
}

func (alwaysSuccessClient) TestFunctionCalling(ctx context.Context, baseURL, apiKey, model string) (service.TestResult, error) {
	return service.TestResult{Outcome: service.TestSuccess, DurationMs: 5}, nil
}

// modelNotFoundClient always classifies as TestModelNotFound, which
// classifyTestResult (internal/service/provider_service.go) never
// overwrites verification_status for — a freshly created key stays
// "untested" instead of becoming verified, letting tests exercise the
// "cannot enable an unverified key" (ErrProviderKeyNotVerified) branch that
// alwaysSuccessClient can never reach.
type modelNotFoundClient struct{}

func (modelNotFoundClient) TestChatCompletion(ctx context.Context, baseURL, apiKey, model string) (service.TestResult, error) {
	return service.TestResult{Outcome: service.TestModelNotFound, DurationMs: 3}, nil
}

func (modelNotFoundClient) TestStreamingCompletion(ctx context.Context, baseURL, apiKey, model string) (service.TestResult, error) {
	return service.TestResult{Outcome: service.TestModelNotFound, DurationMs: 3}, nil
}

func (modelNotFoundClient) TestFunctionCalling(ctx context.Context, baseURL, apiKey, model string) (service.TestResult, error) {
	return service.TestResult{Outcome: service.TestModelNotFound, DurationMs: 3}, nil
}

// erroringClient always returns an error from the client call itself (e.g.
// a concurrency cap rejection), never a TestResult outcome — exercises
// PostProviderTestKey's ProviderTestFailed mapping.
type erroringClient struct{}

func (erroringClient) TestChatCompletion(ctx context.Context, baseURL, apiKey, model string) (service.TestResult, error) {
	return service.TestResult{}, errors.New("client refused the call")
}

func (erroringClient) TestStreamingCompletion(ctx context.Context, baseURL, apiKey, model string) (service.TestResult, error) {
	return service.TestResult{}, errors.New("client refused the call")
}

func (erroringClient) TestFunctionCalling(ctx context.Context, baseURL, apiKey, model string) (service.TestResult, error) {
	return service.TestResult{}, errors.New("client refused the call")
}

// createProviderForTest creates a provider (with alwaysSuccessClient's
// automatic first-key verification, unless r was built with a different
// client) and returns its ID and its first key's ID for tests that need an
// existing provider/key to act on.
func createProviderForTest(t *testing.T, r *gin.Engine, name string) (providerID uint, keyID uint) {
	t.Helper()
	body := map[string]interface{}{
		"name": name, "base_url": "https://api.example.com/v1",
		"key_label": "primary", "key_plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini",
	}
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: create provider %q failed: %d, body: %s", name, w.Code, w.Body.String())
	}
	var view struct {
		ID   uint `json:"id"`
		Keys []struct {
			ID uint `json:"id"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("unmarshal provider view: %v", err)
	}
	if len(view.Keys) == 0 {
		t.Fatalf("expected at least one key in created provider %q", name)
	}
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
	if data.Outcome != int(service.TestSuccess) {
		t.Fatalf("expected outcome %d, got %d", service.TestSuccess, data.Outcome)
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
