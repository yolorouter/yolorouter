package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/service"
	"github.com/yolorouter/yolorouter-ce/internal/testutil"
	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
)

func newModelTestRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	return newModelTestRouterWithClient(t, &alwaysSuccessClient{})
}

func newModelTestRouterWithClient(t *testing.T, client service.ProviderClient) (*gin.Engine, *gorm.DB) {
	t.Helper()
	if err := RegisterValidators(); err != nil {
		t.Fatalf("RegisterValidators failed: %v", err)
	}
	db := testutil.NewSQLiteDB(t)
	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(i)
	}
	svc := service.NewModelService(db, masterKey, client)

	r := gin.New()
	admin := r.Group("/api/admin")
	admin.GET("/models", GetModels(svc))
	admin.POST("/models", PostModel(svc))
	admin.GET("/models/:id", GetModel(svc))
	admin.PATCH("/models/:id", PatchModel(svc))
	admin.PATCH("/models/:id/status", PatchModelStatus(svc))
	admin.POST("/models/:id/candidates/test-mapping", PostModelCandidateTestMapping(svc))
	admin.POST("/models/:id/candidates", PostModelCandidate(svc))
	admin.PATCH("/models/:id/candidates/:candidateId", PatchModelCandidate(svc))
	admin.PATCH("/models/:id/candidates/:candidateId/order", PatchModelCandidateOrder(svc))
	admin.PATCH("/models/:id/candidates/:candidateId/status", PatchModelCandidateStatus(svc))
	admin.POST("/models/:id/candidates/:candidateId/test", PostModelCandidateTest(svc))
	admin.DELETE("/models/:id/candidates/:candidateId", DeleteModelCandidate(svc))
	return r, db
}

type modelResponse struct {
	ID               uint   `json:"id"`
	Name             string `json:"name"`
	ManagementStatus int    `json:"management_status"`
	RunningStatus    string `json:"running_status"`
}

type candidateResponse struct {
	ID               uint `json:"id"`
	ManagementStatus int  `json:"management_status"`
}

func createModelForTest(t *testing.T, r *gin.Engine, name string) uint {
	t.Helper()
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/models", map[string]interface{}{"name": name}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("createModelForTest: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var m modelResponse
	if err := json.Unmarshal(env.Data, &m); err != nil {
		t.Fatalf("unmarshal model response: %v", err)
	}
	return m.ID
}

func TestPostModelCreatesAndReturns200(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/models", map[string]interface{}{"name": "smart"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.Success {
		t.Fatalf("expected success code, got %d", env.Code)
	}
}

func TestPostModelRejectsDuplicateName(t *testing.T) {
	r, _ := newModelTestRouter(t)
	createModelForTest(t, r, "smart")
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/models", map[string]interface{}{"name": "smart"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ModelNameTaken {
		t.Fatalf("expected code %d, got %d", errcode.ModelNameTaken, env.Code)
	}
}

func TestPostModelRejectsMissingName(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/models", map[string]interface{}{}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestGetModelsReturnsCreatedModel(t *testing.T) {
	r, _ := newModelTestRouter(t)
	createModelForTest(t, r, "smart")
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/models", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body struct {
		List []modelResponse `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	if len(body.List) != 1 || body.List[0].Name != "smart" {
		t.Fatalf("expected 1 model named 'smart', got %+v", body.List)
	}
}

func TestGetModelReturns200ForExistingModel(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, env := doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/models/%d", id), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var m modelResponse
	if err := json.Unmarshal(env.Data, &m); err != nil {
		t.Fatalf("unmarshal model response: %v", err)
	}
	if m.Name != "smart" {
		t.Fatalf("expected name 'smart', got %q", m.Name)
	}
}

func TestGetModelReturns400WhenNotFound(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/models/999999", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ModelNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ModelNotFound, env.Code)
	}
}

func TestGetModelReturns400ForBadID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/models/abc", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelRenamesModel(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", id), map[string]interface{}{"name": "smart-v2"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var m modelResponse
	if err := json.Unmarshal(env.Data, &m); err != nil {
		t.Fatalf("unmarshal model response: %v", err)
	}
	if m.Name != "smart-v2" {
		t.Fatalf("expected name 'smart-v2', got %q", m.Name)
	}
}

func TestPatchModelStatusDisablesModel(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d/status", id), map[string]interface{}{"enabled": false}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	_, env := doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/models/%d", id), nil, nil)
	var m modelResponse
	if err := json.Unmarshal(env.Data, &m); err != nil {
		t.Fatalf("unmarshal model response: %v", err)
	}
	if m.ManagementStatus != 2 {
		t.Fatalf("expected management_status=2 (disabled), got %d", m.ManagementStatus)
	}
}

func TestPatchModelStatusReturns400WhenNotFound(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, env := doJSON(t, r, http.MethodPatch, "/api/admin/models/999999/status", map[string]interface{}{"enabled": false}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ModelNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ModelNotFound, env.Code)
	}
}

// createProviderAndKeyForModelTest creates a provider requesting an
// enabled key (unlike createProviderForTest, which omits management_status
// and so leaves the key disabled/untested) — candidate tests need a real
// "available key" (enabled + verified) to run their own basic-text test
// against.
func createProviderAndKeyForModelTest(t *testing.T, r *gin.Engine) uint {
	t.Helper()
	body := map[string]interface{}{
		"name": "provider-a", "base_url": "https://api.example.com/v1",
		"key_label": "primary", "key_plaintext": "sk-abcdefghijklmnopqrstuvwxyz1234", "test_model": "gpt-4o-mini",
		"management_status": 1,
	}
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/providers", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: create provider failed: %d, body: %s", w.Code, w.Body.String())
	}
	var view struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("unmarshal provider view: %v", err)
	}
	return view.ID
}

func TestPostModelCandidateTestMappingReturnsOutcome(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderAndKeyForModelTest(t, providerRouter)

	svc := service.NewModelService(db, testHandlerMasterKey(), &alwaysSuccessClient{})
	r := gin.New()
	admin := r.Group("/api/admin")
	admin.POST("/models", PostModel(svc))
	admin.POST("/models/:id/candidates/test-mapping", PostModelCandidateTestMapping(svc))
	id := createModelForTest(t, r, "smart")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates/test-mapping", id), map[string]interface{}{
		"provider_id": providerID, "provider_model_name": "gpt-4o", "test_type": "basic",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body struct {
		Outcome    int   `json:"outcome"`
		DurationMs int64 `json:"duration_ms"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal test-mapping response: %v", err)
	}
	if body.Outcome != 0 {
		t.Fatalf("expected outcome=0 (success), got %d", body.Outcome)
	}
}

func TestPostModelCandidateTestMappingReturns400ForBadRequest(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates/test-mapping", id), map[string]interface{}{
		"test_type": "bogus",
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

// newModelTestRouterSharingProviderDB builds a model-endpoints-only router
// against an existing DB that already has provider fixtures — used so
// candidate tests can create a real provider via the provider handlers and
// then exercise candidates against it via the model handlers, both against
// the same database.
func newModelTestRouterSharingProviderDB(t *testing.T, db *gorm.DB, client service.ProviderClient) *gin.Engine {
	t.Helper()
	svc := service.NewModelService(db, testHandlerMasterKey(), client)
	r := gin.New()
	admin := r.Group("/api/admin")
	admin.POST("/models", PostModel(svc))
	admin.GET("/models/:id", GetModel(svc))
	admin.POST("/models/:id/candidates/test-mapping", PostModelCandidateTestMapping(svc))
	admin.POST("/models/:id/candidates", PostModelCandidate(svc))
	admin.PATCH("/models/:id/candidates/:candidateId", PatchModelCandidate(svc))
	admin.PATCH("/models/:id/candidates/:candidateId/order", PatchModelCandidateOrder(svc))
	admin.PATCH("/models/:id/candidates/:candidateId/status", PatchModelCandidateStatus(svc))
	admin.POST("/models/:id/candidates/:candidateId/test", PostModelCandidateTest(svc))
	admin.DELETE("/models/:id/candidates/:candidateId", DeleteModelCandidate(svc))
	return r
}

func testHandlerMasterKey() []byte {
	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(i)
	}
	return masterKey
}

func TestPostModelCandidateCreatesCandidate(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderAndKeyForModelTest(t, providerRouter)
	r := newModelTestRouterSharingProviderDB(t, db, &alwaysSuccessClient{})
	id := createModelForTest(t, r, "smart")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates", id), map[string]interface{}{
		"provider_id": providerID, "provider_model_name": "gpt-4o", "input_price": 1, "output_price": 2,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var c candidateResponse
	if err := json.Unmarshal(env.Data, &c); err != nil {
		t.Fatalf("unmarshal candidate response: %v", err)
	}
	if c.ID == 0 {
		t.Fatalf("expected a populated candidate ID")
	}
}

func TestPostModelCandidateReturns400ForDuplicateProvider(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderAndKeyForModelTest(t, providerRouter)
	r := newModelTestRouterSharingProviderDB(t, db, &alwaysSuccessClient{})
	id := createModelForTest(t, r, "smart")

	body := map[string]interface{}{"provider_id": providerID, "provider_model_name": "gpt-4o", "input_price": 1, "output_price": 2}
	if w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates", id), body, nil); w.Code != http.StatusOK {
		t.Fatalf("first candidate create failed: %d", w.Code)
	}
	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates", id), body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ModelCandidateProviderTaken {
		t.Fatalf("expected code %d, got %d", errcode.ModelCandidateProviderTaken, env.Code)
	}
}

func TestPatchModelCandidateUpdatesFields(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderAndKeyForModelTest(t, providerRouter)
	r := newModelTestRouterSharingProviderDB(t, db, &alwaysSuccessClient{})
	id := createModelForTest(t, r, "smart")
	_, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates", id), map[string]interface{}{
		"provider_id": providerID, "provider_model_name": "gpt-4o", "input_price": 1, "output_price": 2,
	}, nil)
	var c candidateResponse
	if err := json.Unmarshal(env.Data, &c); err != nil {
		t.Fatalf("unmarshal candidate response: %v", err)
	}

	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d/candidates/%d", id, c.ID), map[string]interface{}{
		"provider_model_name": "gpt-4o-2024", "input_price": 1.5, "output_price": 3,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelCandidateStatusReturns400WhenUnverified(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderAndKeyForModelTest(t, providerRouter)
	r := newModelTestRouterSharingProviderDB(t, db, &alwaysSuccessClient{})
	id := createModelForTest(t, r, "smart")
	_, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates", id), map[string]interface{}{
		"provider_id": providerID, "provider_model_name": "gpt-4o", "input_price": 1, "output_price": 2,
	}, nil)
	var c candidateResponse
	if err := json.Unmarshal(env.Data, &c); err != nil {
		t.Fatalf("unmarshal candidate response: %v", err)
	}

	w, env2 := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d/candidates/%d/status", id, c.ID), map[string]interface{}{"enabled": true}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env2.Code != errcode.ModelCandidateNotVerified {
		t.Fatalf("expected code %d, got %d", errcode.ModelCandidateNotVerified, env2.Code)
	}
}

func TestPostModelCandidateTestRunsBasicTest(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderAndKeyForModelTest(t, providerRouter)
	r := newModelTestRouterSharingProviderDB(t, db, &alwaysSuccessClient{})
	id := createModelForTest(t, r, "smart")
	_, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates", id), map[string]interface{}{
		"provider_id": providerID, "provider_model_name": "gpt-4o", "input_price": 1, "output_price": 2,
	}, nil)
	var c candidateResponse
	if err := json.Unmarshal(env.Data, &c); err != nil {
		t.Fatalf("unmarshal candidate response: %v", err)
	}

	w, env2 := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates/%d/test", id, c.ID), map[string]interface{}{"test_type": "basic"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var updated struct {
		VerificationStatus int `json:"verification_status"`
	}
	if err := json.Unmarshal(env2.Data, &updated); err != nil {
		t.Fatalf("unmarshal test response: %v", err)
	}
	if updated.VerificationStatus != 1 {
		t.Fatalf("expected verification_status=1 (passed), got %d", updated.VerificationStatus)
	}
}

func TestPatchModelCandidateOrderReturns400ForUnknownCandidate(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d/candidates/999999/order", id), map[string]interface{}{"direction": "up"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ModelCandidateNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ModelCandidateNotFound, env.Code)
	}
}

func TestDeleteModelCandidateSucceeds(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderAndKeyForModelTest(t, providerRouter)
	r := newModelTestRouterSharingProviderDB(t, db, &alwaysSuccessClient{})
	id := createModelForTest(t, r, "smart")
	_, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates", id), map[string]interface{}{
		"provider_id": providerID, "provider_model_name": "gpt-4o", "input_price": 1, "output_price": 2,
	}, nil)
	var c candidateResponse
	if err := json.Unmarshal(env.Data, &c); err != nil {
		t.Fatalf("unmarshal candidate response: %v", err)
	}

	w, _ := doJSON(t, r, http.MethodDelete, fmt.Sprintf("/api/admin/models/%d/candidates/%d", id, c.ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestGetModelsReturns500WhenServiceErrors(t *testing.T) {
	r, db := newModelTestRouter(t)
	testutil.CloseDB(t, db)
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/models", nil, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.InternalError {
		t.Fatalf("expected code %d, got %d", errcode.InternalError, env.Code)
	}
}

func TestPatchModelReturns400ForBadID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodPatch, "/api/admin/models/abc", map[string]interface{}{"name": "x"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelReturns400ForBadBody(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", id), map[string]interface{}{}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelStatusReturns400ForBadID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodPatch, "/api/admin/models/abc/status", map[string]interface{}{"enabled": true}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPostModelCandidateTestMappingReturns400ForBadID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/models/abc/candidates/test-mapping", map[string]interface{}{
		"provider_id": 1, "provider_model_name": "gpt-4o", "test_type": "basic",
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPostModelCandidateTestMappingReturns400ForUntestableProvider(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID, _ := createProviderForTest(t, providerRouter, "provider-a")
	r := newModelTestRouterSharingProviderDB(t, db, &alwaysSuccessClient{})
	id := createModelForTest(t, r, "smart")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates/test-mapping", id), map[string]interface{}{
		"provider_id": providerID, "provider_model_name": "gpt-4o", "test_type": "basic",
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ProviderNoTestableModel {
		t.Fatalf("expected code %d, got %d", errcode.ProviderNoTestableModel, env.Code)
	}
}

func TestPostModelCandidateReturns400ForBadID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/models/abc/candidates", map[string]interface{}{
		"provider_id": 1, "provider_model_name": "gpt-4o",
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPostModelCandidateReturns400ForBadBody(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates", id), map[string]interface{}{}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelCandidateReturns400ForBadModelID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodPatch, "/api/admin/models/abc/candidates/1", map[string]interface{}{"provider_model_name": "x"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelCandidateReturns400ForBadCandidateID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d/candidates/abc", id), map[string]interface{}{"provider_model_name": "x"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelCandidateReturns400ForBadBody(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d/candidates/1", id), map[string]interface{}{}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelCandidateOrderReturns400ForBadModelID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodPatch, "/api/admin/models/abc/candidates/1/order", map[string]interface{}{"direction": "up"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelCandidateOrderReturns400ForBadCandidateID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d/candidates/abc/order", id), map[string]interface{}{"direction": "up"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelCandidateOrderReturns400ForBadBody(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d/candidates/1/order", id), map[string]interface{}{"direction": "sideways"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelCandidateStatusReturns400ForBadModelID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodPatch, "/api/admin/models/abc/candidates/1/status", map[string]interface{}{"enabled": true}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelCandidateStatusReturns400ForBadCandidateID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d/candidates/abc/status", id), map[string]interface{}{"enabled": true}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPatchModelCandidateStatusReturns400ForBadBody(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d/candidates/1/status", id), []byte("not json"), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPostModelCandidateTestReturns400ForBadModelID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/models/abc/candidates/1/test", map[string]interface{}{"test_type": "basic"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPostModelCandidateTestReturns400ForBadCandidateID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates/abc/test", id), map[string]interface{}{"test_type": "basic"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestPostModelCandidateTestReturns400ForBadBody(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates/1/test", id), map[string]interface{}{"test_type": "bogus"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestDeleteModelCandidateReturns400ForBadModelID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodDelete, "/api/admin/models/abc/candidates/1", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestDeleteModelCandidateReturns400ForBadCandidateID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodDelete, fmt.Sprintf("/api/admin/models/%d/candidates/abc", id), nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestDeleteModelCandidateReturns400WhenNotFound(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, env := doJSON(t, r, http.MethodDelete, fmt.Sprintf("/api/admin/models/%d/candidates/999999", id), nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ModelCandidateNotFound {
		t.Fatalf("expected code %d, got %d", errcode.ModelCandidateNotFound, env.Code)
	}
}
