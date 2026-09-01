package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
)

func seedProviderRow(t *testing.T, db *gorm.DB, name string) *model.Provider {
	t.Helper()
	now := time.Now().UTC()
	p := &model.Provider{
		Name: name, ProviderType: "openai", BaseURL: "https://example.invalid",
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seed provider failed: %v", err)
	}
	return p
}

func TestPostProviderModelsImportCreatesAndReports(t *testing.T) {
	r, db := newModelTestRouter(t)
	p := seedProviderRow(t, db, "prov-import")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/import", p.ID), map[string]interface{}{
		"items": []map[string]interface{}{
			{"provider_model_name": "deepseek-ai/DeepSeek-V4", "input_price": 2, "output_price": 8},
		},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var result struct {
		Items []struct {
			Name        string `json:"name"`
			Status      string `json:"status"`
			ModelID     uint   `json:"model_id"`
			CandidateID uint   `json:"candidate_id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &result); err != nil {
		t.Fatalf("unmarshal import response: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Status != "created" || result.Items[0].CandidateID == 0 {
		t.Fatalf("expected one created item with a candidate id, got %+v", result)
	}
}

// Import must hand every stored mapping to the probe queue — without that the
// asynchronous verification the endpoint promises never starts.
func TestPostProviderModelsImportEnqueuesStoredCandidates(t *testing.T) {
	r, db, queue := newModelTestRouterWithQueue(t)
	p := seedProviderRow(t, db, "prov-import")

	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/import", p.ID), map[string]interface{}{
		"items": []map[string]interface{}{
			{"provider_model_name": "model-a", "input_price": 1, "output_price": 2},
			{"provider_model_name": "model-b", "input_price": 1, "output_price": 2},
			{"provider_model_name": "bad name!", "input_price": 1, "output_price": 2},
		},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if got := queue.PendingCount(); got != 2 {
		t.Fatalf("expected the two stored mappings queued for probing, got %d", got)
	}
}

func TestPostProviderModelsImportRejectsEmptyItems(t *testing.T) {
	r, db := newModelTestRouter(t)
	p := seedProviderRow(t, db, "prov-import")
	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/import", p.ID),
		map[string]interface{}{"items": []map[string]interface{}{}}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an empty items list, got %d", w.Code)
	}
}

func TestGetProviderCandidatesReturnsList(t *testing.T) {
	r, db := newModelTestRouter(t)
	p := seedProviderRow(t, db, "prov-list")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/import", p.ID), map[string]interface{}{
		"items": []map[string]interface{}{
			{"provider_model_name": "vendor/model-a", "input_price": 1, "output_price": 2},
		},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("seed import: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}

	w, env = doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/providers/%d/candidates", p.ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var result struct {
		List []struct {
			ModelName          string `json:"model_name"`
			VerificationStatus int    `json:"verification_status"`
		} `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &result); err != nil {
		t.Fatalf("unmarshal candidates response: %v", err)
	}
	if len(result.List) != 1 || result.List[0].ModelName != "vendor/model-a" {
		t.Fatalf("expected the imported mapping listed with its model name, got %+v", result)
	}
}

// Each listed row carries its live queue position, so the progress dialog can
// tell a mapping waiting its turn ("queued") apart from one no probe will
// visit (empty). The test router's queue is deliberately never started, so an
// imported mapping stays exactly where Enqueue put it.
func TestGetProviderCandidatesStampsQueueState(t *testing.T) {
	r, db, _ := newModelTestRouterWithQueue(t)
	p := seedProviderRow(t, db, "prov-queue-state")

	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/import", p.ID), map[string]interface{}{
		"items": []map[string]interface{}{
			{"provider_model_name": "vendor/model-a", "input_price": 1, "output_price": 2},
		},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("seed import: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}

	w, env := doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/providers/%d/candidates", p.ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var result struct {
		List []struct {
			QueueState string `json:"queue_state"`
		} `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &result); err != nil {
		t.Fatalf("unmarshal candidates response: %v", err)
	}
	if len(result.List) != 1 || result.List[0].QueueState != "queued" {
		t.Fatalf("expected the still-enqueued mapping to report queue_state=queued, got %+v", result)
	}
}

// A worker can land a verdict between this endpoint's database read and its
// queue lookup: the response would then pair a stale Untested row with an
// empty queue_state — the one combination a freshly opened page treats as
// settled-idle and never polls, freezing it on Pending until a manual reload.
// Rows that look like that get one re-read before the response goes out.
func TestGetProviderCandidatesRefreshesRowsThatSettleMidRequest(t *testing.T) {
	r, db, _ := newModelTestRouterWithQueue(t)
	p := seedProviderRow(t, db, "prov-settle-race")
	now := time.Now().UTC()
	m := &model.Model{Name: "race-model", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	// Armed like every row the queue actually works on (import and requeue
	// both arm): the torn-row re-read keys on the promise, because without it
	// the untested-unstamped shape is a lasting legal state, not a tear.
	cand := &model.ModelCandidate{
		ModelID: m.ID, ProviderID: p.ID, ProviderModelName: "race-model",
		ManagementStatus: model.ModelCandidateStatusDisabled, VerificationStatus: model.ModelVerificationStatusUntested,
		AutoEnableOnPass: true, ArmedAt: &now,
		CreatedAt: now, UpdatedAt: now, PriceUpdatedAt: now,
	}
	if err := db.Create(cand).Error; err != nil {
		t.Fatalf("seed candidate: %v", err)
	}

	// The verdict lands right after the handler's first candidates read — the
	// moment a real worker would also have left the queue.
	fired := false
	if err := db.Callback().Query().After("gorm:query").Register("test:settle-after-read", func(tx *gorm.DB) {
		if fired || tx.Error != nil {
			return
		}
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "model_candidates" {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		passed := model.ModelVerificationStatusPassed
		result := 1
		if _, err := repository.CommitModelCandidateProbeResults(session, cand.ID, "race-model", "", "settled-run",
			repository.CandidateProbeCommit{VerificationStatus: &passed, LastTestResult: &result, WriteLastTestError: true},
			time.Now().UTC()); err != nil {
			t.Errorf("settle candidate: %v", err)
		}
	}); err != nil {
		t.Fatalf("register settle callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Query().Remove("test:settle-after-read"); err != nil {
			t.Fatalf("remove settle callback: %v", err)
		}
	}()

	w, env := doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/admin/providers/%d/candidates", p.ID), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var result struct {
		List []struct {
			VerificationStatus int    `json:"verification_status"`
			QueueState         string `json:"queue_state"`
		} `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &result); err != nil {
		t.Fatalf("unmarshal candidates response: %v", err)
	}
	if len(result.List) != 1 {
		t.Fatalf("expected 1 row, got %+v", result)
	}
	if result.List[0].VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected the response to carry the settled verdict, not the stale Untested snapshot, got %+v", result.List[0])
	}
}

func TestPostProviderSuggestPricesRejectsEmptyName(t *testing.T) {
	r, db := newModelTestRouter(t)
	p := seedProviderRow(t, db, "prov-prices")
	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/suggest-prices", p.ID),
		map[string]interface{}{"names": []string{""}}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an empty name entry, got %d", w.Code)
	}
}

func TestPostProviderSuggestPricesReturnsEntryPerName(t *testing.T) {
	r, db := newModelTestRouter(t)
	p := seedProviderRow(t, db, "prov-prices")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/suggest-prices", p.ID),
		map[string]interface{}{"names": []string{"some-model"}}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var result struct {
		Prices map[string]struct {
			Source string `json:"source"`
		} `json:"prices"`
	}
	if err := json.Unmarshal(env.Data, &result); err != nil {
		t.Fatalf("unmarshal suggest-prices response: %v", err)
	}
	entry, ok := result.Prices["some-model"]
	if !ok || entry.Source != "" {
		t.Fatalf("expected an empty-source entry for the unmatched name, got %+v", result)
	}
}

// Upstream catalogs really do exceed 500 ids (the provider client follows up
// to 20 pages with no per-page cap), so a full-catalog batch must not be
// rejected at the binding layer.
func TestPostProviderModelsImportAcceptsFullCatalogSizedBatch(t *testing.T) {
	r, db := newModelTestRouter(t)
	p := seedProviderRow(t, db, "prov-import")
	items := make([]map[string]interface{}, 501)
	for i := range items {
		items[i] = map[string]interface{}{"provider_model_name": fmt.Sprintf("vendor/model-%d", i), "input_price": 1, "output_price": 2}
	}
	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/import", p.ID),
		map[string]interface{}{"items": items}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for a 501-item catalog import, got %d, body: %s", w.Code, w.Body.String()[:200])
	}
}

func TestPostProviderSuggestPricesAcceptsFullCatalogSizedBatch(t *testing.T) {
	r, db := newModelTestRouter(t)
	p := seedProviderRow(t, db, "prov-prices")
	names := make([]string, 501)
	for i := range names {
		names[i] = fmt.Sprintf("vendor/model-%d", i)
	}
	w, _ := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/suggest-prices", p.ID),
		map[string]interface{}{"names": names}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for a 501-name price look-up, got %d, body: %s", w.Code, w.Body.String()[:200])
	}
}

// The transport layer must carry the per-row modality declaration into the
// service and the per-item skip reasons back out — the import dialog's
// conflict messaging rides on both directions.
func TestPostProviderModelsImportDeclaresModality(t *testing.T) {
	r, db := newModelTestRouter(t)
	p := seedProviderRow(t, db, "prov-import-modality")

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/import", p.ID), map[string]interface{}{
		"items": []map[string]interface{}{
			{"provider_model_name": "wan2.7-image", "output_modalities": []string{"image"}},
		},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var result struct {
		Items []struct {
			Name        string `json:"name"`
			Status      string `json:"status"`
			ModelID     uint   `json:"model_id"`
			CandidateID uint   `json:"candidate_id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &result); err != nil {
		t.Fatalf("unmarshal import response: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Status != "created" {
		t.Fatalf("expected one created item, got %+v", result.Items)
	}
	var m model.Model
	if err := db.First(&m, result.Items[0].ModelID).Error; err != nil {
		t.Fatalf("load imported model: %v", err)
	}
	if m.OutputModalities != `["image"]` {
		t.Fatalf("expected the row declaration stored, got %q", m.OutputModalities)
	}
	var c model.ModelCandidate
	if err := db.First(&c, result.Items[0].CandidateID).Error; err != nil {
		t.Fatalf("load imported candidate: %v", err)
	}
	if c.BillingMode != model.BillingModeImage {
		t.Fatalf("an image-exclusive row must bill per image, got %q", c.BillingMode)
	}
}

func TestPostProviderModelsImportSurfacesModalityMismatch(t *testing.T) {
	r, db := newModelTestRouter(t)
	p := seedProviderRow(t, db, "prov-import-mismatch")
	now := time.Now().UTC()
	if err := db.Create(&model.Model{
		Name: "wan2.7-image", ManagementStatus: model.ModelStatusEnabled,
		SchedulingMode: model.ModelSchedulingModeFailover, OutputModalities: `["text"]`,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed model failed: %v", err)
	}

	w, env := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/admin/providers/%d/models/import", p.ID), map[string]interface{}{
		"items": []map[string]interface{}{
			{"provider_model_name": "wan2.7-image", "output_modalities": []string{"image"}},
		},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (a per-item skip, not a request failure), got %d, body: %s", w.Code, w.Body.String())
	}
	var result struct {
		Items []struct {
			Name        string `json:"name"`
			Status      string `json:"status"`
			Reason      string `json:"reason"`
			CandidateID uint   `json:"candidate_id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &result); err != nil {
		t.Fatalf("unmarshal import response: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Status != "skipped" || result.Items[0].Reason != "modality_mismatch" || result.Items[0].CandidateID != 0 {
		t.Fatalf("expected a modality_mismatch skip with no candidate, got %+v", result.Items)
	}
	var count int64
	if err := db.Model(&model.ModelCandidate{}).Where("provider_id = ?", p.ID).Count(&count).Error; err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if count != 0 {
		t.Fatalf("a refused row must store no mapping, found %d", count)
	}
}
