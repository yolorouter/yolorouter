package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// seedBulkPriceCandidateRow inserts a provider, a model and one priced
// token-mode candidate directly through gorm, and returns the candidate id.
func seedBulkPriceCandidateRow(t *testing.T, db *gorm.DB, name string, in, out float64) uint {
	t.Helper()
	now := time.Now().UTC()
	p := &model.Provider{Name: "prov-" + name, ProviderType: "openai", BaseURL: "https://example.invalid",
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	m := &model.Model{Name: "model-" + name, ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	c := &model.ModelCandidate{ModelID: m.ID, ProviderID: p.ID, ProviderModelName: name,
		InputPrice: in, OutputPrice: out, PricesDeclared: true, SortOrder: 1,
		ManagementStatus: model.ModelCandidateStatusDisabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(c).Error; err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	return c.ID
}

// TestBulkPriceRequestValidationArms drives every 400 arm of the request
// contract: nil dry_run, unknown operation, scale without a factor or with a
// non-positive one, apply_suggested carrying any factor, an empty id list and
// one past the raw-length cap.
func TestBulkPriceRequestValidationArms(t *testing.T) {
	r, _ := newModelTestRouter(t)

	overCap := make([]uint, 2001)
	for i := range overCap {
		overCap[i] = uint(i + 1)
	}

	arms := []struct {
		name string
		body map[string]interface{}
	}{
		{"missing dry_run", map[string]interface{}{
			"candidate_ids": []uint{1}, "operation": "scale", "factor": 2.0}},
		{"null dry_run", map[string]interface{}{
			"candidate_ids": []uint{1}, "operation": "scale", "factor": 2.0, "dry_run": nil}},
		{"missing operation", map[string]interface{}{
			"candidate_ids": []uint{1}, "dry_run": true}},
		{"invalid operation", map[string]interface{}{
			"candidate_ids": []uint{1}, "operation": "multiply", "factor": 2.0, "dry_run": true}},
		{"scale missing factor", map[string]interface{}{
			"candidate_ids": []uint{1}, "operation": "scale", "dry_run": true}},
		{"scale null factor", map[string]interface{}{
			"candidate_ids": []uint{1}, "operation": "scale", "factor": nil, "dry_run": true}},
		{"scale zero factor", map[string]interface{}{
			"candidate_ids": []uint{1}, "operation": "scale", "factor": 0, "dry_run": true}},
		{"scale negative factor", map[string]interface{}{
			"candidate_ids": []uint{1}, "operation": "scale", "factor": -1.5, "dry_run": true}},
		{"apply_suggested with zero factor", map[string]interface{}{
			"candidate_ids": []uint{1}, "operation": "apply_suggested", "factor": 0, "dry_run": true}},
		{"apply_suggested with positive factor", map[string]interface{}{
			"candidate_ids": []uint{1}, "operation": "apply_suggested", "factor": 1.5, "dry_run": true}},
		{"empty candidate_ids", map[string]interface{}{
			"candidate_ids": []uint{}, "operation": "scale", "factor": 2.0, "dry_run": true}},
		{"null candidate_ids", map[string]interface{}{
			"operation": "scale", "factor": 2.0, "dry_run": true}},
		{"over cap candidate_ids", map[string]interface{}{
			"candidate_ids": overCap, "operation": "scale", "factor": 2.0, "dry_run": true}},
	}
	for _, arm := range arms {
		w, _ := doJSON(t, r, http.MethodPost, "/api/admin/models/candidates/bulk-price", arm.body, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d, body: %s", arm.name, w.Code, w.Body.String())
		}
	}
}

// TestBulkPriceDryRunPreviewRoundTrip drives one full 200 preview: the seeded
// row comes back with scaled after slots and the would-apply count, and an
// unknown id reports not_found.
func TestBulkPriceDryRunPreviewRoundTrip(t *testing.T) {
	r, db := newModelTestRouter(t)
	id := seedBulkPriceCandidateRow(t, db, "preview-x", 2, 8)

	w, env := doJSON(t, r, http.MethodPost, "/api/admin/models/candidates/bulk-price", map[string]interface{}{
		"candidate_ids": []uint{id, 99999}, "operation": "scale", "factor": 1.5, "dry_run": true,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var result struct {
		Items []struct {
			CandidateID  uint   `json:"candidate_id"`
			ModelName    string `json:"model_name"`
			ProviderName string `json:"provider_name"`
			Before       struct {
				InputPrice  float64 `json:"input_price"`
				OutputPrice float64 `json:"output_price"`
			} `json:"before"`
			After struct {
				InputPrice  float64 `json:"input_price"`
				OutputPrice float64 `json:"output_price"`
			} `json:"after"`
			Changed    bool   `json:"changed"`
			SkipReason string `json:"skip_reason"`
		} `json:"items"`
		Applied int `json:"applied"`
		Skipped int `json:"skipped"`
	}
	if err := json.Unmarshal(env.Data, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.Applied != 1 || result.Skipped != 1 || len(result.Items) != 2 {
		t.Fatalf("expected 1 would-apply + 1 skip, got %+v", result)
	}
	first := result.Items[0]
	if first.CandidateID != id || !first.Changed || first.SkipReason != "" {
		t.Fatalf("expected the priced row to preview as changed, got %+v", first)
	}
	if first.Before.InputPrice != 2 || first.After.InputPrice != 3 || first.After.OutputPrice != 12 {
		t.Fatalf("preview slots wrong, got before %+v after %+v", first.Before, first.After)
	}
	if first.ModelName == "" || first.ProviderName == "" {
		t.Fatalf("result rows carry the model and provider names, got %+v", first)
	}
	if result.Items[1].CandidateID != 99999 || result.Items[1].SkipReason != "not_found" {
		t.Fatalf("expected the unknown id to skip not_found, got %+v", result.Items[1])
	}

	// The preview must not have written anything.
	var c model.ModelCandidate
	if err := db.First(&c, id).Error; err != nil {
		t.Fatalf("reload candidate: %v", err)
	}
	if c.InputPrice != 2 || c.OutputPrice != 8 {
		t.Fatalf("dry_run must not write, got %v/%v", c.InputPrice, c.OutputPrice)
	}
}

// TestBulkPriceApplyPersistsThroughHandler drives the write path end to end
// through the HTTP surface.
func TestBulkPriceApplyPersistsThroughHandler(t *testing.T) {
	r, db := newModelTestRouter(t)
	id := seedBulkPriceCandidateRow(t, db, "apply-x", 2, 8)

	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/models/candidates/bulk-price", map[string]interface{}{
		"candidate_ids": []uint{id}, "operation": "scale", "factor": 2, "dry_run": false,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var c model.ModelCandidate
	if err := db.First(&c, id).Error; err != nil {
		t.Fatalf("reload candidate: %v", err)
	}
	if c.InputPrice != 4 || c.OutputPrice != 16 {
		t.Fatalf("apply must persist the scaled prices, got %v/%v", c.InputPrice, c.OutputPrice)
	}
}

// TestBulkPriceAuditSampleCapsAtFive pins the audit detail's sample bound: a
// batch with more changed rows than BulkPriceAuditSampleCap samples exactly
// the first cap CHANGED rows — the leading unchanged row must not crowd the
// sample, and the cap must not overflow into a 2000-row blob.
func TestBulkPriceAuditSampleCapsAtFive(t *testing.T) {
	items := []modeladmin.BulkPriceItem{
		{CandidateID: 900, SkipReason: modeladmin.BulkPriceSkipUnpriced},
	}
	for id := uint(1); id <= 7; id++ {
		items = append(items, modeladmin.BulkPriceItem{CandidateID: id, Changed: true,
			Before: modeladmin.BulkPriceSlots{InputPrice: 1}, After: modeladmin.BulkPriceSlots{InputPrice: 2}})
	}
	dry := true
	detail := bulkPriceAuditDetail(bulkPriceRequest{
		CandidateIDs: []uint{900, 1, 2, 3, 4, 5, 6, 7},
		Operation:    modeladmin.BulkPriceOperationScale,
		DryRun:       &dry,
	}, &modeladmin.BulkPriceResult{Items: items, Applied: 7})

	var doc struct {
		Requested int `json:"requested"`
		Applied   int `json:"applied"`
		Skipped   int `json:"skipped"`
		Sample    []struct {
			CandidateID uint `json:"candidate_id"`
		} `json:"sample"`
	}
	if err := json.Unmarshal([]byte(detail), &doc); err != nil {
		t.Fatalf("unmarshal detail %q: %v", detail, err)
	}
	if doc.Requested != 8 || doc.Applied != 7 || doc.Skipped != 0 {
		t.Fatalf("counts = %d/%d/%d, want 8/7/0", doc.Requested, doc.Applied, doc.Skipped)
	}
	// Pin the constant itself: the fixture below assumes exactly five.
	if modeladmin.BulkPriceAuditSampleCap != 5 {
		t.Fatalf("sample cap moved to %d; the batch here carries 7 changed rows — resize it with the constant", modeladmin.BulkPriceAuditSampleCap)
	}
	if len(doc.Sample) != modeladmin.BulkPriceAuditSampleCap {
		t.Fatalf("sample = %d entries, want exactly the cap %d", len(doc.Sample), modeladmin.BulkPriceAuditSampleCap)
	}
	for i, row := range doc.Sample {
		if row.CandidateID != uint(i+1) {
			t.Fatalf("sample[%d] = candidate %d, want the first five changed rows in request order", i, row.CandidateID)
		}
	}
}

// TestBulkPriceInvalidMapsToParamErrorCode drives the service-error table the
// way writeServiceError resolves it: a direct caller's wrapped
// ErrBulkPriceInvalid must surface as the 400 parameter envelope carrying the
// verbatim rule text, not fall through to the generic 500.
func TestBulkPriceInvalidMapsToParamErrorCode(t *testing.T) {
	wrapped := fmt.Errorf("%w: scale factor must be greater than zero", modeladmin.ErrBulkPriceInvalid)
	rec, body := writeServiceErrorResponse(t, wrapped)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want the 400 parameter envelope", rec.Code)
	}
	if !strings.Contains(body, fmt.Sprint(errcode.InvalidParam)) {
		t.Errorf("body %q does not carry code %d", body, errcode.InvalidParam)
	}
	if !strings.Contains(body, "scale factor must be greater than zero") {
		t.Errorf("body %q lost the verbatim rule text", body)
	}
}
