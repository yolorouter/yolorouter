package handler

// The three save-time consistency rejections through the wire: each refusal
// must surface as its own error code on the 400 envelope (the console keys
// its messages off these numbers), and the same codes answer on the candidate
// save path as on the model save path.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// seedEnabledCandidateThroughAPI adds an enabled candidate on a text-output
// model through the test-and-create endpoint (the always-success client's
// probes pass), returning the candidate id.
func seedEnabledCandidateThroughAPI(t *testing.T, r *gin.Engine, modelID, providerID uint, body map[string]interface{}) uint {
	t.Helper()
	full := map[string]interface{}{
		"provider_id": providerID, "management_status": 1,
	}
	for k, v := range body {
		full[k] = v
	}
	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates/test-and-create", modelID), full, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("seed candidate: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var created struct {
		Candidate *candidateResponse `json:"candidate"`
	}
	if err := json.Unmarshal(env.Data, &created); err != nil || created.Candidate == nil {
		t.Fatalf("unmarshal created candidate: %v, %s", err, env.Data)
	}
	if created.Candidate.ManagementStatus != 1 {
		t.Fatalf("seeded candidate status = %d, want enabled", created.Candidate.ManagementStatus)
	}
	return created.Candidate.ID
}

func TestConsistencyRejectionsSurfaceOwnCodes(t *testing.T) {
	providerRouter, db := newProviderTestRouter(t)
	providerID := createProviderAndKeyForModelTest(t, providerRouter)
	r := newModelTestRouterSharingProviderDB(t, db, &alwaysSuccessClient{})

	t.Run("model path: temperature outside [0,2] answers 12113", func(t *testing.T) {
		id := createModelForTest(t, r, "consistency-temp")
		w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", id),
			map[string]interface{}{"name": "consistency-temp", "temperature": 2.01}, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
		}
		if env.Code != errcode.ModelTemperatureInvalid {
			t.Fatalf("code = %d, want %d (body: %s)", env.Code, errcode.ModelTemperatureInvalid, w.Body.String())
		}
	})

	t.Run("model path: image declaration over a token candidate answers 12111", func(t *testing.T) {
		id := createModelForTest(t, r, "consistency-image")
		seedEnabledCandidateThroughAPI(t, r, id, providerID, map[string]interface{}{
			"provider_model_name": "consistency-image-real",
		})
		w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", id),
			map[string]interface{}{"name": "consistency-image", "output_modalities": []string{"text", "image"}}, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
		}
		if env.Code != errcode.ModelImageBillingMismatch {
			t.Fatalf("code = %d, want %d (body: %s)", env.Code, errcode.ModelImageBillingMismatch, w.Body.String())
		}
	})

	t.Run("model path: context under an enabled candidate's max_output answers 12112", func(t *testing.T) {
		id := createModelForTest(t, r, "consistency-context")
		seedEnabledCandidateThroughAPI(t, r, id, providerID, map[string]interface{}{
			"provider_model_name": "consistency-context-real", "max_output": 8192,
		})
		w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", id),
			map[string]interface{}{"name": "consistency-context", "context_length": 4096}, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
		}
		if env.Code != errcode.ModelMaxOutputExceedsContext {
			t.Fatalf("code = %d, want %d (body: %s)", env.Code, errcode.ModelMaxOutputExceedsContext, w.Body.String())
		}
	})

	t.Run("candidate path: enabled token candidate on an image model answers 12111", func(t *testing.T) {
		w, env := doJSON(t, r, http.MethodPost, "/api/admin/models", map[string]interface{}{
			"name": "consistency-image-cand", "output_modalities": []string{"image"},
		}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("create image model: expected 200, got %d, body: %s", w.Code, w.Body.String())
		}
		var created struct {
			ID uint `json:"id"`
		}
		if err := json.Unmarshal(env.Data, &created); err != nil {
			t.Fatalf("unmarshal model: %v", err)
		}

		w, env = doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/models/%d/candidates/test-and-create", created.ID), map[string]interface{}{
			"provider_id": providerID, "provider_model_name": "consistency-image-cand-real",
			"management_status": 1,
		}, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
		}
		if env.Code != errcode.ModelImageBillingMismatch {
			t.Fatalf("code = %d, want %d (body: %s)", env.Code, errcode.ModelImageBillingMismatch, w.Body.String())
		}
	})
}
