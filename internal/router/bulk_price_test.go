package router

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/auth"
)

// seedBulkPriceWorld inserts one enabled provider, one model and one priced
// token-mode candidate directly, the minimal world a bulk-price request acts
// on through the real router.
func seedBulkPriceWorld(t *testing.T, f *auditFixture, name string, in, out float64) uint {
	t.Helper()
	now := time.Now().UTC()
	p := &model.Provider{Name: "bulk-prov-" + name, ProviderType: "openai", BaseURL: "https://example.invalid",
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1, CreatedAt: now, UpdatedAt: now}
	if err := f.db.Create(p).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	m := &model.Model{Name: "bulk-model-" + name, ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := f.db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	c := &model.ModelCandidate{ModelID: m.ID, ProviderID: p.ID, ProviderModelName: name,
		InputPrice: in, OutputPrice: out, PricesDeclared: true, SortOrder: 1,
		ManagementStatus: model.ModelCandidateStatusDisabled, CreatedAt: now, UpdatedAt: now}
	if err := f.db.Create(c).Error; err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	return c.ID
}

func loadBulkPriceCandidateRow(t *testing.T, f *auditFixture, id uint) model.ModelCandidate {
	t.Helper()
	var c model.ModelCandidate
	if err := f.db.First(&c, id).Error; err != nil {
		t.Fatalf("reload candidate: %v", err)
	}
	return c
}

// bulkPriceAuditDetailDoc mirrors the handler's fixed detail shape, so the
// assertion reads field by field instead of by string surgery.
type bulkPriceAuditDetailDoc struct {
	Operation string `json:"operation"`
	DryRun    bool   `json:"dry_run"`
	Requested int    `json:"requested"`
	Applied   int    `json:"applied"`
	Skipped   int    `json:"skipped"`
	Sample    []struct {
		CandidateID  uint   `json:"candidate_id"`
		ModelName    string `json:"model_name"`
		ProviderName string `json:"provider_name"`
		Before       struct {
			InputPrice      float64  `json:"input_price"`
			OutputPrice     float64  `json:"output_price"`
			CacheWritePrice *float64 `json:"cache_write_price"`
			CacheReadPrice  *float64 `json:"cache_read_price"`
			AudioUnitPrice  *float64 `json:"audio_unit_price"`
		} `json:"before"`
		After struct {
			InputPrice      float64  `json:"input_price"`
			OutputPrice     float64  `json:"output_price"`
			CacheWritePrice *float64 `json:"cache_write_price"`
			CacheReadPrice  *float64 `json:"cache_read_price"`
			AudioUnitPrice  *float64 `json:"audio_unit_price"`
		} `json:"after"`
	} `json:"sample"`
}

// bulkPriceResultDoc mirrors the endpoint's response envelope data.
type bulkPriceResultDoc struct {
	Items []struct {
		CandidateID uint   `json:"candidate_id"`
		Changed     bool   `json:"changed"`
		SkipReason  string `json:"skip_reason"`
	} `json:"items"`
	Applied int `json:"applied"`
	Skipped int `json:"skipped"`
}

func parseBulkPriceResponse(t *testing.T, body []byte) bulkPriceResultDoc {
	t.Helper()
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var result bulkPriceResultDoc
	if err := json.Unmarshal(envelope.Data, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return result
}

// TestBulkPriceMemberSessionDeniedWithNoWrite is the member contract for the
// new write route: a member session is refused 403, the audit trail still
// lands exactly one denied row under the route's dictionary action, and no
// business write happened.
func TestBulkPriceMemberSessionDeniedWithNoWrite(t *testing.T) {
	f := newAuditFixture(t)
	id := seedBulkPriceWorld(t, f, "member-x", 2, 8)
	before := loadBulkPriceCandidateRow(t, f, id)

	now := time.Now().UTC()
	hash, err := auth.HashPassword("MemberPass123!")
	if err != nil {
		t.Fatalf("hash member password: %v", err)
	}
	member := &model.User{Username: "mallory", Role: model.RoleMember, Status: model.UserStatusEnabled,
		IsLocal: true, PasswordHash: hash, CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(f.db, member); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if err := repository.CreateSession(f.db, "tok-bulk-denied", member.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed member session: %v", err)
	}
	memberCk := &http.Cookie{Name: "session_id", Value: "tok-bulk-denied"}

	body := `{"candidate_ids":[` + strconv.FormatUint(uint64(id), 10) + `],"operation":"scale","factor":2,"dry_run":false}`
	w := f.doBody(t, http.MethodPost, "/api/admin/models/candidates/bulk-price", body, memberCk)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member posting bulk-price must be refused 403, got %d: %s", w.Code, w.Body.String())
	}

	rows := f.drain(t)
	if len(rows) != 1 {
		t.Fatalf("the refused write must still land exactly one row, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != "model_candidate.bulk_price_update" {
		t.Errorf("action = %q, want the route's dictionary action model_candidate.bulk_price_update", row.Action)
	}
	if row.Status != model.AuditStatusDenied {
		t.Errorf("status = %q, want denied", row.Status)
	}
	if row.AdminUserID == nil || *row.AdminUserID != member.ID {
		t.Errorf("row must carry the refusing member's id %d, got %v", member.ID, row.AdminUserID)
	}

	after := loadBulkPriceCandidateRow(t, f, id)
	if after.InputPrice != before.InputPrice || after.OutputPrice != before.OutputPrice ||
		!after.PriceUpdatedAt.Equal(before.PriceUpdatedAt) {
		t.Fatalf("a refused member request must not write business rows (before %+v after %+v)", before, after)
	}
}

// TestBulkPriceDryRunAndApplyAuditShape walks the admin happy path through
// the real engine: the dry run lands one audit row whose detail carries the
// fixed shape and writes no business rows; the apply doubles the price,
// advances the price clock, and lands its own row.
func TestBulkPriceDryRunAndApplyAuditShape(t *testing.T) {
	f := newAuditFixture(t)
	id := seedBulkPriceWorld(t, f, "shape-x", 2, 8)
	before := loadBulkPriceCandidateRow(t, f, id)

	body := `{"candidate_ids":[` + strconv.FormatUint(uint64(id), 10) + `],"operation":"scale","factor":2,"dry_run":true}`
	w := f.doBody(t, http.MethodPost, "/api/admin/models/candidates/bulk-price", body, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("dry run must succeed 200, got %d: %s", w.Code, w.Body.String())
	}
	preview := parseBulkPriceResponse(t, w.Body.Bytes())
	if preview.Applied != 1 || preview.Skipped != 0 || len(preview.Items) != 1 || !preview.Items[0].Changed {
		t.Fatalf("preview must report one would-apply row, got %+v", preview)
	}

	rows := f.drain(t)
	if len(rows) != 1 {
		t.Fatalf("the dry run must land exactly one audit row, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != "model_candidate.bulk_price_update" {
		t.Fatalf("action = %q, want model_candidate.bulk_price_update", row.Action)
	}
	if row.Status != model.AuditStatusSuccess {
		t.Fatalf("status = %q, want success", row.Status)
	}

	var detail bulkPriceAuditDetailDoc
	if err := json.Unmarshal([]byte(row.Detail), &detail); err != nil {
		t.Fatalf("unmarshal detail %q: %v", row.Detail, err)
	}
	if detail.Operation != "scale" || !detail.DryRun {
		t.Fatalf("detail operation/dry_run = %q/%v, want scale/true", detail.Operation, detail.DryRun)
	}
	if detail.Requested != 1 || detail.Applied != 1 || detail.Skipped != 0 {
		t.Fatalf("detail counts = %d/%d/%d, want 1/1/0 (would-apply)", detail.Requested, detail.Applied, detail.Skipped)
	}
	if len(detail.Sample) != 1 {
		t.Fatalf("sample must hold the one changed row, got %d", len(detail.Sample))
	}
	sample := detail.Sample[0]
	if sample.CandidateID != id || sample.ModelName != "bulk-model-shape-x" || sample.ProviderName == "" {
		t.Fatalf("sample identity fields wrong, got %+v", sample)
	}
	if sample.Before.InputPrice != 2 || sample.After.InputPrice != 4 || sample.After.OutputPrice != 16 {
		t.Fatalf("sample slots wrong, got before %+v after %+v", sample.Before, sample.After)
	}

	// The dry run wrote nothing.
	afterPreview := loadBulkPriceCandidateRow(t, f, id)
	if afterPreview.InputPrice != 2 || afterPreview.OutputPrice != 8 || !afterPreview.PriceUpdatedAt.Equal(before.PriceUpdatedAt) {
		t.Fatalf("dry_run must not write business rows (before %+v after %+v)", before, afterPreview)
	}

	// The apply leg through a fresh fixture: doubles the price and advances
	// the price clock.
	f2 := newAuditFixture(t)
	id2 := seedBulkPriceWorld(t, f2, "shape-y", 3, 5)
	before2 := loadBulkPriceCandidateRow(t, f2, id2)
	applyBody := `{"candidate_ids":[` + strconv.FormatUint(uint64(id2), 10) + `],"operation":"scale","factor":2,"dry_run":false}`
	w2 := f2.doBody(t, http.MethodPost, "/api/admin/models/candidates/bulk-price", applyBody, f2.adminCk)
	if w2.Code != http.StatusOK {
		t.Fatalf("apply must succeed 200, got %d: %s", w2.Code, w2.Body.String())
	}
	applied := loadBulkPriceCandidateRow(t, f2, id2)
	if applied.InputPrice != 6 || applied.OutputPrice != 10 {
		t.Fatalf("apply must persist 6/10, got %v/%v", applied.InputPrice, applied.OutputPrice)
	}
	if !applied.PriceUpdatedAt.After(before2.PriceUpdatedAt) {
		t.Fatalf("a real change must advance price_updated_at (%v -> %v)", before2.PriceUpdatedAt, applied.PriceUpdatedAt)
	}
	rows2 := f2.drain(t)
	if len(rows2) != 1 {
		t.Fatalf("the apply must land exactly one audit row, got %d", len(rows2))
	}
	if rows2[0].Action != "model_candidate.bulk_price_update" || rows2[0].Status != model.AuditStatusSuccess {
		t.Fatalf("apply row = %q/%q, want model_candidate.bulk_price_update/success", rows2[0].Action, rows2[0].Status)
	}
	var applyDetail bulkPriceAuditDetailDoc
	if err := json.Unmarshal([]byte(rows2[0].Detail), &applyDetail); err != nil {
		t.Fatalf("unmarshal apply detail %q: %v", rows2[0].Detail, err)
	}
	if applyDetail.DryRun || applyDetail.Applied != 1 || applyDetail.Requested != 1 {
		t.Fatalf("apply detail = %+v, want dry_run false requested 1 applied 1", applyDetail)
	}
}
