package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// scheduling_mode deliberately carries no binding tag: the service layer is
// its single validator, so every invalid value — on create, batch create,
// and update alike — answers with the field's own error code instead of the
// generic bad-request envelope. TestModelSchedulingModeRejectsUnknownValue
// pins that code on each path.

func newModelTestRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	return newModelTestRouterWithClient(t, &alwaysSuccessClient{})
}

func newModelTestRouterWithQueue(t *testing.T) (*gin.Engine, *gorm.DB, *modeladmin.ProbeQueue) {
	t.Helper()
	r, db, queue := newModelTestRouterFull(t, &alwaysSuccessClient{})
	return r, db, queue
}

func newModelTestRouterWithClient(t *testing.T, client providerclient.ProviderClient) (*gin.Engine, *gorm.DB) {
	t.Helper()
	r, db, _ := newModelTestRouterFull(t, client)
	return r, db
}

func newModelTestRouterFull(t *testing.T, client providerclient.ProviderClient) (*gin.Engine, *gorm.DB, *modeladmin.ProbeQueue) {
	t.Helper()
	if err := RegisterValidators(); err != nil {
		t.Fatalf("RegisterValidators failed: %v", err)
	}
	db := testutil.NewSQLiteDB(t)
	svc := modeladmin.NewModelService(db, crypto.NewSecretBox(testutil.ProviderMasterKey()), client)

	r := gin.New()
	admin := r.Group("/api/admin")
	admin.GET("/models", GetModels(svc))
	admin.POST("/models", PostModel(svc))
	admin.POST("/models/batch", PostModelsBatch(svc))
	admin.GET("/models/:id", GetModel(svc))
	admin.PATCH("/models/:id", PatchModel(svc))
	admin.PATCH("/models/:id/status", PatchModelStatus(svc))
	admin.POST("/models/:id/candidates", PostModelCandidate(svc))
	admin.POST("/models/:id/candidates/test-and-create", PostModelCandidateTestAndCreate(svc))
	admin.PATCH("/models/:id/candidates/:candidateId", PatchModelCandidate(svc))
	admin.PATCH("/models/:id/candidates/:candidateId/order", PatchModelCandidateOrder(svc))
	admin.PATCH("/models/:id/candidates/:candidateId/status", PatchModelCandidateStatus(svc))
	admin.POST("/models/:id/candidates/:candidateId/test", PostModelCandidateTest(svc))
	admin.GET("/models/candidates/suggest-price", GetCandidateSuggestPrice(svc))
	admin.DELETE("/models/:id/candidates/:candidateId", DeleteModelCandidate(svc))
	// Unstarted on purpose: handler tests assert what got queued, not the
	// asynchronous probing itself (that lives in the modeladmin suite).
	queue := modeladmin.NewProbeQueue(svc, modeladmin.DefaultProbeWorkers)
	admin.POST("/providers/:id/models/import", PostProviderModelsImport(svc, queue))
	admin.POST("/providers/:id/models/suggest-prices", PostProviderSuggestPrices(svc))
	admin.GET("/providers/:id/candidates", GetProviderCandidates(svc, queue))
	return r, db, queue
}

type modelResponse struct {
	ID               uint   `json:"id"`
	Name             string `json:"name"`
	ManagementStatus int    `json:"management_status"`
	RunningStatus    string `json:"running_status"`
}

type candidateResponse struct {
	ID                 uint `json:"id"`
	ManagementStatus   int  `json:"management_status"`
	VerificationStatus int  `json:"verification_status"`
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

func TestPostModelsBatchCreatesAndSkips(t *testing.T) {
	r, _ := newModelTestRouter(t)
	createModelForTest(t, r, "gpt-5.6")

	w, env := doJSON(t, r, http.MethodPost, "/api/admin/models/batch", map[string]interface{}{
		"names": []string{"gpt-5.6", "claude-sonnet-5", "bad name!"},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body struct {
		Created []modelResponse `json:"created"`
		Skipped []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal batch response: %v", err)
	}
	if len(body.Created) != 1 || body.Created[0].Name != "claude-sonnet-5" {
		t.Fatalf("expected 1 created 'claude-sonnet-5', got %+v", body.Created)
	}
	if len(body.Skipped) != 2 {
		t.Fatalf("expected 2 skipped, got %+v", body.Skipped)
	}
}

func TestPostModelsBatchRejectsEmptyNames(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/models/batch", map[string]interface{}{
		"names": []string{},
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty names, got %d", w.Code)
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

func newModelTestRouterSharingProviderDB(t *testing.T, db *gorm.DB, client providerclient.ProviderClient) *gin.Engine {
	t.Helper()
	svc := modeladmin.NewModelService(db, crypto.NewSecretBox(testHandlerMasterKey()), client)
	r := gin.New()
	admin := r.Group("/api/admin")
	admin.POST("/models", PostModel(svc))
	admin.GET("/models/:id", GetModel(svc))
	admin.POST("/models/:id/candidates", PostModelCandidate(svc))
	admin.POST("/models/:id/candidates/test-and-create", PostModelCandidateTestAndCreate(svc))
	admin.PATCH("/models/:id/candidates/:candidateId", PatchModelCandidate(svc))
	admin.PATCH("/models/:id/candidates/:candidateId/order", PatchModelCandidateOrder(svc))
	admin.PATCH("/models/:id/candidates/:candidateId/status", PatchModelCandidateStatus(svc))
	admin.POST("/models/:id/candidates/:candidateId/test", PostModelCandidateTest(svc))
	admin.GET("/models/candidates/suggest-price", GetCandidateSuggestPrice(svc))
	admin.DELETE("/models/:id/candidates/:candidateId", DeleteModelCandidate(svc))
	return r
}

func testHandlerMasterKey() []byte {
	return testutil.ProviderMasterKey()
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

// The endpoint the admin UI saves through: it probes, reports all three
// verdicts, and stores the candidate in one round trip.
func TestPostModelCandidateTestAndCreateReportsProbesAndCreates(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderAndKeyForModelTest(t, providerRouter)
	r := newModelTestRouterSharingProviderDB(t, db, &alwaysSuccessClient{})
	id := createModelForTest(t, r, "smart")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates/test-and-create", id), map[string]interface{}{
		"provider_id": providerID, "provider_model_name": "gpt-4o", "input_price": 1, "output_price": 2,
		"management_status": 1,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body struct {
		Created bool `json:"created"`
		Report  struct {
			Basic           struct{ Ran, Supported bool } `json:"basic"`
			Streaming       struct{ Ran, Supported bool } `json:"streaming"`
			FunctionCalling struct{ Ran, Supported bool } `json:"function_calling"`
		} `json:"report"`
		Candidate *candidateResponse `json:"candidate"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal test-and-create response: %v", err)
	}
	if !body.Created || body.Candidate == nil {
		t.Fatalf("expected the candidate to be created, got %+v", body)
	}
	if body.Candidate.ManagementStatus != 1 {
		t.Fatalf("expected the candidate to be enabled, got %d", body.Candidate.ManagementStatus)
	}
	for name, probe := range map[string]struct{ Ran, Supported bool }{
		"basic": body.Report.Basic, "streaming": body.Report.Streaming, "function_calling": body.Report.FunctionCalling,
	} {
		if !probe.Ran || !probe.Supported {
			t.Fatalf("expected the %s probe to run and pass, got %+v", name, probe)
		}
	}
}

// The retest response serves two client generations at once during a rolling
// upgrade: browser tabs still running the previous frontend read the candidate
// fields at the TOP LEVEL of data, while the current frontend reads
// data.candidate plus data.applied. Both shapes must be present — dropping the
// top-level fields makes every old tab misreport a successful retest.
func TestPostModelCandidateTestServesBothWireShapes(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderAndKeyForModelTest(t, providerRouter)
	r := newModelTestRouterSharingProviderDB(t, db, &alwaysSuccessClient{})
	id := createModelForTest(t, r, "smart")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates/test-and-create", id), map[string]interface{}{
		"provider_id": providerID, "provider_model_name": "gpt-4o", "input_price": 1, "output_price": 2,
		"management_status": 1,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("seed candidate: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var created struct {
		Candidate *candidateResponse `json:"candidate"`
	}
	if err := json.Unmarshal(env.Data, &created); err != nil || created.Candidate == nil {
		t.Fatalf("unmarshal created candidate: %v, %s", err, env.Data)
	}

	w, env = doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/admin/models/%d/candidates/%d/test", id, created.Candidate.ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body struct {
		// Old-client shape: the candidate itself at the top level.
		VerificationStatus *int `json:"verification_status"`
		ManagementStatus   *int `json:"management_status"`
		// Current shape.
		Candidate *candidateResponse `json:"candidate"`
		Applied   *bool              `json:"applied"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal retest response: %v, %s", err, env.Data)
	}
	if body.Candidate == nil || body.Applied == nil {
		t.Fatalf("expected the current shape (candidate + applied), got %s", env.Data)
	}
	if body.VerificationStatus == nil || body.ManagementStatus == nil {
		t.Fatalf("expected the old-client shape (top-level candidate fields), got %s", env.Data)
	}
	if *body.VerificationStatus != body.Candidate.VerificationStatus {
		t.Fatalf("expected both shapes to describe the same row, got top-level=%d nested=%d",
			*body.VerificationStatus, body.Candidate.VerificationStatus)
	}
}

func TestPostModelCandidateTestAndCreateReturns400ForBadModelID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/models/abc/candidates/test-and-create", map[string]interface{}{
		"provider_id": 1, "provider_model_name": "gpt-4o",
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

// Retest takes no request body at all — one run covers the basic mapping and
// both capabilities, so there is no test type for a caller to choose.
func TestPostModelCandidateTestRetestsWithoutABody(t *testing.T) {
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

	w, env2 := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates/%d/test", id, c.ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	// The response distinguishes the row from whether THIS retest's verdict was
	// the one recorded (a concurrent probe can win the commit race, in which
	// case the row reflects the competitor's result) — the client needs the
	// flag to avoid announcing another probe's outcome as this click's.
	var result struct {
		Applied bool `json:"applied"`
		Updated struct {
			VerificationStatus      int   `json:"verification_status"`
			SupportsStreaming       *bool `json:"supports_streaming"`
			SupportsFunctionCalling *bool `json:"supports_function_calling"`
		} `json:"candidate"`
	}
	if err := json.Unmarshal(env2.Data, &result); err != nil {
		t.Fatalf("unmarshal test response: %v", err)
	}
	if !result.Applied {
		t.Fatal("expected applied=true for an uncontended retest")
	}
	updated := result.Updated
	if updated.VerificationStatus != 1 {
		t.Fatalf("expected verification_status=1 (passed), got %d", updated.VerificationStatus)
	}
	// The capability probes ran as part of the same retest, which is what stops
	// the capability columns from sitting empty until someone probes them by hand.
	if updated.SupportsStreaming == nil || !*updated.SupportsStreaming {
		t.Fatalf("expected supports_streaming=true, got %v", updated.SupportsStreaming)
	}
	if updated.SupportsFunctionCalling == nil || !*updated.SupportsFunctionCalling {
		t.Fatalf("expected supports_function_calling=true, got %v", updated.SupportsFunctionCalling)
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

func TestPostModelCandidateTestReturnsErrorForUnknownCandidate(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "smart")
	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates/1/test", id), nil, nil)
	if w.Code == http.StatusOK {
		t.Fatalf("expected an error for a candidate that does not exist, got 200: %s", w.Body.String())
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

// createProviderOnHostForModelTest is createProviderAndKeyForModelTest with the
// base_url spelled out, so a test can put the provider on a host the seed price
// catalog does or does not carry.
func createProviderOnHostForModelTest(t *testing.T, r *gin.Engine, name, baseURL string) uint {
	t.Helper()
	body := map[string]interface{}{
		"name": name, "base_url": baseURL,
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

type suggestPriceResponse struct {
	InputPrice      float64  `json:"input_price"`
	OutputPrice     float64  `json:"output_price"`
	CacheWritePrice *float64 `json:"cache_write_price"`
	CacheReadPrice  *float64 `json:"cache_read_price"`
	Source          string   `json:"source"`
}

func getSuggestPrice(t *testing.T, r *gin.Engine, query string) (*httptest.ResponseRecorder, envelope) {
	t.Helper()
	return doJSON(t, r, http.MethodGet, "/api/admin/models/candidates/suggest-price?"+query, nil, nil)
}

func TestGetCandidateSuggestPriceReturnsSeedPrice(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderOnHostForModelTest(t, providerRouter, "deepseek", "https://api.deepseek.com/v1")
	r := newModelTestRouterSharingProviderDB(t, db, &alwaysSuccessClient{})

	w, env := getSuggestPrice(t, r, fmt.Sprintf("provider_id=%d&provider_model_name=deepseek-v4-flash", providerID))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var got suggestPriceResponse
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatalf("unmarshal suggestion: %v", err)
	}
	if got.Source != "seed" {
		t.Fatalf("expected the built-in catalog to answer, got source=%q body=%s", got.Source, w.Body.String())
	}
	if got.InputPrice <= 0 || got.OutputPrice <= 0 {
		t.Fatalf("expected non-zero catalog prices, got %+v", got)
	}
}

func TestGetCandidateSuggestPriceReturnsEmptySourceForUnknownPair(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderOnHostForModelTest(t, providerRouter, "self-hosted", "https://llm.internal.example/v1")
	r := newModelTestRouterSharingProviderDB(t, db, &alwaysSuccessClient{})

	w, env := getSuggestPrice(t, r, fmt.Sprintf("provider_id=%d&provider_model_name=some-local-model", providerID))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var got suggestPriceResponse
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatalf("unmarshal suggestion: %v", err)
	}
	if got.Source != "" || got.InputPrice != 0 {
		t.Fatalf("expected an empty suggestion, got %+v", got)
	}
}

func TestGetCandidateSuggestPriceRejectsBadQueryParams(t *testing.T) {
	r, _ := newModelTestRouter(t)
	cases := []struct {
		name, query, wantMessage string
	}{
		{"absent provider_id", "provider_model_name=gpt-4o", "provider_id is required"},
		{"blank provider_id", "provider_id=&provider_model_name=gpt-4o", "provider_id is required"},
		// A supplied-but-unusable value must not be reported as a missing one,
		// and every unusable form must name the same contract — telling a client
		// "non-negative" here would invite a retry with the zero also rejected.
		{"malformed provider_id", "provider_id=abc&provider_model_name=gpt-4o", "provider_id must be a positive integer"},
		{"negative provider_id", "provider_id=-1&provider_model_name=gpt-4o", "provider_id must be a positive integer"},
		{"zero provider_id", "provider_id=0&provider_model_name=gpt-4o", "provider_id must be a positive integer"},
		{"absent model name", "provider_id=1", "provider_model_name is required"},
		{"blank model name", "provider_id=1&provider_model_name=%20%20", "provider_model_name is required"},
	}
	for _, tc := range cases {
		w, env := getSuggestPrice(t, r, tc.query)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d, body: %s", tc.name, w.Code, w.Body.String())
			continue
		}
		if env.Message != tc.wantMessage {
			t.Errorf("%s: expected message %q, got %q", tc.name, tc.wantMessage, env.Message)
		}
	}
}

func TestGetCandidateSuggestPriceReturnsProviderNotFound(t *testing.T) {
	r, _ := newModelTestRouter(t)

	w, env := getSuggestPrice(t, r, "provider_id=999999&provider_model_name=gpt-4o")
	if env.Code != errcode.ProviderNotFound {
		t.Fatalf("expected ProviderNotFound, got code=%d body: %s", env.Code, w.Body.String())
	}
}

// TestPatchModelImageInputTriState pins the declaration lifecycle: "no"
// persists false, a PATCH without the field leaves the stored value alone,
// and "unknown" clears back to undeclared (NULL) — three distinct states,
// not two.
func TestPatchModelImageInputTriState(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "text-only")
	patch := func(body map[string]interface{}) modelImageInputResponse {
		t.Helper()
		w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", id), body, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("patch: expected 200, got %d, body: %s", w.Code, w.Body.String())
		}
		var m modelImageInputResponse
		if err := json.Unmarshal(env.Data, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return m
	}

	if m := patch(map[string]interface{}{"name": "text-only", "image_input": "no"}); m.SupportsImageInput == nil || *m.SupportsImageInput {
		t.Fatalf("after image_input=no: got %v, want false", m.SupportsImageInput)
	}
	// Field absent: the stored declaration must survive an unrelated rename.
	if m := patch(map[string]interface{}{"name": "text-only-v2"}); m.SupportsImageInput == nil || *m.SupportsImageInput {
		t.Fatalf("after rename without field: got %v, want false preserved", m.SupportsImageInput)
	}
	if m := patch(map[string]interface{}{"name": "text-only-v2", "image_input": "unknown"}); m.SupportsImageInput != nil {
		t.Fatalf("after image_input=unknown: got %v, want nil (undeclared)", *m.SupportsImageInput)
	}
}

type modelImageInputResponse struct {
	Name               string `json:"name"`
	SupportsImageInput *bool  `json:"supports_image_input"`
}

// TestPatchModelRejectedRenameDoesNotPersistImageInput pins the write order:
// a PATCH whose rename half is rejected (name already taken) must not have
// quietly persisted the image-input declaration first — the client sees an
// error and the database must still agree with the pre-edit view.
func TestPatchModelRejectedRenameDoesNotPersistImageInput(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "keep-me")
	createModelForTest(t, r, "taken")

	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", id),
		map[string]interface{}{"name": "taken", "image_input": "no"}, nil)
	if w.Code == http.StatusOK {
		t.Fatalf("expected the duplicate rename to be rejected, got 200: %s", w.Body.String())
	}

	_, env := doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/models/%d", id), nil, nil)
	var m modelImageInputResponse
	if err := json.Unmarshal(env.Data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.SupportsImageInput != nil {
		t.Fatalf("declaration persisted despite rejected PATCH: got %v, want nil", *m.SupportsImageInput)
	}
}

// TestPatchModelRenameFollowsVisionFallbackSetting pins the rename cascade:
// when the renamed model is the configured vision-fallback describe model,
// the setting follows the new public name (version bumped so cache refreshes
// and CAS writers see it); renaming any other model leaves it untouched.
func TestPatchModelRenameFollowsVisionFallbackSetting(t *testing.T) {
	r, db := newModelTestRouter(t)
	fallbackID := createModelForTest(t, r, "eyes-v1")
	otherID := createModelForTest(t, r, "unrelated")
	if err := db.Exec(`UPDATE system_settings SET value = 'eyes-v1' WHERE key = 'vision_fallback_model'`).Error; err != nil {
		t.Fatalf("seed setting: %v", err)
	}

	readSetting := func() (string, int64) {
		t.Helper()
		var row struct {
			Value   string
			Version int64
		}
		if err := db.Table("system_settings").Select("value, version").
			Where("key = 'vision_fallback_model'").Take(&row).Error; err != nil {
			t.Fatalf("read setting: %v", err)
		}
		return row.Value, row.Version
	}
	_, verBefore := readSetting()

	// Renaming an unrelated model must not touch the reference.
	w, _ := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", otherID), map[string]interface{}{"name": "unrelated-v2"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("unrelated rename: %d %s", w.Code, w.Body.String())
	}
	if v, ver := readSetting(); v != "eyes-v1" || ver != verBefore {
		t.Fatalf("unrelated rename touched setting: %q v%d", v, ver)
	}

	// Renaming the fallback model itself must carry the reference along.
	w, _ = doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", fallbackID), map[string]interface{}{"name": "eyes-v2"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("fallback rename: %d %s", w.Code, w.Body.String())
	}
	if v, ver := readSetting(); v != "eyes-v2" || ver != verBefore+1 {
		t.Fatalf("setting after rename = %q v%d, want eyes-v2 v%d", v, ver, verBefore+1)
	}
	// The cascade must leave the settings PAIR intact: the reader rejects a
	// version mismatch between the two rows as corruption, and the CAS save
	// needs both at one version — so the proof that the rename didn't brick
	// the setting is a successful read through the real reader, followed by
	// a successful CAS save at the version it returned.
	snap, snapVer, err := repository.GetVisionFallback(db)
	if err != nil {
		t.Fatalf("GetVisionFallback after rename: %v", err)
	}
	if snap.Model != "eyes-v2" {
		t.Fatalf("snapshot model = %q, want eyes-v2", snap.Model)
	}
	if _, _, err := repository.UpdateVisionFallback(db, snapVer, "eyes-v2", "still saveable"); err != nil {
		t.Fatalf("CAS save after rename must succeed, got: %v", err)
	}
}

type schedulingModelResponse struct {
	ID             uint   `json:"id"`
	Name           string `json:"name"`
	SchedulingMode string `json:"scheduling_mode"`
}

func TestModelSchedulingModeRoundTrip(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/models",
		map[string]interface{}{"name": "spread", "scheduling_mode": "balanced"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var created schedulingModelResponse
	if err := json.Unmarshal(env.Data, &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	if created.SchedulingMode != "balanced" {
		t.Fatalf("created scheduling_mode = %q, want balanced", created.SchedulingMode)
	}

	w, env = doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", created.ID),
		map[string]interface{}{"name": "spread", "scheduling_mode": "failover"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("patch expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var patched schedulingModelResponse
	if err := json.Unmarshal(env.Data, &patched); err != nil {
		t.Fatalf("unmarshal patch response: %v", err)
	}
	if patched.SchedulingMode != "failover" {
		t.Fatalf("patched scheduling_mode = %q, want failover", patched.SchedulingMode)
	}

	w, env = doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/models/%d", created.ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var detail schedulingModelResponse
	if err := json.Unmarshal(env.Data, &detail); err != nil {
		t.Fatalf("unmarshal detail response: %v", err)
	}
	if detail.SchedulingMode != "failover" {
		t.Fatalf("detail scheduling_mode = %q, want failover", detail.SchedulingMode)
	}
}

func TestModelSchedulingModeRejectsUnknownValue(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/models",
		map[string]interface{}{"name": "broken", "scheduling_mode": "round-robin"}, nil)
	if w.Code != http.StatusBadRequest || env.Code != errcode.ModelSchedulingModeInvalid {
		t.Fatalf("create with invalid mode = (%d, code %d), want (400, %d); body: %s", w.Code, env.Code, errcode.ModelSchedulingModeInvalid, w.Body.String())
	}
	w, env = doJSON(t, r, http.MethodPost, "/api/admin/models/batch",
		map[string]interface{}{"names": []string{"broken-b"}, "scheduling_mode": "round-robin"}, nil)
	if w.Code != http.StatusBadRequest || env.Code != errcode.ModelSchedulingModeInvalid {
		t.Fatalf("batch create with invalid mode = (%d, code %d), want (400, %d); body: %s", w.Code, env.Code, errcode.ModelSchedulingModeInvalid, w.Body.String())
	}
	id := createModelForTest(t, r, "still-broken")
	w, env = doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", id),
		map[string]interface{}{"name": "still-broken", "scheduling_mode": "weighted"}, nil)
	if w.Code != http.StatusBadRequest || env.Code != errcode.ModelSchedulingModeInvalid {
		t.Fatalf("patch with invalid mode = (%d, code %d), want (400, %d); body: %s", w.Code, env.Code, errcode.ModelSchedulingModeInvalid, w.Body.String())
	}
	// A present-but-empty mode must be rejected too — never read as "the
	// default", which would let a submission that carries nothing silently
	// reset the scheduler.
	w, env = doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", id),
		map[string]interface{}{"name": "still-broken", "scheduling_mode": ""}, nil)
	if w.Code != http.StatusBadRequest || env.Code != errcode.ModelSchedulingModeInvalid {
		t.Fatalf("patch with empty mode = (%d, code %d), want (400, %d); body: %s", w.Code, env.Code, errcode.ModelSchedulingModeInvalid, w.Body.String())
	}
}

// JSON null reads as "field absent" on every scheduling_mode path — the same
// convention image_input follows: create falls to the failover default,
// update keeps the current mode. Pinned so the null contract stays a
// decision rather than an accident.
func TestModelSchedulingModeNullMeansAbsent(t *testing.T) {
	r, _ := newModelTestRouter(t)
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/models",
		map[string]interface{}{"name": "nullish", "scheduling_mode": nil}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("create with null mode = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var created schedulingModelResponse
	if err := json.Unmarshal(env.Data, &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	if created.SchedulingMode != "failover" {
		t.Fatalf("create with null mode = %q, want the failover default", created.SchedulingMode)
	}

	// Switch to balanced, then PATCH with null: the mode must survive.
	w, _ = doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", created.ID),
		map[string]interface{}{"name": "nullish", "scheduling_mode": "balanced"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("switch to balanced = %d; body: %s", w.Code, w.Body.String())
	}
	w, env = doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", created.ID),
		map[string]interface{}{"name": "nullish", "scheduling_mode": nil}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("patch with null mode = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var patched schedulingModelResponse
	if err := json.Unmarshal(env.Data, &patched); err != nil {
		t.Fatalf("unmarshal patch response: %v", err)
	}
	if patched.SchedulingMode != "balanced" {
		t.Fatalf("null patch changed the mode to %q, want balanced preserved", patched.SchedulingMode)
	}

	w, env = doJSON(t, r, http.MethodPost, "/api/admin/models/batch",
		map[string]interface{}{"names": []string{"nullish-b"}, "scheduling_mode": nil}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("batch create with null mode = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var batch struct {
		Created []schedulingModelResponse `json:"created"`
	}
	if err := json.Unmarshal(env.Data, &batch); err != nil {
		t.Fatalf("unmarshal batch response: %v", err)
	}
	if len(batch.Created) != 1 || batch.Created[0].SchedulingMode != "failover" {
		t.Fatalf("batch create with null mode = %+v, want one model with the failover default", batch.Created)
	}
}

func TestPostModelsBatchCarriesOutputModalities(t *testing.T) {
	r, db := newModelTestRouter(t)
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/models/batch", map[string]interface{}{
		"names":             []string{"wan2.7-image"},
		"output_modalities": []string{"image"},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body struct {
		Created []modelResponse `json:"created"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal batch response: %v", err)
	}
	if len(body.Created) != 1 {
		t.Fatalf("expected 1 created model, got %+v", body.Created)
	}
	var m model.Model
	if err := db.First(&m, body.Created[0].ID).Error; err != nil {
		t.Fatalf("load created model: %v", err)
	}
	if m.OutputModalities != `["image"]` {
		t.Fatalf("expected the batch declaration stored, got %q", m.OutputModalities)
	}
}

func TestPostModelsBatchRejectsUnknownModality(t *testing.T) {
	r, db := newModelTestRouter(t)
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/models/batch", map[string]interface{}{
		"names":             []string{"never-created"},
		"output_modalities": []string{"audio"},
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ModelOutputModalityInvalid {
		t.Fatalf("expected code %d, got %d", errcode.ModelOutputModalityInvalid, env.Code)
	}
	var count int64
	if err := db.Model(&model.Model{}).Where("name = ?", "never-created").Count(&count).Error; err != nil {
		t.Fatalf("count models: %v", err)
	}
	if count != 0 {
		t.Fatal("a rejected declaration must not leave rows behind")
	}
}
