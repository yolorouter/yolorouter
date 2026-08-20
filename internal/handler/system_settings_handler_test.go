package handler

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/service/systemsettings"
)

func setupSettingsRouter(t *testing.T) (*gin.Engine, *systemsettings.SystemSettingsService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE system_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('custom_system_prompt_enabled','false'),('custom_system_prompt','')`)
	svc := systemsettings.NewSystemSettingsService(db)
	r := gin.New()
	r.GET("/api/admin/system-settings/custom-system-prompt", GetCustomSystemPrompt(svc))
	r.PUT("/api/admin/system-settings/custom-system-prompt", PutCustomSystemPrompt(svc))
	r.GET("/api/admin/system-settings/input-compression", GetInputCompression(svc))
	r.PUT("/api/admin/system-settings/input-compression", PutInputCompression(svc))
	return r, svc
}

func TestGetCustomSystemPromptReturnsSeeded(t *testing.T) {
	r, _ := setupSettingsRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/system-settings/custom-system-prompt", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Enabled bool   `json:"enabled"`
			Text    string `json:"text"`
			Version int64  `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Data.Enabled || resp.Data.Text != "" || resp.Data.Version != 1 {
		t.Fatalf("unexpected payload: %+v", resp.Data)
	}
}

func TestPutCustomSystemPromptMissingFields400(t *testing.T) {
	r, _ := setupSettingsRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/admin/system-settings/custom-system-prompt", bytes.NewBufferString(`{}`)))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPutCustomSystemPromptSuccessReturnsNewVersion(t *testing.T) {
	r, _ := setupSettingsRouter(t)
	body, _ := json.Marshal(map[string]interface{}{"enabled": true, "text": "hi", "version": int64(1)})
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/custom-system-prompt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Version int64 `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Data.Version != 2 {
		t.Fatalf("new version = %d, want 2", resp.Data.Version)
	}
}

func TestPutCustomSystemPromptStaleVersion409(t *testing.T) {
	r, _ := setupSettingsRouter(t)
	body, _ := json.Marshal(map[string]interface{}{"enabled": false, "text": "", "version": int64(99)})
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/custom-system-prompt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 409 {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

// setupSettingsRouterWithIC builds a fresh router+DB seeded with both the CSP
// rows and the input_compression_enabled row at v1 disabled, so the IC tests
// mirror how the CSP tests rely on setupSettingsRouter's seeded state.
func setupSettingsRouterWithIC(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE system_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('custom_system_prompt_enabled','false'),('custom_system_prompt',''),('input_compression_enabled','false')`)
	svc := systemsettings.NewSystemSettingsService(db)
	r := gin.New()
	r.GET("/api/admin/system-settings/input-compression", GetInputCompression(svc))
	r.PUT("/api/admin/system-settings/input-compression", PutInputCompression(svc))
	return r
}

func TestGetInputCompressionReturnsSeeded(t *testing.T) {
	r := setupSettingsRouterWithIC(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/system-settings/input-compression", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Enabled bool  `json:"enabled"`
			Version int64 `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Data.Enabled || resp.Data.Version != 1 {
		t.Fatalf("unexpected payload: %+v", resp.Data)
	}
}

func TestPutInputCompressionMissingFields400(t *testing.T) {
	r := setupSettingsRouterWithIC(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/admin/system-settings/input-compression", bytes.NewBufferString(`{}`)))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPutInputCompressionZeroVersion400(t *testing.T) {
	r := setupSettingsRouterWithIC(t)
	// version=0 must be rejected even with enabled present.
	body, _ := json.Marshal(map[string]interface{}{"enabled": true, "version": int64(0)})
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/input-compression", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPutInputCompressionSuccessReturnsNewVersion(t *testing.T) {
	r := setupSettingsRouterWithIC(t)
	body, _ := json.Marshal(map[string]interface{}{"enabled": true, "version": int64(1)})
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/input-compression", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Enabled bool  `json:"enabled"`
			Version int64 `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.Data.Enabled || resp.Data.Version != 2 {
		t.Fatalf("want enabled=true/v2, got enabled=%v v%d", resp.Data.Enabled, resp.Data.Version)
	}
}

// TestPutInputCompressionConflictEmits11014 verifies the 409 response carries
// errcode 11014 (InputCompressionConflict), NOT 11012 (CustomSystemPromptConflict)
// — the two settings share a status code but distinct business codes so the
// frontend can route the retry to the right control.
func TestPutInputCompressionConflictEmits11014(t *testing.T) {
	r := setupSettingsRouterWithIC(t)
	body, _ := json.Marshal(map[string]interface{}{"enabled": false, "version": int64(99)})
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/input-compression", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 409 {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	var resp struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Code != 11014 {
		t.Fatalf("errcode = %d, want 11014 (InputCompressionConflict, NOT 11012)", resp.Code)
	}
}

// setupVisionFallbackRouter is a dedicated harness: the vision-fallback pair
// seeded, plus a models table so the save-time model-name validation has
// something real to check against.
func setupVisionFallbackRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE system_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('vision_fallback_model',''),('vision_fallback_prompt','')`)
	db.Exec(`CREATE TABLE models (id INTEGER PRIMARY KEY, name TEXT, management_status INTEGER DEFAULT 1, supports_image_input INTEGER NULL, created_at DATETIME, updated_at DATETIME)`)
	db.Exec(`INSERT INTO models (id, name) VALUES (1, 'glm-4v')`)
	svc := systemsettings.NewSystemSettingsService(db)
	r := gin.New()
	r.GET("/api/admin/system-settings/vision-fallback", GetVisionFallback(svc))
	r.PUT("/api/admin/system-settings/vision-fallback", PutVisionFallback(svc))
	return r
}

func TestPutVisionFallbackMissingFields400(t *testing.T) {
	r := setupVisionFallbackRouter(t)
	body, _ := json.Marshal(map[string]any{"model": "glm-4v"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/vision-fallback", bytes.NewReader(body))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 for missing prompt/version", w.Code)
	}
}

// An unknown model name must be rejected at save time — a typo here would
// otherwise silently disable the feature at describe time.
func TestPutVisionFallbackRejectsUnknownModel(t *testing.T) {
	r := setupVisionFallbackRouter(t)
	body, _ := json.Marshal(map[string]any{"model": "no-such-model", "prompt": "", "version": 1})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/admin/system-settings/vision-fallback", bytes.NewReader(body)))
	var resp struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Code != 11018 {
		t.Fatalf("code = %d, want 11018 (unknown model), body: %s", resp.Code, w.Body.String())
	}
}

func TestPutVisionFallbackSuccessAndStaleVersion409(t *testing.T) {
	r := setupVisionFallbackRouter(t)
	body, _ := json.Marshal(map[string]any{"model": "glm-4v", "prompt": "look closely", "version": 1})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/admin/system-settings/vision-fallback", bytes.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("first put: status = %d, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Model   string `json:"model"`
			Prompt  string `json:"prompt"`
			Version int64  `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Data.Model != "glm-4v" || resp.Data.Prompt != "look closely" || resp.Data.Version != 2 {
		t.Fatalf("payload = %+v, want committed snapshot at version 2", resp.Data)
	}
	// Clearing the model (= disabling) with the stale version must 409.
	body, _ = json.Marshal(map[string]any{"model": "", "prompt": "", "version": 1})
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/admin/system-settings/vision-fallback", bytes.NewReader(body)))
	if w.Code != 409 {
		t.Fatalf("stale put: status = %d, want 409, body: %s", w.Code, w.Body.String())
	}
	// An empty model needs no models-table lookup and disables the feature.
	body, _ = json.Marshal(map[string]any{"model": "", "prompt": "", "version": 2})
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/admin/system-settings/vision-fallback", bytes.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("disable put: status = %d, body: %s", w.Code, w.Body.String())
	}
}
