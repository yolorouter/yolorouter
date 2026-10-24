package handler

// The standalone probe endpoint's HTTP contract: POST
// /api/admin/providers/:id/models/probe answers with the shared candidate
// report plus its own vision verdict, keeps every upstream refusal —
// credential or capability — inside a 200 envelope instead of an HTTP error
// status, and stores nothing. The retest route's negative is pinned here too:
// its response shape gains no vision key, because stored candidates have no
// vision column to feed.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// probeOverrideClient passes everything through to alwaysSuccessClient
// except the two probes a test names: the basic chat verdict and the
// image-input verdict. The vision call count doubles as the "did the image
// request actually fire" observable.
type probeOverrideClient struct {
	alwaysSuccessClient

	basicOutcome  *providerclient.TestOutcome
	basicDetail   string
	visionOutcome *providerclient.TestOutcome
	visionDetail  string
	visionErr     error

	mu          sync.Mutex
	visionCalls int
}

func (c *probeOverrideClient) TestChatCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	if c.basicOutcome != nil {
		return providerclient.TestResult{Outcome: *c.basicOutcome, DurationMs: 5, Detail: c.basicDetail}, nil
	}
	return c.alwaysSuccessClient.TestChatCompletion(ctx, proto, baseURL, apiKey, model)
}

func (c *probeOverrideClient) TestImageInputChat(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	c.mu.Lock()
	c.visionCalls++
	c.mu.Unlock()
	if c.visionErr != nil {
		return providerclient.TestResult{}, c.visionErr
	}
	if c.visionOutcome != nil {
		return providerclient.TestResult{Outcome: *c.visionOutcome, DurationMs: 5, Detail: c.visionDetail}, nil
	}
	return providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}, nil
}

func (c *probeOverrideClient) visionCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.visionCalls
}

// probeEndpointFixture creates a provider with one verified key (through the
// provider router, whose create path verifies against alwaysSuccessClient)
// and a model router on the same database that the probe endpoint lives on.
func probeEndpointFixture(t *testing.T, client providerclient.ProviderClient) (r *gin.Engine, db *gorm.DB, providerID uint) {
	t.Helper()
	providerRouter, db := newProviderTestRouter(t)
	providerID = createProviderAndKeyForModelTest(t, providerRouter)
	return newModelTestRouterSharingProviderDB(t, db, client), db, providerID
}

func TestPostProviderModelProbeReportsBasicCapabilitiesAndVision(t *testing.T) {
	r, _, providerID := probeEndpointFixture(t, &probeOverrideClient{})

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/probe", providerID), map[string]interface{}{
		"provider_model_name": "gpt-5-vision",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body struct {
		Report struct {
			Basic           probeReportJSON `json:"basic"`
			Streaming       probeReportJSON `json:"streaming"`
			FunctionCalling probeReportJSON `json:"function_calling"`
		} `json:"report"`
		Vision probeReportJSON `json:"vision"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal probe response: %v, %s", err, env.Data)
	}
	for name, probe := range map[string]probeReportJSON{
		"basic": body.Report.Basic, "streaming": body.Report.Streaming,
		"function_calling": body.Report.FunctionCalling, "vision": body.Vision,
	} {
		if !probe.Ran || probe.Supported == nil || !*probe.Supported {
			t.Errorf("expected the %s probe to run and pass, got ran=%v supported=%v", name, probe.Ran, probe.Supported)
		}
	}
}

type probeReportJSON struct {
	Ran        bool   `json:"ran"`
	Supported  *bool  `json:"supported"`
	Outcome    *int   `json:"outcome"`
	DurationMs int64  `json:"duration_ms"`
	Detail     string `json:"detail"`
}

func TestPostProviderModelProbeRejectsMissingModelName(t *testing.T) {
	r, _, providerID := probeEndpointFixture(t, &probeOverrideClient{})

	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/probe", providerID), map[string]interface{}{}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing provider_model_name, got %d", w.Code)
	}
}

func TestPostProviderModelProbeRejectsUnknownModality(t *testing.T) {
	r, _, providerID := probeEndpointFixture(t, &probeOverrideClient{})

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/probe", providerID), map[string]interface{}{
		"provider_model_name": "m", "output_modalities": []string{"banana"},
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown modality id, got %d", w.Code)
	}
	if env.Code != errcode.ModelOutputModalityInvalid {
		t.Fatalf("expected code %d, got %d", errcode.ModelOutputModalityInvalid, env.Code)
	}
}

// A 400/415 from the upstream against a mapping whose basic probe passed is
// the decisive "no image input" answer: vision.ran stays true with
// supported=false, and the upstream's own words ride in the detail.
func TestPostProviderModelProbeVisionRejectionIsDecisive(t *testing.T) {
	rejected := providerclient.TestRequestRejected
	client := &probeOverrideClient{
		visionOutcome: &rejected,
		visionDetail:  "HTTP 400: image input is not supported for this model",
	}
	r, _, providerID := probeEndpointFixture(t, client)

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/probe", providerID), map[string]interface{}{
		"provider_model_name": "text-only-model",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body struct {
		Vision probeReportJSON `json:"vision"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal probe response: %v", err)
	}
	if !body.Vision.Ran {
		t.Errorf("vision.ran = false, want the rejection to count as an answer")
	}
	if body.Vision.Supported == nil || *body.Vision.Supported {
		t.Errorf("vision.supported = %v, want false", body.Vision.Supported)
	}
	if body.Vision.Detail != "HTTP 400: image input is not supported for this model" {
		t.Errorf("vision.detail = %q, want the upstream diagnostic", body.Vision.Detail)
	}
	if got := client.visionCallCount(); got != 1 {
		t.Errorf("image-input requests = %d, want exactly 1", got)
	}
}

// An upstream credential refusal rides inside the 200 envelope as a failed
// basic with the upstream's own message — the same presentation every other
// probe report failure gets — never as an HTTP error status.
func TestPostProviderModelProbeCredentialFailureRidesTheReport(t *testing.T) {
	authFailed := providerclient.TestAuthFailed
	r, _, providerID := probeEndpointFixture(t, &probeOverrideClient{
		basicOutcome: &authFailed,
		basicDetail:  "HTTP 401: invalid api key",
	})

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/probe", providerID), map[string]interface{}{
		"provider_model_name": "gpt-5",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("a credential refusal must stay a 200 report, got %d, body: %s", w.Code, w.Body.String())
	}
	var body struct {
		Report struct {
			Basic probeReportJSON `json:"basic"`
		} `json:"report"`
		Vision probeReportJSON `json:"vision"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal probe response: %v", err)
	}
	if !body.Report.Basic.Ran || body.Report.Basic.Supported == nil || *body.Report.Basic.Supported {
		t.Errorf("basic = %+v, want a failed basic", body.Report.Basic)
	}
	if body.Report.Basic.Detail != "HTTP 401: invalid api key" {
		t.Errorf("basic.detail = %q, want the upstream message verbatim", body.Report.Basic.Detail)
	}
	if body.Vision.Ran {
		t.Errorf("vision.ran = true, want the vision arm gated off behind the failed basic")
	}
}

// The endpoint's whole point: verdicts live in the response, never in the
// database.
func TestPostProviderModelProbeWritesNothing(t *testing.T) {
	r, db, providerID := probeEndpointFixture(t, &probeOverrideClient{})

	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/probe", providerID), map[string]interface{}{
		"provider_model_name": "brand-new-upstream-model",
		"output_modalities":   []string{"text", "image"},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var models int64
	if err := db.Model(&model.Model{}).Count(&models).Error; err != nil {
		t.Fatalf("count models: %v", err)
	}
	if models != 0 {
		t.Errorf("probe left %d model rows, want 0", models)
	}
	var candidates int64
	if err := db.Model(&model.ModelCandidate{}).Count(&candidates).Error; err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if candidates != 0 {
		t.Errorf("probe left %d candidate rows, want 0", candidates)
	}
}

// The retest route keeps its exact response shape: the candidate (both wire
// shapes) plus applied — and no vision key anywhere, because a stored
// candidate has no vision column for one to feed.
func TestPostModelCandidateTestResponseCarriesNoVisionKey(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderAndKeyForModelTest(t, providerRouter)
	client := &probeOverrideClient{}
	r := newModelTestRouterSharingProviderDB(t, db, client)
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
	if got := client.visionCallCount(); got != 0 {
		t.Errorf("test-and-create made %d image-input requests, want 0", got)
	}

	w, env = doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/admin/models/%d/candidates/%d/test", id, created.Candidate.ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var data map[string]interface{}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal retest response: %v", err)
	}
	if _, found := topLevelKey(data, "vision"); found {
		t.Fatalf("retest response carries a vision key: %s", env.Data)
	}
	if _, ok := data["applied"]; !ok {
		t.Errorf("retest response lost its applied flag: %s", env.Data)
	}
	if _, ok := data["candidate"]; !ok {
		t.Errorf("retest response lost its candidate object: %s", env.Data)
	}
	if data["verification_status"] == nil {
		t.Errorf("retest response lost the old-client top-level shape: %s", env.Data)
	}
}

// topLevelKey reads one key off a decoded JSON object without asserting
// anything about the rest.
func topLevelKey(data map[string]interface{}, key string) (interface{}, bool) {
	v, ok := data[key]
	return v, ok
}
