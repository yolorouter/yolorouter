package handler

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
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

// setupKeyAutoRecoveryRouter seeds the key-auto-recovery pair at the
// migration default (enabled + 30 minutes) so the KAR tests mirror how the
// other families' tests rely on their seeded state.
func setupKeyAutoRecoveryRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE system_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('key_auto_recovery_enabled','true'),('key_auto_recovery_interval_minutes','30')`)
	svc := systemsettings.NewSystemSettingsService(db)
	r := gin.New()
	r.GET("/api/admin/system-settings/key-auto-recovery", GetKeyAutoRecovery(svc))
	r.PUT("/api/admin/system-settings/key-auto-recovery", PutKeyAutoRecovery(svc))
	return r
}

func TestGetKeyAutoRecoveryReturnsSeeded(t *testing.T) {
	r := setupKeyAutoRecoveryRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/system-settings/key-auto-recovery", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Enabled         bool  `json:"enabled"`
			IntervalMinutes int   `json:"interval_minutes"`
			Version         int64 `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.Data.Enabled || resp.Data.IntervalMinutes != 30 || resp.Data.Version != 1 {
		t.Fatalf("unexpected payload: %+v", resp.Data)
	}
}

func TestPutKeyAutoRecoveryMissingFields400(t *testing.T) {
	r := setupKeyAutoRecoveryRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/admin/system-settings/key-auto-recovery", bytes.NewBufferString(`{}`)))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestPutKeyAutoRecoveryZeroVersion400 verifies the no-first-write rule: a
// version of 0 (what a missing-row read would report on a not-yet-migrated
// database) is rejected, so the row can only ever be created by the seeding
// migration — the PUT handler stays as strict as the other settings.
func TestPutKeyAutoRecoveryZeroVersion400(t *testing.T) {
	r := setupKeyAutoRecoveryRouter(t)
	body, _ := json.Marshal(map[string]interface{}{"enabled": true, "interval_minutes": 30, "version": int64(0)})
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/key-auto-recovery", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// A non-integer interval or a non-boolean enabled fails JSON binding and is
// rejected with the existing param-error form.
func TestPutKeyAutoRecoveryNonIntegerInterval400(t *testing.T) {
	r := setupKeyAutoRecoveryRouter(t)
	body, _ := json.Marshal(map[string]interface{}{"enabled": true, "interval_minutes": 3.5, "version": int64(1)})
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/key-auto-recovery", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 for interval 3.5, body: %s", w.Code, w.Body.String())
	}
}

func TestPutKeyAutoRecoveryNonBooleanEnabled400(t *testing.T) {
	r := setupKeyAutoRecoveryRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/admin/system-settings/key-auto-recovery", bytes.NewBufferString(`{"enabled":"yes","interval_minutes":30,"version":1}`)))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 for string enabled, body: %s", w.Code, w.Body.String())
	}
}

// Out-of-range integers pass binding and are rejected by the service-layer
// bounds check with this setting's own business code (11020), the same
// 400-with-errcode form the vision-fallback family uses for validation.
func TestPutKeyAutoRecoveryIntervalOutOfBoundsEmits11020(t *testing.T) {
	r := setupKeyAutoRecoveryRouter(t)
	for _, bad := range []int{0, -1, 1441} {
		body, _ := json.Marshal(map[string]interface{}{"enabled": true, "interval_minutes": bad, "version": int64(1)})
		req := httptest.NewRequest("PUT", "/api/admin/system-settings/key-auto-recovery", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("interval %d: status = %d, want 400, body: %s", bad, w.Code, w.Body.String())
		}
		var resp struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("interval %d: unmarshal: %v", bad, err)
		}
		if resp.Code != 11020 {
			t.Fatalf("interval %d: errcode = %d, want 11020 (KeyAutoRecoveryIntervalInvalid)", bad, resp.Code)
		}
	}
}

func TestPutKeyAutoRecoverySuccessReturnsNewVersion(t *testing.T) {
	r := setupKeyAutoRecoveryRouter(t)
	body, _ := json.Marshal(map[string]interface{}{"enabled": false, "interval_minutes": 45, "version": int64(1)})
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/key-auto-recovery", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Enabled         bool  `json:"enabled"`
			IntervalMinutes int   `json:"interval_minutes"`
			Version         int64 `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Data.Enabled || resp.Data.IntervalMinutes != 45 || resp.Data.Version != 2 {
		t.Fatalf("want disabled/45/v2, got %+v", resp.Data)
	}
}

// TestPutKeyAutoRecoveryConflictEmits11019 verifies the 409 response carries
// errcode 11019 (KeyAutoRecoveryConflict), distinct from the other settings'
// conflict codes so the frontend can route the retry to the right control.
func TestPutKeyAutoRecoveryConflictEmits11019(t *testing.T) {
	r := setupKeyAutoRecoveryRouter(t)
	body, _ := json.Marshal(map[string]interface{}{"enabled": false, "interval_minutes": 45, "version": int64(99)})
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/key-auto-recovery", bytes.NewReader(body))
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
	if resp.Code != 11019 {
		t.Fatalf("errcode = %d, want 11019 (KeyAutoRecoveryConflict)", resp.Code)
	}
}

// --- Request log retention ---------------------------------------------------

// setupRequestLogRetentionRouter seeds the request_log_retention_days row at
// the migration default (0 = keep forever). The gorm handle comes back too:
// the fail-open tests need to delete the row out from under the service.
func setupRequestLogRetentionRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE system_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('request_log_retention_days','0')`)
	svc := systemsettings.NewSystemSettingsService(db)
	r := gin.New()
	r.GET("/api/admin/system-settings/request-log-retention", GetRequestLogRetention(svc))
	r.PUT("/api/admin/system-settings/request-log-retention", PutRequestLogRetention(svc))
	return r, db
}

// getRequestLogRetentionDays is the shared GET-and-extract helper for the
// retention tests: status must be 200 and the payload's retention_days /
// version come back for the caller to assert on.
func getRequestLogRetentionDays(t *testing.T, r *gin.Engine) (int, int64) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/system-settings/request-log-retention", nil))
	if w.Code != 200 {
		t.Fatalf("GET: status = %d, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			RetentionDays int   `json:"retention_days"`
			Version       int64 `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp.Data.RetentionDays, resp.Data.Version
}

// putRequestLogRetentionRaw issues a PUT with an exact raw JSON body so the
// invalid-matrix tests control the wire form byte for byte.
func putRequestLogRetentionRaw(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/request-log-retention", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestGetRequestLogRetentionReturnsSeededZero pins the seeded read: the
// migration-planted row reads back as 0 (keep forever) at its seed version.
func TestGetRequestLogRetentionReturnsSeededZero(t *testing.T) {
	r, _ := setupRequestLogRetentionRouter(t)
	days, ver := getRequestLogRetentionDays(t, r)
	if days != 0 || ver != 1 {
		t.Fatalf("want 0/v1 seeded, got %d/v%d", days, ver)
	}
}

// TestGetRequestLogRetentionMissingRowFailOpensToZero pins the fail-open
// read on a database that predates the seeding migration: the absent row
// must read as the keep-forever default (version 0), never as an error —
// the admin console and the cleanup loop both degrade to "delete nothing".
func TestGetRequestLogRetentionMissingRowFailOpensToZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE system_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	svc := systemsettings.NewSystemSettingsService(db)
	r := gin.New()
	r.GET("/api/admin/system-settings/request-log-retention", GetRequestLogRetention(svc))
	r.PUT("/api/admin/system-settings/request-log-retention", PutRequestLogRetention(svc))

	days, ver := getRequestLogRetentionDays(t, r)
	if days != 0 || ver != 0 {
		t.Fatalf("want 0/v0 (keep-forever default on missing row), got %d/v%d", days, ver)
	}
}

// TestPutRequestLogRetentionSuccessAndReadBack pins the write half of the
// matrix: PUT 30 commits (new version handed back) and the authoritative
// GET reads 30.
func TestPutRequestLogRetentionSuccessAndReadBack(t *testing.T) {
	r, _ := setupRequestLogRetentionRouter(t)
	w := putRequestLogRetentionRaw(t, r, `{"retention_days":30,"version":1}`)
	if w.Code != 200 {
		t.Fatalf("PUT 30: status = %d, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			RetentionDays int   `json:"retention_days"`
			Version       int64 `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Data.RetentionDays != 30 || resp.Data.Version != 2 {
		t.Fatalf("want 30/v2, got %d/v%d", resp.Data.RetentionDays, resp.Data.Version)
	}
	if days, ver := getRequestLogRetentionDays(t, r); days != 30 || ver != 2 {
		t.Fatalf("read after PUT: want 30/v2, got %d/v%d", days, ver)
	}
}

// TestGetRequestLogRetentionAfterRowsDeletedFailOpensToZero pins the
// fail-open read after the settings rows are wiped: the read falls back to
// the keep-forever default (0/v0) instead of erroring, so a damaged
// settings table can never fail the cleanup loop into deleting anything.
func TestGetRequestLogRetentionAfterRowsDeletedFailOpensToZero(t *testing.T) {
	r, db := setupRequestLogRetentionRouter(t)
	if days, _ := getRequestLogRetentionDays(t, r); days != 0 {
		t.Fatalf("precondition: seeded read = %d, want 0", days)
	}
	if res := db.Exec(`DELETE FROM system_settings`); res.Error != nil {
		t.Fatalf("wipe settings rows: %v", res.Error)
	}
	days, ver := getRequestLogRetentionDays(t, r)
	if days != 0 || ver != 0 {
		t.Fatalf("want 0/v0 (fail-open after rows deleted), got %d/v%d", days, ver)
	}
}

func TestPutRequestLogRetentionMissingFields400(t *testing.T) {
	r, _ := setupRequestLogRetentionRouter(t)
	w := putRequestLogRetentionRaw(t, r, `{}`)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestPutRequestLogRetentionZeroVersion400 verifies the no-first-write
// rule: a version of 0 (what a missing-row read reports on a not-yet-
// migrated database) is rejected, so the row can only ever be created by
// the seeding migration — the PUT handler stays as strict as the other
// settings families.
func TestPutRequestLogRetentionZeroVersion400(t *testing.T) {
	r, _ := setupRequestLogRetentionRouter(t)
	w := putRequestLogRetentionRaw(t, r, `{"retention_days":30,"version":0}`)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestPutRequestLogRetentionLegalDomainAccepted pins the inclusive domain:
// 0 (keep forever), 1 and 3650 all commit; each accepted PUT advances the
// version so the next one carries the fresh version.
func TestPutRequestLogRetentionLegalDomainAccepted(t *testing.T) {
	r, _ := setupRequestLogRetentionRouter(t)
	for i, tc := range []struct {
		days    int
		version int64
	}{
		{days: 0, version: 1},
		{days: 1, version: 2},
		{days: 3650, version: 3},
	} {
		body, _ := json.Marshal(map[string]interface{}{"retention_days": tc.days, "version": tc.version})
		req := httptest.NewRequest("PUT", "/api/admin/system-settings/request-log-retention", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("case %d (days=%d): status = %d, want 200, body: %s", i, tc.days, w.Code, w.Body.String())
		}
	}
	if days, _ := getRequestLogRetentionDays(t, r); days != 3650 {
		t.Fatalf("final read = %d, want 3650", days)
	}
}

// TestPutRequestLogRetentionInvalidValuesRejected pins the invalid half of
// the domain matrix: -1 / 3651 pass JSON binding but are refused by the
// service-layer bounds check with this setting's own business code (11022);
// 1.5 / "" / "abc" fail JSON binding outright. Every rejection must leave
// the stored setting untouched.
func TestPutRequestLogRetentionInvalidValuesRejected(t *testing.T) {
	r, _ := setupRequestLogRetentionRouter(t)
	// Establish a non-zero stored value so "setting unchanged" is a real
	// assertion (0 is also the default, which would mask a no-op write).
	if w := putRequestLogRetentionRaw(t, r, `{"retention_days":30,"version":1}`); w.Code != 200 {
		t.Fatalf("seed PUT 30: status = %d, body: %s", w.Code, w.Body.String())
	}

	// Integer values outside the domain: 400 carrying errcode 11022.
	for _, bad := range []string{"-1", "3651"} {
		w := putRequestLogRetentionRaw(t, r, `{"retention_days":`+bad+`,"version":2}`)
		if w.Code != 400 {
			t.Fatalf("days %s: status = %d, want 400, body: %s", bad, w.Code, w.Body.String())
		}
		var resp struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("days %s: unmarshal: %v", bad, err)
		}
		if resp.Code != 11022 {
			t.Fatalf("days %s: errcode = %d, want 11022 (RequestLogRetentionDaysInvalid)", bad, resp.Code)
		}
		if days, ver := getRequestLogRetentionDays(t, r); days != 30 || ver != 2 {
			t.Fatalf("days %s: setting changed to %d/v%d, want 30/v2 unchanged", bad, days, ver)
		}
	}

	// Values that never bind into an int field: rejected by JSON decoding.
	for _, bad := range []string{`1.5`, `""`, `"abc"`} {
		w := putRequestLogRetentionRaw(t, r, `{"retention_days":`+bad+`,"version":2}`)
		if w.Code != 400 {
			t.Fatalf("days %s: status = %d, want 400, body: %s", bad, w.Code, w.Body.String())
		}
		if days, ver := getRequestLogRetentionDays(t, r); days != 30 || ver != 2 {
			t.Fatalf("days %s: setting changed to %d/v%d, want 30/v2 unchanged", bad, days, ver)
		}
	}
}

// TestPutRequestLogRetentionConflictEmits11021 verifies the 409 response
// carries errcode 11021 (RequestLogRetentionConflict), distinct from the
// other settings' conflict codes so the frontend can route the retry to the
// right control.
func TestPutRequestLogRetentionConflictEmits11021(t *testing.T) {
	r, _ := setupRequestLogRetentionRouter(t)
	w := putRequestLogRetentionRaw(t, r, `{"retention_days":30,"version":99}`)
	if w.Code != 409 {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	var resp struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Code != 11021 {
		t.Fatalf("errcode = %d, want 11021 (RequestLogRetentionConflict)", resp.Code)
	}
}

// --- Admin audit retention --------------------------------------------------

// setupAdminAuditRetentionRouter seeds the admin_audit_retention_days row
// at the migration default (180 days). The gorm handle comes back too: the
// fail-open tests need to delete the row out from under the service.
func setupAdminAuditRetentionRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE system_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('admin_audit_retention_days','180')`)
	svc := systemsettings.NewSystemSettingsService(db)
	r := gin.New()
	r.GET("/api/admin/system-settings/admin-audit-retention", GetAdminAuditRetention(svc))
	r.PUT("/api/admin/system-settings/admin-audit-retention", PutAdminAuditRetention(svc))
	return r, db
}

// getAdminAuditRetentionDays is the shared GET-and-extract helper for the
// audit retention tests: status must be 200 and the payload's
// retention_days / version come back for the caller to assert on.
func getAdminAuditRetentionDays(t *testing.T, r *gin.Engine) (int, int64) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/system-settings/admin-audit-retention", nil))
	if w.Code != 200 {
		t.Fatalf("GET: status = %d, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			RetentionDays int   `json:"retention_days"`
			Version       int64 `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp.Data.RetentionDays, resp.Data.Version
}

// putAdminAuditRetentionRaw issues a PUT with an exact raw JSON body so the
// invalid-matrix tests control the wire form byte for byte.
func putAdminAuditRetentionRaw(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/admin-audit-retention", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestGetAdminAuditRetentionReturnsSeededDefault pins the seeded read: the
// migration-planted row reads back as 180 at its seed version.
func TestGetAdminAuditRetentionReturnsSeededDefault(t *testing.T) {
	r, _ := setupAdminAuditRetentionRouter(t)
	days, ver := getAdminAuditRetentionDays(t, r)
	if days != 180 || ver != 1 {
		t.Fatalf("want 180/v1 seeded, got %d/v%d", days, ver)
	}
}

// TestGetAdminAuditRetentionMissingRowFailOpensToDefault pins the
// fail-open read on a database that predates the seeding migration: the
// absent row must read as the shipped 180-day default (version 0), never
// as an error — the admin console and the retention loop both degrade to
// the same window a freshly seeded deployment ships with.
func TestGetAdminAuditRetentionMissingRowFailOpensToDefault(t *testing.T) {
	r, db := setupAdminAuditRetentionRouter(t)
	if res := db.Exec(`DELETE FROM system_settings WHERE key = 'admin_audit_retention_days'`); res.Error != nil {
		t.Fatalf("drop the seeded row: %v", res.Error)
	}
	days, ver := getAdminAuditRetentionDays(t, r)
	if days != 180 || ver != 0 {
		t.Fatalf("want 180/v0 (shipped default on missing row), got %d/v%d", days, ver)
	}
}

// TestPutAdminAuditRetentionSuccessAndReadBack pins the write half: PUT 30
// commits (new version handed back) and the authoritative GET reads 30.
func TestPutAdminAuditRetentionSuccessAndReadBack(t *testing.T) {
	r, _ := setupAdminAuditRetentionRouter(t)
	w := putAdminAuditRetentionRaw(t, r, `{"retention_days":30,"version":1}`)
	if w.Code != 200 {
		t.Fatalf("PUT 30: status = %d, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			RetentionDays int   `json:"retention_days"`
			Version       int64 `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Data.RetentionDays != 30 || resp.Data.Version != 2 {
		t.Fatalf("want 30/v2, got %d/v%d", resp.Data.RetentionDays, resp.Data.Version)
	}
	if days, ver := getAdminAuditRetentionDays(t, r); days != 30 || ver != 2 {
		t.Fatalf("read after PUT: want 30/v2, got %d/v%d", days, ver)
	}
}

// TestPutAdminAuditRetentionMissingFields400 and the zero-version form pin
// the request shape: retention_days and version are both required, and a
// version of 0 (what a missing-row read reports) is refused so the row can
// only ever be created by the seeding migration.
func TestPutAdminAuditRetentionMissingFields400(t *testing.T) {
	r, _ := setupAdminAuditRetentionRouter(t)
	for _, body := range []string{`{}`, `{"retention_days":30}`, `{"version":1}`} {
		w := putAdminAuditRetentionRaw(t, r, body)
		if w.Code != 400 {
			t.Fatalf("body %s: status = %d, want 400", body, w.Code)
		}
	}
}

func TestPutAdminAuditRetentionZeroVersion400(t *testing.T) {
	r, _ := setupAdminAuditRetentionRouter(t)
	w := putAdminAuditRetentionRaw(t, r, `{"retention_days":30,"version":0}`)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestPutAdminAuditRetentionLegalDomainAccepted pins the inclusive domain:
// 0 (keep forever), 1 and 3650 all commit; each accepted PUT advances the
// version so the next one carries the fresh version.
func TestPutAdminAuditRetentionLegalDomainAccepted(t *testing.T) {
	r, _ := setupAdminAuditRetentionRouter(t)
	step := func(ver int64, days int) int64 {
		t.Helper()
		body, _ := json.Marshal(map[string]interface{}{"retention_days": days, "version": ver})
		w := putAdminAuditRetentionRaw(t, r, string(body))
		if w.Code != 200 {
			t.Fatalf("PUT days=%d ver=%d: status %d, body %s", days, ver, w.Code, w.Body.String())
		}
		var resp struct {
			Data struct {
				Version int64 `json:"version"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return resp.Data.Version
	}
	if ver := step(1, 0); ver != 2 {
		t.Fatalf("days 0: want v2, got v%d", ver)
	}
	if ver := step(2, 1); ver != 3 {
		t.Fatalf("days 1: want v3, got v%d", ver)
	}
	if ver := step(3, 3650); ver != 4 {
		t.Fatalf("days 3650: want v4, got v%d", ver)
	}
}

// TestPutAdminAuditRetentionInvalidValuesRejected pins the domain's far
// side, mirroring the request-log twin's split: integer values outside the
// domain answer 400 carrying the setting's own business code (11024) and
// leave the stored value untouched, while values that never bind into an
// int field are rejected by JSON decoding with the generic 400.
func TestPutAdminAuditRetentionInvalidValuesRejected(t *testing.T) {
	r, _ := setupAdminAuditRetentionRouter(t)
	for _, bad := range []string{"-1", "3651"} {
		w := putAdminAuditRetentionRaw(t, r, `{"retention_days":`+bad+`,"version":1}`)
		if w.Code != 400 {
			t.Fatalf("days %s: status = %d, want 400, body: %s", bad, w.Code, w.Body.String())
		}
		var resp struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("days %s: unmarshal: %v", bad, err)
		}
		if resp.Code != 11024 {
			t.Fatalf("days %s: errcode = %d, want 11024 (AdminAuditRetentionDaysInvalid)", bad, resp.Code)
		}
		if days, ver := getAdminAuditRetentionDays(t, r); days != 180 || ver != 1 {
			t.Fatalf("days %s: setting changed to %d/v%d, want 180/v1 unchanged", bad, days, ver)
		}
	}
	// Values that never bind into an int field: rejected by JSON decoding.
	for _, bad := range []string{`1.5`, `""`, `"abc"`} {
		w := putAdminAuditRetentionRaw(t, r, `{"retention_days":`+bad+`,"version":1}`)
		if w.Code != 400 {
			t.Fatalf("days %s: status = %d, want 400, body: %s", bad, w.Code, w.Body.String())
		}
		if days, ver := getAdminAuditRetentionDays(t, r); days != 180 || ver != 1 {
			t.Fatalf("days %s: setting changed to %d/v%d, want 180/v1 unchanged", bad, days, ver)
		}
	}
}

// TestPutAdminAuditRetentionConflictEmits11023 verifies the 409 response
// carries errcode 11023 (AdminAuditRetentionConflict), distinct from the
// other settings' conflict codes so the frontend can route the retry to
// the right control.
func TestPutAdminAuditRetentionConflictEmits11023(t *testing.T) {
	r, _ := setupAdminAuditRetentionRouter(t)
	w := putAdminAuditRetentionRaw(t, r, `{"retention_days":30,"version":99}`)
	if w.Code != 409 {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	var resp struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Code != 11023 {
		t.Fatalf("errcode = %d, want 11023 (AdminAuditRetentionConflict)", resp.Code)
	}
}

// --- Outbound proxy ----------------------------------------------------------

// setupOutboundProxyRouter seeds the proxy pair at the migration default
// (mode off, empty URL). The gorm handle comes back too: the fail-open test
// needs to delete the rows out from under the service.
func setupOutboundProxyRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE system_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('proxy_mode','off'),('proxy_url','')`)
	svc := systemsettings.NewSystemSettingsService(db)
	r := gin.New()
	r.GET("/api/admin/system-settings/outbound-proxy", GetOutboundProxy(svc))
	r.PUT("/api/admin/system-settings/outbound-proxy", PutOutboundProxy(svc))
	return r, db
}

// getOutboundProxySetting is the shared GET-and-extract helper for the
// outbound proxy tests: status must be 200 and the payload's mode / url /
// version come back for the caller to assert on.
func getOutboundProxySetting(t *testing.T, r *gin.Engine) (mode, url string, ver int64) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/system-settings/outbound-proxy", nil))
	if w.Code != 200 {
		t.Fatalf("GET: status = %d, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Mode    string `json:"mode"`
			URL     string `json:"url"`
			Version int64  `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp.Data.Mode, resp.Data.URL, resp.Data.Version
}

// putOutboundProxyRaw issues a PUT with an exact raw JSON body so the
// invalid-value matrix controls the wire form byte for byte.
func putOutboundProxyRaw(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/admin/system-settings/outbound-proxy", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestGetOutboundProxyReturnsSeededDefault pins the seeded read: the
// migration-planted pair reads back as off with an empty URL at its seed
// version — upgrading changes no behavior.
func TestGetOutboundProxyReturnsSeededDefault(t *testing.T) {
	r, _ := setupOutboundProxyRouter(t)
	mode, url, ver := getOutboundProxySetting(t, r)
	if mode != "off" || url != "" || ver != 1 {
		t.Fatalf("want off/''/v1 seeded, got %q/%q/v%d", mode, url, ver)
	}
}

// TestGetOutboundProxyMissingRowsFailOpenToOff pins the fail-open read on a
// database that predates the seeding migration: the absent pair must read
// as the shipped off/direct default (version 0), never as an error — the
// console and the transport resolution path degrade to the same direct
// default a freshly seeded deployment ships with.
func TestGetOutboundProxyMissingRowsFailOpenToOff(t *testing.T) {
	r, db := setupOutboundProxyRouter(t)
	if res := db.Exec(`DELETE FROM system_settings WHERE key IN ('proxy_mode','proxy_url')`); res.Error != nil {
		t.Fatalf("drop the seeded rows: %v", res.Error)
	}
	mode, url, ver := getOutboundProxySetting(t, r)
	if mode != "off" || url != "" || ver != 0 {
		t.Fatalf("want off/''/v0 (shipped default on missing rows), got %q/%q/v%d", mode, url, ver)
	}
}

// TestPutOutboundProxySuccessAndReadBack pins the write half: a PUT moving
// the pair to custom commits (new version handed back) and the
// authoritative GET reads the same mode and URL.
func TestPutOutboundProxySuccessAndReadBack(t *testing.T) {
	r, _ := setupOutboundProxyRouter(t)
	w := putOutboundProxyRaw(t, r, `{"mode":"custom","url":"socks5://127.0.0.1:1080","version":1}`)
	if w.Code != 200 {
		t.Fatalf("PUT custom: status = %d, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Mode    string `json:"mode"`
			URL     string `json:"url"`
			Version int64  `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Data.Mode != "custom" || resp.Data.URL != "socks5://127.0.0.1:1080" || resp.Data.Version != 2 {
		t.Fatalf("want custom/socks5://127.0.0.1:1080/v2, got %q/%q/v%d", resp.Data.Mode, resp.Data.URL, resp.Data.Version)
	}
	if mode, url, ver := getOutboundProxySetting(t, r); mode != "custom" || url != "socks5://127.0.0.1:1080" || ver != 2 {
		t.Fatalf("read after PUT: want custom/socks5://127.0.0.1:1080/v2, got %q/%q/v%d", mode, url, ver)
	}
}

// TestPutOutboundProxyMissingFields400 and the zero-version form pin the
// request shape: mode, url and version are all required (pointers — a
// partial body must not silently flip the mode or clear the URL), and a
// version of 0 (what a missing-row read reports) is refused so the pair
// can only ever be created by the seeding migration.
func TestPutOutboundProxyMissingFields400(t *testing.T) {
	r, _ := setupOutboundProxyRouter(t)
	for _, body := range []string{`{}`, `{"mode":"off"}`, `{"url":""}`, `{"version":1}`, `{"mode":"off","url":""}`} {
		w := putOutboundProxyRaw(t, r, body)
		if w.Code != 400 {
			t.Fatalf("body %s: status = %d, want 400", body, w.Code)
		}
		var resp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body %s: unmarshal: %v", body, err)
		}
		if !strings.Contains(resp.Message, "mode, url and version (>=1) are all required") {
			t.Fatalf("body %s: message = %q, want the required-fields message", body, resp.Message)
		}
	}
}

func TestPutOutboundProxyZeroVersion400(t *testing.T) {
	r, _ := setupOutboundProxyRouter(t)
	w := putOutboundProxyRaw(t, r, `{"mode":"off","url":"","version":0}`)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestPutOutboundProxyInvalidValuesRejectedEmits11026 pins the validation
// half of the contract: an unknown mode, custom without a URL, an
// unstorable URL, and a bad URL parked behind off all answer 400 carrying
// the setting's own business code (11026) and leave the stored pair
// untouched — a refused value never partially commits.
func TestPutOutboundProxyInvalidValuesRejectedEmits11026(t *testing.T) {
	r, _ := setupOutboundProxyRouter(t)
	for _, body := range []string{
		`{"mode":"sometimes","url":"","version":1}`,                        // unknown mode
		`{"mode":"custom","url":"","version":1}`,                           // custom requires a URL
		`{"mode":"custom","url":"ftp://proxy.example.com:21","version":1}`, // scheme outside the supported four
		`{"mode":"off","url":"not a storable url","version":1}`,            // parked URL must still be storable
	} {
		w := putOutboundProxyRaw(t, r, body)
		if w.Code != 400 {
			t.Fatalf("body %s: status = %d, want 400, body: %s", body, w.Code, w.Body.String())
		}
		var resp struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body %s: unmarshal: %v", body, err)
		}
		if resp.Code != 11026 {
			t.Fatalf("body %s: errcode = %d, want 11026 (OutboundProxyURLInvalid)", body, resp.Code)
		}
		if mode, url, ver := getOutboundProxySetting(t, r); mode != "off" || url != "" || ver != 1 {
			t.Fatalf("body %s: setting changed to %q/%q/v%d, want off/''/v1 unchanged", body, mode, url, ver)
		}
	}
}

// TestPutOutboundProxyConflictEmits11025 verifies the 409 response carries
// errcode 11025 (OutboundProxyConflict), distinct from the other settings'
// conflict codes so the frontend can route the retry to the right control.
// The 409 is set explicitly by the handler — httpStatusForCode's range
// mapping would answer 400 for a 11xxx code — so this test pins both the
// status and the code.
func TestPutOutboundProxyConflictEmits11025(t *testing.T) {
	r, _ := setupOutboundProxyRouter(t)
	w := putOutboundProxyRaw(t, r, `{"mode":"custom","url":"socks5://127.0.0.1:1080","version":99}`)
	if w.Code != 409 {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	var resp struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Code != 11025 {
		t.Fatalf("errcode = %d, want 11025 (OutboundProxyConflict)", resp.Code)
	}
}
