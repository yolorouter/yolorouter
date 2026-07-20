package gateway

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/repository"
	"github.com/yolorouter/yolorouter-ce/internal/testutil"
)

func TestComputeCost(t *testing.T) {
	// Candidate prices are CNY per million tokens (design doc §3.3).
	// 1M input @ 1.0 + 0.5M output @ 2.0 = 1.0 + 1.0 = 2.0 CNY = 200 cents.
	cand := &model.ModelCandidate{InputPrice: 1.0, OutputPrice: 2.0}
	usage := &Usage{PromptTokens: 1_000_000, CompletionTokens: 500_000}
	cents, known := computeCost(cand, usage)
	if !known {
		t.Fatal("expected cost to be known when usage + candidate present")
	}
	if cents != 200 {
		t.Fatalf("cost = %d cents, want 200", cents)
	}
}

func TestComputeCostRoundsToCent(t *testing.T) {
	// 1 token @ 1.0/M = 0.000001 CNY = 0.0001 cents -> rounds to 0 cents.
	cand := &model.ModelCandidate{InputPrice: 1.0, OutputPrice: 0}
	usage := &Usage{PromptTokens: 1, CompletionTokens: 0}
	cents, known := computeCost(cand, usage)
	if !known || cents != 0 {
		t.Fatalf("expected known 0 cents, got %d (known=%v)", cents, known)
	}
}

func TestComputeCostMissingUsageIsUnknown(t *testing.T) {
	// GATE-21: missing usage must be "unknown", never 0 cost.
	cand := &model.ModelCandidate{InputPrice: 1.0, OutputPrice: 1.0}
	if cents, known := computeCost(cand, nil); known || cents != 0 {
		t.Fatalf("expected unknown/0 for nil usage, got %d (known=%v)", cents, known)
	}
}

func TestComputeCostMissingCandidateIsUnknown(t *testing.T) {
	usage := &Usage{PromptTokens: 100, CompletionTokens: 100}
	if cents, known := computeCost(nil, usage); known || cents != 0 {
		t.Fatalf("expected unknown/0 for nil candidate, got %d (known=%v)", cents, known)
	}
}

// TestFinalizeWritesBodyRow (Codex #5, PRD §6.8.4/§6.8.6/LOG-06): finalize
// persists the four body fields (already redacted at capture) into
// request_log_bodies, keyed by request_id, alongside the request_logs row.
func TestFinalizeWritesBodyRow(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newRelaySvc(t, db)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, nil)

	rc := &RelayContext{
		RequestID:            "req-body-1",
		APIKeyID:             apiKey.ID,
		RequestBody:          []byte(`{"model":"gpt-4o"}`),
		UpstreamRequestBody:  []byte(`{"model":"gpt-4o-real"}`),
		ResponseBody:         []byte(`{"model":"gpt-4o","choices":[]}`),
		UpstreamResponseBody: []byte(`{"model":"gpt-4o-real","choices":[]}`),
	}

	svc.finalize(rc, 200, "", time.Now())

	var logCount int64
	db.Model(&model.RequestLog{}).Count(&logCount)
	if logCount != 1 {
		t.Fatalf("expected 1 request_logs row, got %d", logCount)
	}

	body, err := repository.GetRequestLogBodyByRequestID(db, "req-body-1")
	if err != nil {
		t.Fatalf("GetRequestLogBodyByRequestID: %v", err)
	}
	if body == nil {
		t.Fatal("expected a request_log_bodies row, got none")
	}
	if body.RequestBody != `{"model":"gpt-4o"}` {
		t.Errorf("RequestBody = %q, want caller's original request", body.RequestBody)
	}
	if body.UpstreamRequestBody != `{"model":"gpt-4o-real"}` {
		t.Errorf("UpstreamRequestBody = %q, want rewritten upstream request", body.UpstreamRequestBody)
	}
	if body.ResponseBody != `{"model":"gpt-4o","choices":[]}` {
		t.Errorf("ResponseBody = %q, want caller-facing response", body.ResponseBody)
	}
	if body.UpstreamResponseBody != `{"model":"gpt-4o-real","choices":[]}` {
		t.Errorf("UpstreamResponseBody = %q, want raw upstream response", body.UpstreamResponseBody)
	}
	if body.StreamBodyPath != "" {
		t.Errorf("StreamBodyPath = %q, want empty for a non-stream request", body.StreamBodyPath)
	}
}

// TestFinalizeBodyWriteFailureDoesNotRollbackBilling (Codex #5): the
// request_log_bodies write is best-effort — a failure there must not roll
// back or block the request_logs row (billing/audit trail), which is written
// first and is authoritative.
func TestFinalizeBodyWriteFailureDoesNotRollbackBilling(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newRelaySvc(t, db)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, nil)

	// Force UpsertRequestLogBody to fail without touching request_logs, so
	// the assertion below proves the body write's failure never rolled back
	// (or panicked around) the earlier billing/log write.
	if err := db.Exec("DROP TABLE request_log_bodies").Error; err != nil {
		t.Fatalf("drop request_log_bodies: %v", err)
	}

	rc := &RelayContext{RequestID: "req-body-fail-1", APIKeyID: apiKey.ID}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("finalize panicked on request_log_bodies write failure: %v", r)
		}
	}()
	svc.finalize(rc, 200, "", time.Now())

	var logCount int64
	db.Model(&model.RequestLog{}).Count(&logCount)
	if logCount != 1 {
		t.Fatalf("expected request_logs row despite body-write failure, got count=%d", logCount)
	}
}

func TestGenerateRequestIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := generateRequestID()
		if len(id) != 16 {
			t.Fatalf("id length = %d, want 16 (id=%q)", len(id), id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id generated: %q", id)
		}
		seen[id] = struct{}{}
	}
}
