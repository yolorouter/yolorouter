package handler

// Capability-metadata exposure on the model PATCH/GET endpoints: the five
// new fields ride the wire in their declared tri-state forms (string enums
// for the bools, null-clearing wrappers for the numbers, an empty-present
// array for the input list), and the derived supports_image_input mirror is
// visible through the same responses the console reads.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/yolorouter/yolorouter/pkg/errcode"
)

type modelCapabilityResponse struct {
	ContextLength      *int     `json:"context_length"`
	InputModalities    []string `json:"input_modalities"`
	SupportsTools      *bool    `json:"supports_tools"`
	SupportsReasoning  *bool    `json:"supports_reasoning"`
	Temperature        *float64 `json:"temperature"`
	SupportsImageInput *bool    `json:"supports_image_input"`
}

// TestPatchModelCapabilityMetadataRoundTrip walks the full wire lifecycle:
// set all five, read them back through PATCH's own response and through a
// fresh GET, clear them all (each by its own clearing form), and confirm an
// unrelated PATCH preserves them.
func TestPatchModelCapabilityMetadataRoundTrip(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "cap-api")

	patch := func(t *testing.T, body map[string]interface{}) modelCapabilityResponse {
		t.Helper()
		w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", id), body, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("patch: expected 200, got %d, body: %s", w.Code, w.Body.String())
		}
		var m modelCapabilityResponse
		if err := json.Unmarshal(env.Data, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return m
	}

	// Set all five in one PATCH; the response itself must carry them. The
	// temperature deliberately carries three decimals: the PATCH response is
	// rebuilt from the stored row (GetModelDetail), so the exact 0.705
	// read-back proves the declaration survived the store bit-exactly — the
	// precision caliber the PG arm's DOUBLE PRECISION column and SQLite's
	// REAL share. A 2-decimal value could not tell a rounding column from a
	// faithful one.
	m := patch(t, map[string]interface{}{
		"name":               "cap-api",
		"context_length":     200000,
		"input_modalities":   []string{"text", "image"},
		"supports_tools":     "yes",
		"supports_reasoning": "no",
		"temperature":        0.705,
	})
	if m.ContextLength == nil || *m.ContextLength != 200000 {
		t.Fatalf("context_length = %v, want 200000", m.ContextLength)
	}
	if len(m.InputModalities) != 2 || m.InputModalities[1] != "image" {
		t.Fatalf("input_modalities = %v, want [text image]", m.InputModalities)
	}
	if m.SupportsTools == nil || !*m.SupportsTools {
		t.Fatalf("supports_tools = %v, want true", m.SupportsTools)
	}
	if m.SupportsReasoning == nil || *m.SupportsReasoning {
		t.Fatalf("supports_reasoning = %v, want false", m.SupportsReasoning)
	}
	if m.Temperature == nil || *m.Temperature != 0.705 {
		t.Fatalf("temperature = %v, want 0.705", m.Temperature)
	}
	if m.SupportsImageInput == nil || !*m.SupportsImageInput {
		t.Fatalf("supports_image_input mirror = %v, want true for an image-declaring list", m.SupportsImageInput)
	}

	// A fresh GET must expose the same fields — the card view reads this.
	w, env := doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/models/%d", id), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var got modelCapabilityResponse
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatalf("unmarshal GET: %v", err)
	}
	if got.ContextLength == nil || got.Temperature == nil || got.SupportsTools == nil ||
		got.SupportsReasoning == nil || len(got.InputModalities) != 2 || got.SupportsImageInput == nil {
		t.Fatalf("GET response lost capability fields: %+v", got)
	}
	if *got.Temperature != 0.705 {
		t.Fatalf("GET temperature = %v, want the exact declared 0.705", *got.Temperature)
	}

	// An unrelated PATCH keeps every stored declaration.
	m = patch(t, map[string]interface{}{"name": "cap-api-renamed"})
	if m.ContextLength == nil || m.Temperature == nil || m.SupportsTools == nil ||
		m.SupportsReasoning == nil || len(m.InputModalities) != 2 || m.SupportsImageInput == nil {
		t.Fatalf("unrelated PATCH dropped stored declarations: %+v", m)
	}

	// Clear all five, each through its own clearing form: null for the
	// numbers, "unknown" for the bools, an empty present array for the list.
	m = patch(t, map[string]interface{}{
		"name":               "cap-api-renamed",
		"context_length":     nil,
		"temperature":        nil,
		"supports_tools":     "unknown",
		"supports_reasoning": "unknown",
		"input_modalities":   []string{},
	})
	if m.ContextLength != nil || m.Temperature != nil || m.SupportsTools != nil ||
		m.SupportsReasoning != nil || m.InputModalities != nil || m.SupportsImageInput != nil {
		t.Fatalf("cleared declarations must read undeclared on the wire, got %+v", m)
	}
}

// The mirror through the wire: a legacy-declared image-input model whose
// input declaration stops containing image must read supports_image_input
// false afterwards — the vision-fallback hot path reads that column, so the
// console must never be able to leave the pair inconsistent.
func TestPatchModelInputModalitiesMirrorThroughAPI(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "mirror-api")

	patch := func(t *testing.T, body map[string]interface{}) modelCapabilityResponse {
		t.Helper()
		w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", id), body, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("patch: expected 200, got %d, body: %s", w.Code, w.Body.String())
		}
		var m modelCapabilityResponse
		if err := json.Unmarshal(env.Data, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return m
	}

	if m := patch(t, map[string]interface{}{"name": "mirror-api", "image_input": "yes"}); m.SupportsImageInput == nil || !*m.SupportsImageInput {
		t.Fatalf("legacy image_input=yes: got %v, want true", m.SupportsImageInput)
	}
	m := patch(t, map[string]interface{}{"name": "mirror-api", "input_modalities": []string{"text"}})
	if m.SupportsImageInput == nil || *m.SupportsImageInput {
		t.Fatalf("after dropping image from the declaration: got %v, want false", m.SupportsImageInput)
	}
	if m := patch(t, map[string]interface{}{"name": "mirror-api", "input_modalities": []string{}}); m.SupportsImageInput != nil {
		t.Fatalf("after clearing the declaration: got %v, want nil", *m.SupportsImageInput)
	}
}

// An unknown input modality id answers with the field's own error code, not
// a generic failure — the same contract the output list already follows.
func TestPatchModelInputModalitiesRejectsUnknownID(t *testing.T) {
	r, _ := newModelTestRouter(t)
	id := createModelForTest(t, r, "vocab-api")

	w, env := doJSON(t, r, http.MethodPatch, fmt.Sprintf("/api/admin/models/%d", id),
		map[string]interface{}{"name": "vocab-api", "input_modalities": []string{"telepathy"}}, nil)
	if w.Code == http.StatusOK {
		t.Fatalf("unknown modality id accepted: %s", w.Body.String())
	}
	if env.Code != errcode.ModelInputModalityInvalid {
		t.Fatalf("code = %d, want %d (body: %s)", env.Code, errcode.ModelInputModalityInvalid, w.Body.String())
	}
}
