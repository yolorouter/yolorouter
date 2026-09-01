package gateway

import (
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

func TestComputeCost(t *testing.T) {
	// Candidate prices are CNY per million tokens. Cost is
	// stored as integer micros (CNY × 1e6, i.e. 6-decimal precision).
	// 1M input @ 1.0 + 0.5M output @ 2.0 = 1.0 + 1.0 = 2.0 CNY = 2_000_000 micros.
	cand := &model.ModelCandidate{InputPrice: 1.0, OutputPrice: 2.0}
	usage := &protocols.IRUsage{PromptTokens: 1_000_000, CompletionTokens: 500_000}
	cost := computeCost(cand, usage, 0)
	if !cost.Known {
		t.Fatal("expected cost to be known when usage + candidate present")
	}
	if cost.CostMicros != 2_000_000 {
		t.Fatalf("cost = %d micros, want 2_000_000", cost.CostMicros)
	}
}

func TestComputeCostCacheEconomics(t *testing.T) {
	// input @ 3.0/M, cache read @ 0.3/M (cheaper), cache write @ 3.75/M (a
	// premium over input). 1M cache-read tokens save (3.0−0.3)=2.7 CNY;
	// 1M cache-write tokens cost an extra (3.75−3.0)=0.75 CNY.
	readPrice, writePrice := 0.3, 3.75
	cand := &model.ModelCandidate{
		InputPrice:      3.0,
		OutputPrice:     6.0,
		CacheReadPrice:  &readPrice,
		CacheWritePrice: &writePrice,
	}
	usage := &protocols.IRUsage{PromptTokens: 1_000_000, CacheReadTokens: 1_000_000, CacheWriteTokens: 1_000_000}
	cost := computeCost(cand, usage, 0)
	if cost.CacheReadSavedMicros != 2_700_000 {
		t.Errorf("cache read saved = %d, want 2_700_000", cost.CacheReadSavedMicros)
	}
	if cost.CacheWriteExtraMicros != 750_000 {
		t.Errorf("cache write extra = %d, want 750_000", cost.CacheWriteExtraMicros)
	}
}

func TestComputeCostNoCachePriceHasNoSavings(t *testing.T) {
	// Without configured cache prices, cache tokens bill at the input price, so
	// there is neither a read saving nor a write premium.
	cand := &model.ModelCandidate{InputPrice: 2.0, OutputPrice: 4.0}
	usage := &protocols.IRUsage{PromptTokens: 1_000_000, CacheReadTokens: 500_000, CacheWriteTokens: 500_000}
	cost := computeCost(cand, usage, 0)
	if cost.CacheReadSavedMicros != 0 || cost.CacheWriteExtraMicros != 0 {
		t.Errorf("expected 0/0 savings without cache prices, got read=%d write=%d",
			cost.CacheReadSavedMicros, cost.CacheWriteExtraMicros)
	}
}

func TestComputeCostRoundsToMicro(t *testing.T) {
	// Micros are the smallest stored unit (CNY × 1e6). 1 token @ 1.5/M =
	// 0.0000015 CNY = 1.5 micros -> rounds to 2 micros.
	cand := &model.ModelCandidate{InputPrice: 1.5, OutputPrice: 0}
	usage := &protocols.IRUsage{PromptTokens: 1, CompletionTokens: 0}
	cost := computeCost(cand, usage, 0)
	if !cost.Known || cost.CostMicros != 2 {
		t.Fatalf("expected known 2 micros, got %d (known=%v)", cost.CostMicros, cost.Known)
	}
}

func TestComputeCostMissingUsageIsUnknown(t *testing.T) {
	// Missing usage must be "unknown", never 0 cost.
	cand := &model.ModelCandidate{InputPrice: 1.0, OutputPrice: 1.0}
	if cost := computeCost(cand, nil, 0); cost.Known || cost.CostMicros != 0 {
		t.Fatalf("expected unknown/0 for nil usage, got %d (known=%v)", cost.CostMicros, cost.Known)
	}
}

func TestComputeCostMissingCandidateIsUnknown(t *testing.T) {
	usage := &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 100}
	if cost := computeCost(nil, usage, 0); cost.Known || cost.CostMicros != 0 {
		t.Fatalf("expected unknown/0 for nil candidate, got %d (known=%v)", cost.CostMicros, cost.Known)
	}
}

// TestFinalizeWritesBodyRow: finalize
// persists the four body fields (stored verbatim; v0.1 does not scrub body
// content) into request_log_bodies, keyed by request_id, alongside the
// request_logs row.
func TestFinalizeWritesBodyRow(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, nil)

	rc := &Exchange{
		requestID:      "req-body-1",
		apiKeyID:       apiKey.ID,
		requestHeaders: []byte(`{"User-Agent":["curl/8.0"]}`),
	}
	rc.bodies.SetRequest([]byte(`{"model":"gpt-4o"}`))
	rc.bodies.SetUpstreamRequest([]byte(`{"model":"gpt-4o-real"}`))
	rc.bodies.SetResponse([]byte(`{"model":"gpt-4o","choices":[]}`))
	rc.bodies.SetUpstreamResponse([]byte(`{"model":"gpt-4o-real","choices":[]}`))

	svc.finalize(rc, nil, 200, "", time.Now())

	svc.recordTerminal(rc)

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
	if body.RequestHeaders != `{"User-Agent":["curl/8.0"]}` {
		t.Errorf("RequestHeaders = %q, want the captured (masked) headers", body.RequestHeaders)
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

// TestFinalizeBodyWriteFailureDoesNotRollbackBilling: the
// request_log_bodies write is best-effort — a failure there must not roll
// back or block the request_logs row (billing/audit trail), which is written
// first and is authoritative.
func TestFinalizeBodyWriteFailureDoesNotRollbackBilling(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, nil)

	// Force UpsertRequestLogBody to fail without touching request_logs, so
	// the assertion below proves the body write's failure never rolled back
	// (or panicked around) the earlier billing/log write.
	if err := db.Exec("DROP TABLE request_log_bodies").Error; err != nil {
		t.Fatalf("drop request_log_bodies: %v", err)
	}

	rc := &Exchange{requestID: "req-body-fail-1", apiKeyID: apiKey.ID}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("finalize panicked on request_log_bodies write failure: %v", r)
		}
	}()
	svc.finalize(rc, nil, 200, "", time.Now())
	svc.recordTerminal(rc)

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

// TestComputeCostCompressSavings verifies the ESTIMATED compress saving is
// priced at the candidate's input rate. 1500 tokens saved @ 2.5 CNY/M =
// 1500 * 2.5 = 3750 micros (the /1e6 from per-M pricing cancels the ×1e6
// to micros). Reported only — CostMicros itself is unaffected.
func TestComputeCostCompressSavings(t *testing.T) {
	cand := &model.ModelCandidate{InputPrice: 2.5, OutputPrice: 5.0}
	usage := &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 50}
	cost := computeCost(cand, usage, 1500)
	if !cost.Known {
		t.Fatal("expected cost to be known when usage + candidate present")
	}
	if cost.CompressCostSavedMicros != 3750 {
		t.Errorf("compress cost saved = %d, want 3750 micros", cost.CompressCostSavedMicros)
	}
	// Sanity: the billed CostMicros is unaffected by the compress savings line.
	bare := computeCost(cand, usage, 0)
	if cost.CostMicros != bare.CostMicros {
		t.Errorf("compress savings leaked into CostMicros: with=%d without=%d",
			cost.CostMicros, bare.CostMicros)
	}
}

// TestComputeCostCompressSavingsRoundingFractional: a fractional input price
// must round to the nearest micro (matching the CostMicros rounding policy).
// 1 token @ 1.5/M = 1.5 micros -> rounds to 2.
func TestComputeCostCompressSavingsRoundingFractional(t *testing.T) {
	cand := &model.ModelCandidate{InputPrice: 1.5, OutputPrice: 0}
	usage := &protocols.IRUsage{PromptTokens: 1, CompletionTokens: 0}
	cost := computeCost(cand, usage, 1)
	if cost.CompressCostSavedMicros != 2 {
		t.Errorf("compress cost saved = %d, want 2 micros after rounding", cost.CompressCostSavedMicros)
	}
}

// TestComputeCostCompressSavingsUnknownIsZero: when usage is nil (pricing/
// usage unknown), cost_saved must be 0 even if tokens_saved > 0 — matching
// the CostKnown=false semantics so an unknown row never reports a saving.
func TestComputeCostCompressSavingsUnknownIsZero(t *testing.T) {
	cand := &model.ModelCandidate{InputPrice: 2.0, OutputPrice: 4.0}
	if cost := computeCost(cand, nil, 1000); cost.Known || cost.CompressCostSavedMicros != 0 {
		t.Fatalf("expected unknown/0 compress savings for nil usage, got %d (known=%v)",
			cost.CompressCostSavedMicros, cost.Known)
	}
	if cost := computeCost(nil, &protocols.IRUsage{PromptTokens: 1}, 1000); cost.Known || cost.CompressCostSavedMicros != 0 {
		t.Fatalf("expected unknown/0 compress savings for nil candidate, got %d (known=%v)",
			cost.CompressCostSavedMicros, cost.Known)
	}
}

// TestFinalizeWritesCompressColumns: when compression ran, the four compress
// columns on request_logs and compressed_request_body on request_log_bodies
// are populated. Cost_saved is the tokens-saved × input-price math verified
// in TestComputeCostCompressSavings; here we verify it persisted end-to-end.
func TestFinalizeWritesCompressColumns(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, nil)

	// Cand InputPrice 2.0/M, tokensSaved 1500 -> 1500 * 2.0 = 3000 micros.
	// the attempt state candidate is only read in-memory by computeCost (InputPrice), never
	// persisted by finalize — so an unpersisted in-memory candidate is enough.
	cand := &model.ModelCandidate{
		InputPrice: 2.0, OutputPrice: 4.0, MaxOutput: 128,
		SupportsStreaming: boolPtr(true), ManagementStatus: model.ModelCandidateStatusEnabled,
		VerificationStatus: model.ModelVerificationStatusPassed,
	}
	rc := &Exchange{
		requestID: "req-compress-1",
		apiKeyID:  apiKey.ID,
		// One successful attempt — the request reached upstream, so the
		// compress savings are real (not phantom) and must be persisted.
		attempts: []AttemptRecord{{
			CandidateID: cand.ID, ProviderID: 1, ProviderName: "test",
			Outcome: AttemptSuccess, StatusCode: 200,
		}},
	}
	rc.attempt.BeginCandidate(cand)
	seedCompressionSaved(rc, 1500, "whitespace", "whitespace", "contractions")
	rc.bodies.SetCompressedRequest([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.finalize(rc, &fact.UsageReported{Unit: fact.UnitToken, Prompt: 100, Completion: 50, Total: 150}, 200, "", time.Now())
	svc.recordTerminal(rc)

	row, err := repository.GetRequestLogByRequestID(db, "req-compress-1")
	if err != nil || row == nil {
		t.Fatalf("GetRequestLogByRequestID: %v (row=%v)", err, row)
	}
	if row.CompressEstimatedTokensSaved != 1500 {
		t.Errorf("CompressEstimatedTokensSaved = %d, want 1500", row.CompressEstimatedTokensSaved)
	}
	if row.CompressEstimatedCostSavedMicros != 3000 {
		t.Errorf("CompressEstimatedCostSavedMicros = %d, want 3000", row.CompressEstimatedCostSavedMicros)
	}
	if row.CompressSkipReason != "" {
		t.Errorf("CompressSkipReason = %q, want empty (compression ran)", row.CompressSkipReason)
	}
	// Per-block occurrences preserved: two "whitespace" entries are both kept
	// so downstream stats can count how many times each compressor fired.
	if row.CompressorsApplied != "whitespace,whitespace,contractions" {
		t.Errorf("CompressorsApplied = %q, want %q", row.CompressorsApplied, "whitespace,whitespace,contractions")
	}
	if !row.CostKnown {
		t.Errorf("CostKnown = false, want true (usage + candidate present)")
	}

	body, err := repository.GetRequestLogBodyByRequestID(db, "req-compress-1")
	if err != nil || body == nil {
		t.Fatalf("GetRequestLogBodyByRequestID: %v (body=%v)", err, body)
	}
	if body.CompressedRequestBody != `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}` {
		t.Errorf("CompressedRequestBody = %q, want the post-compression bytes", body.CompressedRequestBody)
	}
}

// TestFinalizeWritesCompressSkippedColumns: when compression was enabled but
// skipped, skip_reason is populated and the savings/body fields are zero/empty.
func TestFinalizeWritesCompressSkippedColumns(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, nil)

	rc := &Exchange{
		requestID: "req-compress-skip-1",
		apiKeyID:  apiKey.ID,
	}
	seedCompressionSkipped(rc, "too_small")
	svc.finalize(rc, nil, 200, "", time.Now())
	svc.recordTerminal(rc)

	row, err := repository.GetRequestLogByRequestID(db, "req-compress-skip-1")
	if err != nil || row == nil {
		t.Fatalf("GetRequestLogByRequestID: %v (row=%v)", err, row)
	}
	if row.CompressEstimatedTokensSaved != 0 {
		t.Errorf("CompressEstimatedTokensSaved = %d, want 0 (skipped)", row.CompressEstimatedTokensSaved)
	}
	if row.CompressEstimatedCostSavedMicros != 0 {
		t.Errorf("CompressEstimatedCostSavedMicros = %d, want 0 (skipped)", row.CompressEstimatedCostSavedMicros)
	}
	if row.CompressSkipReason != "too_small" {
		t.Errorf("CompressSkipReason = %q, want %q", row.CompressSkipReason, "too_small")
	}
	if row.CompressorsApplied != "" {
		t.Errorf("CompressorsApplied = %q, want empty (skipped)", row.CompressorsApplied)
	}

	body, err := repository.GetRequestLogBodyByRequestID(db, "req-compress-skip-1")
	if err != nil || body == nil {
		t.Fatalf("GetRequestLogBodyByRequestID: %v (body=%v)", err, body)
	}
	if body.CompressedRequestBody != "" {
		t.Errorf("CompressedRequestBody = %q, want empty (skipped)", body.CompressedRequestBody)
	}
}

// TestFinalizeWritesCompressColumnsUncompressed: when compression never ran
// (CompressEnabled false — fields all zero-value), the four compress columns
// and compressed_request_body stay at their zero/empty defaults.
func TestFinalizeWritesCompressColumnsUncompressed(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, nil)

	rc := &Exchange{requestID: "req-nocompress-1", apiKeyID: apiKey.ID}
	svc.finalize(rc, nil, 200, "", time.Now())
	svc.recordTerminal(rc)

	row, err := repository.GetRequestLogByRequestID(db, "req-nocompress-1")
	if err != nil || row == nil {
		t.Fatalf("GetRequestLogByRequestID: %v (row=%v)", err, row)
	}
	if row.CompressEstimatedTokensSaved != 0 || row.CompressEstimatedCostSavedMicros != 0 {
		t.Errorf("compress savings not zero for an uncompressed request: tokens=%d cost=%d",
			row.CompressEstimatedTokensSaved, row.CompressEstimatedCostSavedMicros)
	}
	if row.CompressSkipReason != "" || row.CompressorsApplied != "" {
		t.Errorf("compress reason/list not empty for an uncompressed request: reason=%q list=%q",
			row.CompressSkipReason, row.CompressorsApplied)
	}

	body, err := repository.GetRequestLogBodyByRequestID(db, "req-nocompress-1")
	if err != nil || body == nil {
		t.Fatalf("GetRequestLogBodyByRequestID: %v (body=%v)", err, body)
	}
	if body.CompressedRequestBody != "" {
		t.Errorf("CompressedRequestBody = %q, want empty for uncompressed request", body.CompressedRequestBody)
	}
}

// TestFinalizeCompressCostSavedZeroWhenPricingUnknown: a request that saved
// tokens but had no usage (early failure before any attempt) must persist
// cost_saved=0 even though tokens_saved may be non-zero. This is the case the
// dashboard cares about: never report a saving on a row whose cost is unknown.
func TestFinalizeCompressCostSavedZeroWhenPricingUnknown(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, nil)

	rc := &Exchange{
		requestID: "req-unknown-price-1",
		apiKeyID:  apiKey.ID,
		// Usage and Candidate are nil — pricing is unknown, and crucially
		// Attempts is empty (pre-relay rejection).
	}
	seedCompressionSaved(rc, 5000, "whitespace") // large, but no usage to price it
	rc.bodies.SetCompressedRequest([]byte(`{"model":"gpt-4o"}`))
	svc.finalize(rc, nil, http.StatusInternalServerError, "no_candidate", time.Now())
	svc.recordTerminal(rc)

	row, err := repository.GetRequestLogByRequestID(db, "req-unknown-price-1")
	if err != nil || row == nil {
		t.Fatalf("GetRequestLogByRequestID: %v (row=%v)", err, row)
	}
	if row.CostKnown {
		t.Errorf("CostKnown = true, want false (no usage to price)")
	}
	if row.CompressEstimatedCostSavedMicros != 0 {
		t.Errorf("CompressEstimatedCostSavedMicros = %d, want 0 when pricing is unknown",
			row.CompressEstimatedCostSavedMicros)
	}
	// tokens_saved is zeroed too: the request was rejected pre-relay so no
	// upstream call was made — persisting savings would inflate the compress
	// rate / savings metrics.
	if row.CompressEstimatedTokensSaved != 0 {
		t.Errorf("CompressEstimatedTokensSaved = %d, want 0 (pre-relay rejection)",
			row.CompressEstimatedTokensSaved)
	}
	if row.CompressorsApplied != "" {
		t.Errorf("CompressorsApplied = %q, want empty (pre-relay rejection)",
			row.CompressorsApplied)
	}
}

// TestFinalizeCompressPhantomSavingsZeroedOnPreRelayRejection: compression
// modified the body and produced savings, but the request was then rejected
// before any upstream attempt (no routable candidate, model disabled, etc.).
// The compress savings fields MUST be zeroed so the stats endpoint does not
// count phantom savings. CompressSkipReason is kept (audit-only). The
// compressed body on request_log_bodies is still persisted (for debugging).
func TestFinalizeCompressPhantomSavingsZeroedOnPreRelayRejection(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, nil)

	rc := &Exchange{
		requestID: "req-phantom-1",
		apiKeyID:  apiKey.ID,
		// No Attempts — simulates a pre-relay rejection (e.g. no routable
		// candidate after compression already ran on the caller body).
	}
	seedCompressionSaved(rc, 1500, "whitespace", "log")
	rc.bodies.SetCompressedRequest([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	// 503 = no routable candidate, the most common post-compress pre-relay path.
	svc.finalize(rc, nil, http.StatusServiceUnavailable, "no_enabled_candidate", time.Now())
	svc.recordTerminal(rc)

	row, err := repository.GetRequestLogByRequestID(db, "req-phantom-1")
	if err != nil || row == nil {
		t.Fatalf("GetRequestLogByRequestID: %v (row=%v)", err, row)
	}
	if row.CompressEstimatedTokensSaved != 0 {
		t.Errorf("CompressEstimatedTokensSaved = %d, want 0 (phantom savings zeroed)",
			row.CompressEstimatedTokensSaved)
	}
	if row.CompressEstimatedCostSavedMicros != 0 {
		t.Errorf("CompressEstimatedCostSavedMicros = %d, want 0 (phantom savings zeroed)",
			row.CompressEstimatedCostSavedMicros)
	}
	if row.CompressorsApplied != "" {
		t.Errorf("CompressorsApplied = %q, want empty (phantom savings zeroed)",
			row.CompressorsApplied)
	}
	// CompressSkipReason stays as-is — it's audit-only.
	// (empty here because compression ran, but the zeroing logic does NOT touch it)

	// The compressed body is still persisted on request_log_bodies for debugging.
	body, err := repository.GetRequestLogBodyByRequestID(db, "req-phantom-1")
	if err != nil || body == nil {
		t.Fatalf("GetRequestLogBodyByRequestID: %v (body=%v)", err, body)
	}
	if body.CompressedRequestBody == "" {
		t.Errorf("CompressedRequestBody should still be persisted for debugging even on pre-relay rejection")
	}
}

// TestJoinCompressors covers the comma-join helper directly: per-block
// occurrences are preserved (NOT deduped) so stats can count how many times
// each compressor fired, order is preserved, and empty input yields "".

func TestNetPromptTokens(t *testing.T) {
	cases := []struct {
		name  string
		usage *protocols.IRUsage
		want  int
	}{
		{"nil usage", nil, 0},
		{
			// Anthropic: input_tokens is already net; flag false -> unchanged.
			"claude net input passes through",
			&protocols.IRUsage{PromptTokens: 237, CacheReadTokens: 17152, CacheIncludedInPrompt: false},
			237,
		},
		{
			// OpenAI: prompt_tokens includes cache; flag true -> subtract cache.
			"openai gross input has cache subtracted",
			&protocols.IRUsage{PromptTokens: 17389, CacheReadTokens: 17152, CacheIncludedInPrompt: true},
			237, // 17389 - 17152
		},
		{
			// Under the INCLUSIVE convention the prompt total contains both
			// cache lines, so both come out. This case used to expect 400, on
			// the premise that no protocol counts a cache write inside the
			// prompt — true while the only cache-write source was Anthropic,
			// whose input_tokens is net (the case above still covers that).
			//
			// It stopped being true once the gateway started emitting and
			// accepting the cache_creation_input_tokens alias on OpenAI-shaped
			// wires, where the gross prompt is net + read + write (the
			// convention new-api uses). Leaving the write in would count those
			// tokens twice: once on the cache-write line, once as fresh input.
			"inclusive prompt has both cache lines subtracted",
			&protocols.IRUsage{PromptTokens: 1000, CacheReadTokens: 600, CacheWriteTokens: 300, CacheIncludedInPrompt: true},
			100, // 1000 - 600 - 300
		},
		{
			// Anthropic reports a net input_tokens alongside a cache write, so
			// neither count is subtracted.
			"claude net input with cache write passes through",
			&protocols.IRUsage{PromptTokens: 237, CacheWriteTokens: 4096, CacheIncludedInPrompt: false},
			237,
		},
		{
			// DeepSeek splits prompt_tokens into hit + miss; the hit half decodes
			// as cache read, leaving the miss half as net input.
			"deepseek hit subtracted leaving miss",
			&protocols.IRUsage{PromptTokens: 1000, CacheReadTokens: 600, CacheIncludedInPrompt: true},
			400, // the prompt_cache_miss_tokens half
		},
		{
			// Defensive: upstream reporting cache > prompt floors at 0.
			"cache exceeding prompt floors at zero",
			&protocols.IRUsage{PromptTokens: 100, CacheReadTokens: 500, CacheIncludedInPrompt: true},
			0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := netPromptTokens(tc.usage); got != tc.want {
				t.Errorf("netPromptTokens(%+v) = %d, want %d", tc.usage, got, tc.want)
			}
		})
	}
}

// TestComputeCostRejectsIncoherentUsage: token counts come off the wire from
// third-party upstreams, so a buggy or hostile provider can report negatives, or
// a cache read larger than the inclusive prompt it was read from. Both are
// physically impossible and must be treated as unknown usage — Known=false
// keeps the cost off the bill and, because finalize gates budget accumulation
// on Known, off the API key's budget too.
//
// Note on what is NOT here: "parts exceed stated total" is deliberately absent.
// A stated total that the parts overflow is "attribution unknown", not
// impossible — the tokens were really consumed. Every gateway surveyed
// (new-api, litellm, llmgateway) recomputes total = prompt + completion and
// bills on; this gateway follows that convention. See
// TestComputeCostAcceptsContradictoryTotal for the positive case.
func TestComputeCostRejectsIncoherentUsage(t *testing.T) {
	readPrice := 0.02
	cand := &model.ModelCandidate{InputPrice: 1.0, OutputPrice: 2.0, CacheReadPrice: &readPrice}

	cases := []struct {
		name  string
		usage *protocols.IRUsage
	}{
		{
			// Would have billed 500 cache reads on a 100-token prompt. This is the
			// sole bound on the cache count, so it holds for any record that
			// normalizeCacheConvention has not reclassified — see the end of this
			// test for the reclassified counterpart.
			"cache read exceeds inclusive prompt",
			&protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, CacheReadTokens: 500, CacheIncludedInPrompt: true},
		},
		{
			// A negative cache count inflates net input AND produces a negative
			// cache line item, so the total can come out below zero.
			"negative cache read",
			&protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, CacheReadTokens: -1000, CacheIncludedInPrompt: true},
		},
		{"negative prompt", &protocols.IRUsage{PromptTokens: -5, CompletionTokens: 10}},
		{"negative completion", &protocols.IRUsage{PromptTokens: 100, CompletionTokens: -10}},
		{"negative cache write", &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, CacheWriteTokens: -1}},
		{"negative total", &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: -1}},
		{
			// Regression: a negative reasoning count must reach the verdict.
			// It arrives on IRUsage.ReasoningTokens and must survive to here,
			// so the wire encoder (which refuses via HasNegativeCount) and this
			// billing gate agree on rejecting it.
			"negative reasoning tokens",
			&protocols.IRUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, ReasoningTokens: -10},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeCost(cand, tc.usage, 0)
			if got.Known {
				t.Errorf("Known = true, want false for incoherent usage %+v", tc.usage)
			}
			if got.CostMicros != 0 {
				t.Errorf("CostMicros = %d, want 0", got.CostMicros)
			}
		})
	}

	// A cache read equal to the whole prompt is legitimate (a fully cached
	// prompt) and must still bill.
	full := &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, CacheReadTokens: 100, CacheIncludedInPrompt: true}
	if got := computeCost(cand, full, 0); !got.Known {
		t.Error("a fully-cached prompt must remain billable")
	}
	// Anthropic reports an independent net input, so a cache read larger than
	// it is normal rather than incoherent.
	netInput := &protocols.IRUsage{PromptTokens: 237, CompletionTokens: 10, CacheReadTokens: 17152, CacheIncludedInPrompt: false}
	if got := computeCost(cand, netInput, 0); !got.Known {
		t.Error("cache read may exceed a net (non-inclusive) input")
	}
	// The cache bound above is a bound on the INCLUSIVE convention only, so it
	// must not survive normalizeCacheConvention reclassifying a record: a stated
	// total of 610 corroborates the net reading (100+10+500), after which the
	// prompt is billed as net input rather than floored to zero by a subtraction
	// that no longer applies.
	exclusive := &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 610, CacheReadTokens: 500, CacheIncludedInPrompt: true}
	normalizeCacheConvention(exclusive)
	if got := computeCost(cand, exclusive, 0); !got.Known {
		t.Error("a net-prompt upstream must bill once its convention is normalized")
	}
	if n := netPromptTokens(exclusive); n != 100 {
		t.Errorf("netPromptTokens after normalization = %d, want 100", n)
	}
	// An upstream that omits total_tokens leaves it at 0; the bound only
	// applies when a total was actually stated.
	noTotal := &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10}
	if got := computeCost(cand, noTotal, 0); !got.Known {
		t.Error("a missing total must not make usage incoherent")
	}
	// Exactly-equal parts and total is the normal OpenAI shape.
	exact := &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}
	if got := computeCost(cand, exact, 0); !got.Known {
		t.Error("prompt+completion == total is the normal case")
	}
	// A contradictory total (parts overflow it) is NOT rejected: the tokens were
	// really consumed, just not split between prompt/completion, and every gateway
	// surveyed recomputes the total as the parts-sum and bills on. This locks the
	// industry-convention behavior decided after surveying new-api/litellm/llmgateway.
	contradictory := &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 1_000_000, TotalTokens: 110}
	if got := computeCost(cand, contradictory, 0); !got.Known {
		t.Error("a contradictory total must still bill (industry convention: recompute, don't reject)")
	}
}

// TestComputeCostGuardsMicrosOverflow: float64→int64 conversion is undefined
// out of range, so an absurd unit price against a huge-but-coherent token count
// must yield unknown rather than an arbitrary budget charge.
func TestComputeCostGuardsMicrosOverflow(t *testing.T) {
	cand := &model.ModelCandidate{InputPrice: 1e12, OutputPrice: 1e12}
	usage := &protocols.IRUsage{PromptTokens: 1_000_000_000, CompletionTokens: 1_000_000_000, TotalTokens: 2_000_000_000}
	got := computeCost(cand, usage, 0)
	if got.Known {
		t.Errorf("Known = true, want false when micros overflows int64 (got %d)", got.CostMicros)
	}
}

func TestComputeCostUsesNetInputAcrossProtocols(t *testing.T) {
	// A converting upstream (Anthropic ingress) reports net input=237 with
	// cache_read=17152 (flag false); an OpenAI upstream reports the same
	// request as gross prompt=17389 with cache_read=17152 (flag true). Both
	// must bill identically: 237 net @ input price + 17152 @ cache_read price.
	readPrice := 0.02
	cand := &model.ModelCandidate{InputPrice: 1.0, OutputPrice: 2.0, CacheReadPrice: &readPrice}

	anthropic := &protocols.IRUsage{PromptTokens: 237, CompletionTokens: 10, CacheReadTokens: 17152, CacheIncludedInPrompt: false}
	openai := &protocols.IRUsage{PromptTokens: 17389, CompletionTokens: 10, CacheReadTokens: 17152, CacheIncludedInPrompt: true}

	// 237×1.0 + 17152×0.02 + 10×2.0 = 237 + 343.04 + 20 = 600.04 -> 600 micros.
	const wantMicros = 600
	if c := computeCost(cand, anthropic, 0); c.CostMicros != wantMicros {
		t.Errorf("anthropic cost = %d micros, want %d", c.CostMicros, wantMicros)
	}
	if c := computeCost(cand, openai, 0); c.CostMicros != wantMicros {
		t.Errorf("openai cost = %d micros, want %d", c.CostMicros, wantMicros)
	}
}

// TestFinalizeNormalizesCacheExclusivePrompt: an OpenAI-compatible upstream
// fronting an Anthropic model reports the NET input as prompt_tokens and puts
// the cached portion only in prompt_tokens_details, so the cache-read count can
// exceed the prompt. finalize must read that as "this upstream uses the net
// convention" and log the request in full, rather than discarding every count —
// which used to zero the completion and cache columns too, making a perfectly
// successful request look like it consumed nothing.
//
// Asserted through the persisted row (and the key's budget), because that is
// what the operator actually sees.
func TestFinalizeNormalizesCacheExclusivePrompt(t *testing.T) {
	readPrice := 0.02
	cases := []struct {
		name       string
		usage      *protocols.IRUsage
		wantInput  int
		wantOutput int
		wantRead   int
		wantKnown  bool
		wantMicros int64
	}{
		{
			// A whole prompt served from cache, reported behind an OpenAI-shaped
			// usage object: prompt_tokens carries the net 0, and the stated total
			// counts the cache on its own line (0+30+6855) — the net convention's
			// identity, nothing like the inclusive one's (30).
			// 6855×0.02 + 30×2.0 = 137.1 + 60 = 197.1 -> 197 micros.
			name:      "net prompt with cache read is logged in full",
			usage:     &protocols.IRUsage{PromptTokens: 0, CompletionTokens: 30, TotalTokens: 6885, CacheReadTokens: 6855, CacheIncludedInPrompt: true},
			wantInput: 0, wantOutput: 30, wantRead: 6855, wantKnown: true, wantMicros: 197,
		},
		{
			// A non-zero net prompt must survive intact: subtracting the cache
			// and flooring at 0 would silently drop these 100 real input tokens.
			// 100×1.0 + 6855×0.02 + 10×2.0 = 257.1 -> 257 micros.
			name:      "net prompt is not swallowed by the cache read",
			usage:     &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 6965, CacheReadTokens: 6855, CacheIncludedInPrompt: true},
			wantInput: 100, wantOutput: 10, wantRead: 6855, wantKnown: true, wantMicros: 257,
		},
		{
			// The subset rule stands on its own, so slack in the total cannot block
			// it: the stated total falls short of the net identity here because the
			// upstream folded a count into it that the OpenAI usage shape cannot
			// carry (a cache write), yet a 6521-token cache read still cannot be a
			// subset of a zero-token prompt.
			// 6521×0.02 + 30×2.0 = 130.42 + 60 = 190.42 -> 190 micros.
			name:      "the subset rule holds when the total carries slack",
			usage:     &protocols.IRUsage{PromptTokens: 0, CompletionTokens: 30, TotalTokens: 6885, CacheReadTokens: 6521, CacheIncludedInPrompt: true},
			wantInput: 0, wantOutput: 30, wantRead: 6521, wantKnown: true, wantMicros: 190,
		},
		{
			// An upstream that really is inclusive and merely over-reports its
			// cached count states a total that says so (100000+100). Reclassifying
			// on the inequality alone would bill the gross prompt at the input price
			// AND the cache at the cache price — the same tokens charged twice.
			name:  "inclusive upstream over-reporting its cache is not reclassified",
			usage: &protocols.IRUsage{PromptTokens: 100_000, CompletionTokens: 100, TotalTokens: 100_100, CacheReadTokens: 100_001, CacheIncludedInPrompt: true},
		},
		{
			// No stated total means no corroboration, and guessing here can only
			// overcharge — so the record stays inclusive and is rejected.
			name:  "cache exceeding prompt without a stated total is rejected",
			usage: &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, CacheReadTokens: 6855, CacheIncludedInPrompt: true},
		},
		{
			// An absurd cache count cannot buy itself a reclassification: the stated
			// total refutes the net reading, so the cache bound still applies and
			// nothing reaches the bill or the key's budget.
			name:  "an absurd cache count is still rejected",
			usage: &protocols.IRUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CacheReadTokens: 2_000_000_000, CacheIncludedInPrompt: true},
		},
		{
			// The net parts (610) do not fit inside the stated total (400), so the
			// net reading is self-contradictory. Billing 600 tokens against a total
			// the upstream capped at 400 would be worse than recording nothing.
			name:  "net parts that overflow the stated total are rejected",
			usage: &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 400, CacheReadTokens: 500, CacheIncludedInPrompt: true},
		},
		{
			// A cache write belongs to both readings. Here the stated total is
			// exactly the inclusive account (100+10+1000), and the net parts (1211)
			// do not fit — counting the write on the net side only would have tipped
			// this the other way and double-billed the gross prompt.
			name:  "a cache write does not tip an inclusive upstream into net",
			usage: &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 1110, CacheReadTokens: 101, CacheWriteTokens: 1000, CacheIncludedInPrompt: true},
		},
		{
			// A count near MaxInt wraps the net sum negative, and a wrapped sum
			// compares as though it fits inside any total.
			name:  "a count large enough to overflow the sum is rejected",
			usage: &protocols.IRUsage{PromptTokens: 1, CompletionTokens: 0, TotalTokens: 2, CacheReadTokens: math.MaxInt, CacheIncludedInPrompt: true},
		},
		{
			// A net-convention upstream reports cache < prompt on any request that
			// is not almost entirely cached, which is the common case rather than
			// the exotic one. The stated total (1000+10+500) still says net, and
			// reading it as inclusive would subtract the cache from a prompt that
			// never contained it. 1000×1.0 + 500×0.02 + 10×2.0 = 1030 micros.
			name:      "net convention is detected when the cache is smaller than the prompt",
			usage:     &protocols.IRUsage{PromptTokens: 1000, CompletionTokens: 10, TotalTokens: 1510, CacheReadTokens: 500, CacheIncludedInPrompt: true},
			wantInput: 1000, wantOutput: 10, wantRead: 500, wantKnown: true, wantMicros: 1030,
		},
		{
			// Cache EQUAL to prompt is ambiguous on its face, and the stated total
			// is what settles it. Here 210 leaves room for the cache on its own
			// line: net. 100×1.0 + 100×0.02 + 10×2.0 = 122 micros.
			name:      "cache equal to prompt reads as net when the total says so",
			usage:     &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 210, CacheReadTokens: 100, CacheIncludedInPrompt: true},
			wantInput: 100, wantOutput: 10, wantRead: 100, wantKnown: true, wantMicros: 122,
		},
		{
			// The same counts as the case above with a total of 110 instead: no
			// room for the cache outside the prompt, so it is an ordinary fully
			// cached prompt and the subtraction stands.
			// 100×0.02 + 10×2.0 = 22 -> 22 micros.
			name:      "fully cached inclusive prompt is unchanged",
			usage:     &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, CacheReadTokens: 100, CacheIncludedInPrompt: true},
			wantInput: 0, wantOutput: 10, wantRead: 100, wantKnown: true, wantMicros: 22,
		},
		{
			// A spec-compliant inclusive upstream states total = prompt + completion,
			// so the net reading never fits — the classification is decided by that
			// identity, not by luck.
			// 237×1.0 + 17152×0.02 + 10×2.0 = 600.04 -> 600 micros.
			name:      "inclusive upstream stating its total is not reclassified",
			usage:     &protocols.IRUsage{PromptTokens: 17389, CompletionTokens: 10, TotalTokens: 17399, CacheReadTokens: 17152, CacheIncludedInPrompt: true},
			wantInput: 237, wantOutput: 10, wantRead: 17152, wantKnown: true, wantMicros: 600,
		},
		{
			// Slack in the stated total is not evidence: the 90 tokens here are
			// unaccounted for under either reading, so they say nothing about where
			// the cache read sits. The cache is still a plausible subset of the
			// prompt and the total does not single it out, so the subtraction
			// stands. 50×1.0 + 50×0.02 + 10×2.0 = 71 micros.
			name:      "unexplained slack in the total is not read as net accounting",
			usage:     &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 200, CacheReadTokens: 50, CacheIncludedInPrompt: true},
			wantInput: 50, wantOutput: 10, wantRead: 50, wantKnown: true, wantMicros: 71,
		},
		{
			// A spec-compliant OpenAI upstream still has its cache subtracted.
			// 237×1.0 + 17152×0.02 + 10×2.0 = 600.04 -> 600 micros.
			name:      "inclusive prompt still has cache subtracted",
			usage:     &protocols.IRUsage{PromptTokens: 17389, CompletionTokens: 10, CacheReadTokens: 17152, CacheIncludedInPrompt: true},
			wantInput: 237, wantOutput: 10, wantRead: 17152, wantKnown: true, wantMicros: 600,
		},
		{
			// Anthropic's own endpoint: input_tokens is already net. Same request,
			// same bill as the row above.
			name:      "anthropic net input is unchanged",
			usage:     &protocols.IRUsage{PromptTokens: 237, CompletionTokens: 10, CacheReadTokens: 17152, CacheIncludedInPrompt: false},
			wantInput: 237, wantOutput: 10, wantRead: 17152, wantKnown: true, wantMicros: 600,
		},
		{
			// Genuinely corrupt counts are still dropped whole: a negative cache
			// read would produce a negative line item.
			name:  "negative cache read is still rejected",
			usage: &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 10, CacheReadTokens: -1000, CacheIncludedInPrompt: true},
		},
		{
			// A contradictory total (parts overflow it) is NOT rejected: every
			// gateway surveyed recomputes the total as the parts-sum and bills on,
			// and so does this one. The upstream's stated total of 110 is ignored;
			// billing reads the parts directly. 100×1.0 + 1000000×2.0 = 2000100.
			name:      "parts exceeding the stated total still bill (industry convention)",
			usage:     &protocols.IRUsage{PromptTokens: 100, CompletionTokens: 1_000_000, TotalTokens: 110},
			wantInput: 100, wantOutput: 1_000_000, wantRead: 0, wantKnown: true, wantMicros: 2_000_100,
		},
		{
			// Absent usage stays unknown — never a free request.
			name:  "missing usage stays unknown",
			usage: nil,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			svc := newSvc(t, db)
			apiKey := createAPIKey(t, db, model.APIKeyStatusActive, nil)
			cand := &model.ModelCandidate{InputPrice: 1.0, OutputPrice: 2.0, CacheReadPrice: &readPrice}

			reqID := fmt.Sprintf("req-cacheconv-%d", i)
			rc := &Exchange{requestID: reqID, apiKeyID: apiKey.ID}
			rc.attempt.BeginCandidate(cand)
			svc.finalize(rc, usageReportOf(tc.usage), 200, "", time.Now())
			svc.recordTerminal(rc)

			row, err := repository.GetRequestLogByRequestID(db, reqID)
			if err != nil || row == nil {
				t.Fatalf("GetRequestLogByRequestID: %v (row=%v)", err, row)
			}
			if row.InputTokens != tc.wantInput {
				t.Errorf("InputTokens = %d, want %d", row.InputTokens, tc.wantInput)
			}
			if row.OutputTokens != tc.wantOutput {
				t.Errorf("OutputTokens = %d, want %d", row.OutputTokens, tc.wantOutput)
			}
			if row.CacheReadTokens != tc.wantRead {
				t.Errorf("CacheReadTokens = %d, want %d", row.CacheReadTokens, tc.wantRead)
			}
			if row.CostKnown != tc.wantKnown {
				t.Errorf("CostKnown = %v, want %v", row.CostKnown, tc.wantKnown)
			}
			if row.CostMicros != tc.wantMicros {
				t.Errorf("CostMicros = %d, want %d", row.CostMicros, tc.wantMicros)
			}

			// A billed request must reach the key's budget, or a limit set on it
			// would never bite.
			var updated model.APIKey
			if err := db.First(&updated, apiKey.ID).Error; err != nil {
				t.Fatalf("reload api key: %v", err)
			}
			if updated.BudgetSpentMicros != tc.wantMicros {
				t.Errorf("BudgetSpentMicros = %d, want %d", updated.BudgetSpentMicros, tc.wantMicros)
			}
		})
	}
}

// TestReportUsageCarriesEveryCount pins the settle-side bridge: reportUsage
// rebuilds the persisted usage record from the in-memory counts, and a field
// left out here is a field that survived the whole relay only to be dropped in
// the final hop before the timeline. Distinct primes per field so a crossed
// wire cannot cancel out.
func TestReportUsageCarriesEveryCount(t *testing.T) {
	svc := &Service{}
	rc := &Exchange{requestID: "test"}
	usage := &protocols.IRUsage{
		PromptTokens:     101,
		CompletionTokens: 37,
		TotalTokens:      211,
		CacheReadTokens:  17,
		CacheWriteTokens: 19,
		ReasoningTokens:  23,
		WebSearchCount:   29,
	}
	svc.reportUsage(rc, usageReportOf(usage), usage, newExchangeSink(rc))

	var rep *fact.UsageReported
	for _, e := range rc.timeline.All() {
		if u, ok := e.Record.(fact.UsageReported); ok {
			rep = &u
			break
		}
	}
	if rep == nil {
		t.Fatal("no UsageReported on the timeline")
	}
	want := fact.UsageReported{
		Unit:   fact.UnitToken,
		Source: fact.UsageFromUpstream,
		// Stated as a literal, not via netPromptTokens: an expectation computed
		// by the code under test cannot catch that code drifting. The fixture
		// uses the net (non-inclusive) convention, so net prompt = raw prompt.
		Prompt:         101,
		Completion:     37,
		Total:          211,
		CacheRead:      17,
		CacheWrite:     19,
		Reasoning:      23,
		WebSearchCount: 29,
	}
	if *rep != want {
		t.Errorf("UsageReported = %+v, want %+v", *rep, want)
	}
}
