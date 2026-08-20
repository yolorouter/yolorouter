package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/apikey"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// apiKeyTestMasterKey returns a deterministic 32-byte AES key for the handler
// tests, mirroring provider_handler_test.go's recipe. The plaintext-reveal
// path decrypts with this same key, so create and reveal must share it.
func apiKeyTestMasterKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

func newAPIKeyTestRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	if err := RegisterValidators(); err != nil {
		t.Fatalf("RegisterValidators failed: %v", err)
	}
	db := testutil.NewSQLiteDB(t)
	svc := apikey.NewAPIKeyService(db, crypto.NewSecretBox(apiKeyTestMasterKey()))
	r := gin.New()
	// PostAPIKey reads the session identity from the context (the created
	// key's owner); stubIdentity stands in for RequireSession here, exactly
	// like the auth handler tests do.
	r.POST("/api/admin/api-keys", stubIdentity(1, "admin"), PostAPIKey(svc))
	r.GET("/api/admin/api-keys/:id/plaintext", GetAPIKeyPlaintext(svc))
	return r, db
}

func postAPIKey(t *testing.T, r *gin.Engine, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/api-keys", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Model-scope create contract with no model seeding required: a custom key
// (allow_all_models=false) must name at least one model, while an all-models
// key legitimately carries no allowlist. gin's required_without only checks the
// slice is non-nil, so an explicit empty [] must be caught by
// the service layer's model-scope validation instead.
func TestPostAPIKeyModelScopeContract(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"empty custom allowlist", map[string]any{"allow_all_models": false, "model_ids": []uint{}}, http.StatusBadRequest},
		{"omitted allowlist on custom", map[string]any{"allow_all_models": false}, http.StatusBadRequest},
		{"all-models needs no ids", map[string]any{"allow_all_models": true, "model_ids": []uint{}}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newAPIKeyTestRouter(t)
			w := postAPIKey(t, r, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestPostAPIKeyAcceptsCustomAllowlist(t *testing.T) {
	r, db := newAPIKeyTestRouter(t)
	now := time.Now().UTC()
	m := &model.Model{Name: "gpt-4o", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	w := postAPIKey(t, r, map[string]any{"allow_all_models": false, "model_ids": []uint{m.ID}})
	if w.Code != http.StatusOK {
		t.Fatalf("valid custom allowlist must succeed, got %d: %s", w.Code, w.Body.String())
	}
}

// newAPIKeyPatchTestRouter wires POST + PATCH + GET so the CAS 409 contract
// can be exercised end-to-end: create a key, read its authoritative
// updated_at via GET, then PATCH with that token.
func newAPIKeyPatchTestRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	if err := RegisterValidators(); err != nil {
		t.Fatalf("RegisterValidators failed: %v", err)
	}
	db := testutil.NewSQLiteDB(t)
	svc := apikey.NewAPIKeyService(db, crypto.NewSecretBox(apiKeyTestMasterKey()))
	r := gin.New()
	r.POST("/api/admin/api-keys", stubIdentity(1, "admin"), PostAPIKey(svc))
	r.GET("/api/admin/api-keys/:id", GetAPIKey(svc))
	r.PATCH("/api/admin/api-keys/:id", PatchAPIKey(svc))
	return r, db
}

func patchAPIKey(t *testing.T, r *gin.Engine, id uint, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/api-keys/"+strconv.Itoa(int(id)), bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func getAPIKeyRaw(t *testing.T, r *gin.Engine, id uint) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/api-keys/"+strconv.Itoa(int(id)), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET api-key %d: status %d body %s", id, w.Code, w.Body.String())
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	return env.Data
}

// TestPatchAPIKeyReturns409OnCASConflict exercises the full optimistic-lock
// contract: a PATCH carrying the row's authoritative updated_at must succeed
// on the first attempt, and a second PATCH reusing that stale token (after the
// first PATCH bumped updated_at) must return 409 with errcode 11013, instead
// of silently overwriting the committed state.
func TestPatchAPIKeyReturns409OnCASConflict(t *testing.T) {
	r, db := newAPIKeyPatchTestRouter(t)
	now := time.Now().UTC()
	m := &model.Model{Name: "cas-model", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	w := postAPIKey(t, r, map[string]any{"allow_all_models": false, "model_ids": []uint{m.ID}})
	if w.Code != http.StatusOK {
		t.Fatalf("create key: status %d body %s", w.Code, w.Body.String())
	}
	var createEnv struct {
		Data struct {
			APIKey struct {
				ID uint `json:"id"`
			} `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createEnv); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	keyID := createEnv.Data.APIKey.ID

	// Read the authoritative updated_at — this is the snapshot the modal would
	// capture on open and send back as expected_updated_at.
	data := getAPIKeyRaw(t, r, keyID)
	staleUpdatedAt, _ := data["updated_at"].(string)
	if staleUpdatedAt == "" {
		t.Fatalf("GET response missing updated_at: %v", data)
	}

	// First PATCH with the fresh token commits and bumps updated_at.
	w1 := patchAPIKey(t, r, keyID, map[string]any{
		"custom_system_prompt_enabled_override": true,
		"custom_system_prompt_enabled":          true,
		"custom_system_prompt":                  "first writer wins",
		"expected_updated_at":                   staleUpdatedAt,
	})
	if w1.Code != http.StatusOK {
		t.Fatalf("fresh CAS PATCH should succeed, got %d: %s", w1.Code, w1.Body.String())
	}

	// Second PATCH reusing the now-stale token must 409 (errcode 11013).
	w2 := patchAPIKey(t, r, keyID, map[string]any{
		"custom_system_prompt": "second writer stale",
		"expected_updated_at":  staleUpdatedAt,
	})
	if w2.Code != http.StatusConflict {
		t.Fatalf("stale CAS PATCH should return 409, got %d: %s", w2.Code, w2.Body.String())
	}
	var conflictEnv struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &conflictEnv); err != nil {
		t.Fatalf("decode conflict body: %v", err)
	}
	if conflictEnv.Code != 11013 {
		t.Fatalf("conflict body should carry errcode 11013, got %d", conflictEnv.Code)
	}
}

// TestPatchAPIKeyWithoutCASKeepsLegacyBehavior confirms that omitting
// expected_updated_at disables CAS entirely — the legacy path used by
// EditKeyModal and CreateKeyModal must keep working unchanged.
func TestPatchAPIKeyWithoutCASKeepsLegacyBehavior(t *testing.T) {
	r, db := newAPIKeyPatchTestRouter(t)
	now := time.Now().UTC()
	m := &model.Model{Name: "nocas-model", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	w := postAPIKey(t, r, map[string]any{"allow_all_models": false, "model_ids": []uint{m.ID}})
	if w.Code != http.StatusOK {
		t.Fatalf("create key: status %d body %s", w.Code, w.Body.String())
	}
	var createEnv struct {
		Data struct {
			APIKey struct {
				ID uint `json:"id"`
			} `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createEnv); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	// No expected_updated_at field — unconditional UPDATE, never 409.
	wp := patchAPIKey(t, r, createEnv.Data.APIKey.ID, map[string]any{"remark": "nocas-remark"})
	if wp.Code != http.StatusOK {
		t.Fatalf("non-CAS PATCH should succeed, got %d: %s", wp.Code, wp.Body.String())
	}
}

// TestPatchAPIKeyCompressOverrideTrueWithoutEnabledReturns400 verifies the
// service-layer combination rule surfaces over HTTP: override=true without
// a compress_enabled value must return 400 with errcode 11015, not 500.
func TestPatchAPIKeyCompressOverrideTrueWithoutEnabledReturns400(t *testing.T) {
	r, db := newAPIKeyPatchTestRouter(t)
	now := time.Now().UTC()
	m := &model.Model{Name: "compress-err-model", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	w := postAPIKey(t, r, map[string]any{"allow_all_models": false, "model_ids": []uint{m.ID}})
	if w.Code != http.StatusOK {
		t.Fatalf("create key: status %d body %s", w.Code, w.Body.String())
	}
	var createEnv struct {
		Data struct {
			APIKey struct {
				ID uint `json:"id"`
			} `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createEnv); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	wp := patchAPIKey(t, r, createEnv.Data.APIKey.ID, map[string]any{
		"compress_enabled_override": true,
	})
	if wp.Code != http.StatusBadRequest {
		t.Fatalf("override=true without enabled should return 400, got %d: %s", wp.Code, wp.Body.String())
	}
	var errEnv struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(wp.Body.Bytes(), &errEnv); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errEnv.Code != 11015 {
		t.Fatalf("error body should carry errcode 11015, got %d", errEnv.Code)
	}
}

// TestPatchAPIKeyCompressOverrideTrueWithEnabledPersists verifies that
// override=true + enabled writes both columns and they are readable via GET.
func TestPatchAPIKeyCompressOverrideTrueWithEnabledPersists(t *testing.T) {
	r, db := newAPIKeyPatchTestRouter(t)
	now := time.Now().UTC()
	m := &model.Model{Name: "compress-ok-model", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	w := postAPIKey(t, r, map[string]any{"allow_all_models": false, "model_ids": []uint{m.ID}})
	if w.Code != http.StatusOK {
		t.Fatalf("create key: status %d body %s", w.Code, w.Body.String())
	}
	var createEnv struct {
		Data struct {
			APIKey struct {
				ID uint `json:"id"`
			} `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createEnv); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	keyID := createEnv.Data.APIKey.ID

	wp := patchAPIKey(t, r, keyID, map[string]any{
		"compress_enabled_override": true,
		"compress_enabled":          true,
	})
	if wp.Code != http.StatusOK {
		t.Fatalf("override=true + enabled should succeed, got %d: %s", wp.Code, wp.Body.String())
	}

	data := getAPIKeyRaw(t, r, keyID)
	if data["compress_enabled_override"] != true {
		t.Fatalf("compress_enabled_override should be true, got %v", data["compress_enabled_override"])
	}
	if data["compress_enabled"] != true {
		t.Fatalf("compress_enabled should be true, got %v", data["compress_enabled"])
	}
}

// TestPatchAPIKeyCompressOverrideFalseStoresFalse verifies that override=false
// zeroes both columns regardless of the enabled value sent alongside.
func TestPatchAPIKeyCompressOverrideFalseStoresFalse(t *testing.T) {
	r, db := newAPIKeyPatchTestRouter(t)
	now := time.Now().UTC()
	m := &model.Model{Name: "compress-off-model", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	w := postAPIKey(t, r, map[string]any{
		"allow_all_models":          false,
		"model_ids":                 []uint{m.ID},
		"compress_enabled_override": true,
		"compress_enabled":          true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create key: status %d body %s", w.Code, w.Body.String())
	}
	var createEnv struct {
		Data struct {
			APIKey struct {
				ID uint `json:"id"`
			} `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createEnv); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	keyID := createEnv.Data.APIKey.ID

	wp := patchAPIKey(t, r, keyID, map[string]any{
		"compress_enabled_override": false,
		"compress_enabled":          true,
	})
	if wp.Code != http.StatusOK {
		t.Fatalf("override=false should succeed, got %d: %s", wp.Code, wp.Body.String())
	}

	data := getAPIKeyRaw(t, r, keyID)
	if data["compress_enabled_override"] != false {
		t.Fatalf("compress_enabled_override should be false, got %v", data["compress_enabled_override"])
	}
	if data["compress_enabled"] != false {
		t.Fatalf("compress_enabled should be false, got %v", data["compress_enabled"])
	}
}

// TestPostAPIKeyCompressFieldsPersisted verifies that compress fields supplied
// at create time are stored and surfaced in the response.
func TestPostAPIKeyCompressFieldsPersisted(t *testing.T) {
	r, db := newAPIKeyPatchTestRouter(t)
	now := time.Now().UTC()
	m := &model.Model{Name: "compress-create-model", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	w := postAPIKey(t, r, map[string]any{
		"allow_all_models":          false,
		"model_ids":                 []uint{m.ID},
		"compress_enabled_override": true,
		"compress_enabled":          true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create with compress should succeed, got %d: %s", w.Code, w.Body.String())
	}
	var createEnv struct {
		Data struct {
			APIKey struct {
				CompressEnabledOverride bool `json:"compress_enabled_override"`
				CompressEnabled         bool `json:"compress_enabled"`
			} `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createEnv); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if !createEnv.Data.APIKey.CompressEnabledOverride {
		t.Fatalf("compress_enabled_override should be true in create response")
	}
	if !createEnv.Data.APIKey.CompressEnabled {
		t.Fatalf("compress_enabled should be true in create response")
	}
}

// TestGetAPIKeyPlaintextEndToEnd exercises the full reveal path: POST creates
// a key (handing back the plaintext once), then GET /:id/plaintext must return
// that same plaintext. The plaintext_key field name matches the create
// response so the frontend reuses one code path.
func TestGetAPIKeyPlaintextEndToEnd(t *testing.T) {
	r, db := newAPIKeyTestRouter(t)
	now := time.Now().UTC()
	m := &model.Model{Name: "reveal-model", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	w := postAPIKey(t, r, map[string]any{"allow_all_models": true})
	if w.Code != http.StatusOK {
		t.Fatalf("create key: status %d body %s", w.Code, w.Body.String())
	}
	var createEnv struct {
		Data struct {
			PlaintextKey string `json:"plaintext_key"`
			APIKey       struct {
				ID uint `json:"id"`
			} `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createEnv); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if !strings.HasPrefix(createEnv.Data.PlaintextKey, "sk-yr-") {
		t.Fatalf("create response missing plaintext_key, got %q", createEnv.Data.PlaintextKey)
	}

	// Reveal: GET /api-keys/:id/plaintext must return the same plaintext.
	req := httptest.NewRequest(http.MethodGet, "/api/admin/api-keys/"+strconv.Itoa(int(createEnv.Data.APIKey.ID))+"/plaintext", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req)
	if w2.Code != http.StatusOK {
		t.Fatalf("reveal: status %d body %s", w2.Code, w2.Body.String())
	}
	var revealEnv struct {
		Data struct {
			PlaintextKey string `json:"plaintext_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &revealEnv); err != nil {
		t.Fatalf("decode reveal response: %v", err)
	}
	if revealEnv.Data.PlaintextKey != createEnv.Data.PlaintextKey {
		t.Fatalf("revealed plaintext %q != create-time plaintext %q", revealEnv.Data.PlaintextKey, createEnv.Data.PlaintextKey)
	}

	// Both responses carry a credential in their body, so neither may be kept
	// by a browser's disk cache or a shared proxy: a stored copy outlives the
	// session it was fetched in and is readable by whoever has the machine
	// afterwards. The reveal is a GET, the shape caches are most willing to
	// store, which is why it is asserted alongside the create.
	for _, resp := range []struct {
		name string
		w    *httptest.ResponseRecorder
	}{{"create", w}, {"reveal", w2}} {
		if got := resp.w.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Errorf("%s response Cache-Control = %q, want it to forbid storing the key", resp.name, got)
		}
	}
}

// TestGetAPIKeyPlaintextLegacyRowReturns11016 seeds a pre-00021 row (no
// encrypted_key) and asserts the reveal endpoint returns the dedicated code,
// so the frontend can show "this key predates the feature, create a new one".
func TestGetAPIKeyPlaintextLegacyRowReturns11016(t *testing.T) {
	r, db := newAPIKeyTestRouter(t)
	now := time.Now().UTC()
	// Seed directly — the create path always populates encrypted_key, so a
	// legacy row must be seeded by hand to exercise the empty-column branch.
	legacy := &model.APIKey{
		KeyHash: "a-fixed-sha256-hash-value-here", KeyPrefix: "sk-yr-legacy000",
		Status: model.APIKeyStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(legacy).Error; err != nil {
		t.Fatalf("seed legacy key: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/api-keys/"+strconv.Itoa(int(legacy.ID))+"/plaintext", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// 11016 is a client error (the key's state doesn't support reveal), so the
	// envelope maps it to 4xx — assert the business code in the body, not 200.
	var env struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode legacy reveal body (status %d): %v body %s", w.Code, err, w.Body.String())
	}
	if env.Code != errcode.APIKeyPlaintextUnavailable {
		t.Fatalf("expected errcode %d for legacy row, got %d (status %d body %s)", errcode.APIKeyPlaintextUnavailable, env.Code, w.Code, w.Body.String())
	}
}
