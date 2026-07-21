package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/repository"
	"github.com/yolorouter/yolorouter-ce/pkg/logger"
	"go.uber.org/zap"
)

// generateRequestID returns a fresh 16-hex-char id for one gateway request
// (GATE-08: every failed response carries this so the admin can find the
// row). crypto/rand keeps it unguessable — it's surfaced to the caller, so a
// predictable counter would leak request volume / ordering.
func generateRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback so a rand failure can never break request routing: epoch
		// nanos is still unique enough to find a log row.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// computeCost returns the cost in integer cents and whether the cost is
// "known" (PRD §6.7.5, §6.7.6). Unknown = usage missing (GATE-21) — the row
// records cost_cents=0 with cost_known=false so the dashboard never shows it
// as a free request. Candidate prices are CNY per million tokens (design
// doc §3.3); cache-read/write pricing is deferred to a later module.
func computeCost(cand *model.ModelCandidate, usage *Usage) (cents int64, known bool) {
	if usage == nil || cand == nil {
		return 0, false
	}
	// PRD §6.7.5: cost = (prompt − cache_read) × input_price
	//                   + cache_read × cache_read_price
	//                   + cache_write × cache_write_price
	//                   + completion × output_price
	// cache_read tokens are a subset of prompt_tokens (OpenAI), so subtract
	// them from the input line to avoid double-counting. Candidate prices
	// are CNY per million tokens (design doc §3.3).
	cacheRead := usage.CacheReadTokens
	cacheWrite := usage.CacheWriteTokens
	nonCacheInput := usage.PromptTokens - cacheRead
	if nonCacheInput < 0 {
		nonCacheInput = 0 // defensive: upstream reporting cache_read > prompt
	}
	// Candidate without a configured cache price bills cache tokens at the
	// input price (PRD §6.7.5: "候选未配置缓存价格时，对应缓存 Token 按输入单价计费").
	cacheReadPrice := cand.InputPrice
	if cand.CacheReadPrice != nil {
		cacheReadPrice = *cand.CacheReadPrice
	}
	cacheWritePrice := cand.InputPrice
	if cand.CacheWritePrice != nil {
		cacheWritePrice = *cand.CacheWritePrice
	}
	cost := float64(nonCacheInput)/1_000_000*cand.InputPrice +
		float64(cacheRead)/1_000_000*cacheReadPrice +
		float64(cacheWrite)/1_000_000*cacheWritePrice +
		float64(usage.CompletionTokens)/1_000_000*cand.OutputPrice
	return int64(cost*100 + 0.5), true
}

// safeUpstreamMessage produces the message shown to the caller for a 4xx
// non-auth upstream failure. The upstream body is NOT forwarded verbatim —
// it can echo back parts of the request (including the rewritten model) and,
// for some providers, fragments of credential detail (GATE-07). A bare
// "upstream returned status N" is enough for the caller to act on.
func safeUpstreamMessage(status int) string {
	return fmt.Sprintf("upstream returned status %d", status)
}

// finalize writes the request_logs row and, when cost is known and positive,
// accumulates the spend onto the API key's budget_spent_cents. Called on
// every exit path (success, every failure class) so each gateway request
// produces exactly one row (GATE-13). rc.Candidate/Provider/Usage may be nil
// on early failures (before any candidate was tried); finalize is nil-safe
// for all of them.
//
// Budget accumulation uses the COST from this request, not a re-read of the
// row — two concurrent requests that each compute their own cost and add it
// atomically (repository.IncrementAPIKeyBudgetSpent is a single
// budget_spent_cents = budget_spent_cents + ? UPDATE) cannot lose updates to
// each other.
func (s *RelayService) finalize(rc *RelayContext, statusCode int, failReason string, start time.Time) {
	if rc.logWritten.Swap(true) {
		return // already finalized (e.g. Handle's panic-recovery defer after a normal finalize)
	}
	rc.StatusCode = statusCode
	durationMs := time.Since(start).Milliseconds()
	costCents, costKnown := computeCost(rc.Candidate, rc.Usage)

	var providerID *uint
	if rc.Provider != nil {
		id := rc.Provider.ID
		providerID = &id
	}
	var failPtr *string
	if failReason != "" {
		fr := failReason
		failPtr = &fr
	}
	apiKeyID := rc.APIKeyID
	var inputTokens, outputTokens, cacheWriteTokens, cacheReadTokens int
	if rc.Usage != nil {
		inputTokens = rc.Usage.PromptTokens
		outputTokens = rc.Usage.CompletionTokens
		cacheWriteTokens = rc.Usage.CacheWriteTokens
		cacheReadTokens = rc.Usage.CacheReadTokens
	}

	logRow := &model.RequestLog{
		RequestID:        rc.RequestID,
		APIKeyID:         &apiKeyID,
		ModelName:        rc.OriginalModel,
		ProviderID:       providerID,
		IsStream:         rc.IsStream,
		StatusCode:       statusCode,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		CacheWriteTokens: cacheWriteTokens,
		CacheReadTokens:  cacheReadTokens,
		CostCents:        costCents,
		CostKnown:        costKnown,
		FailReason:       failPtr,
		Attempts:         len(rc.Attempts),
		DurationMs:       durationMs,
	}
	// GATE-13: keep every attempt's order / key label / failure cause, not
	// just the count. Stored as JSON so the §6.8 query page can render it
	// later without a schema change; empty when no attempt ran (pre-check
	// failure before any candidate was tried).
	if len(rc.Attempts) > 0 {
		if detail, mErr := json.Marshal(rc.Attempts); mErr == nil {
			s := string(detail)
			logRow.AttemptsDetail = &s // *string so empty stays SQL NULL, not ''
		}
	}
	if err := repository.CreateRequestLog(s.db, logRow); err != nil {
		logger.Error("gateway: write request log failed",
			zap.String("request_id", rc.RequestID), zap.Error(err))
	}
	if costKnown && costCents > 0 {
		if err := repository.IncrementAPIKeyBudgetSpent(s.db, rc.APIKeyID, costCents); err != nil {
			logger.Error("gateway: increment budget spent failed",
				zap.String("request_id", rc.RequestID), zap.Error(err))
		}
	}

	// PRD §6.8.4/§6.8.6/LOG-06: record obtainable request/response bodies
	// (stored verbatim; v0.1 does not scrub body content — only request
	// headers are masked, via SanitizeHeaders). Idempotent UPSERT (UNIQUE
	// request_id) so retry/double-call never duplicates. Best-effort: a body-
	// write failure is logged only — the billing row (above) is authoritative
	// and must not roll back on a body failure (Codex #5).
	//
	// streamBodyPath is derived here (not stored on rc) rather than kept as a
	// second string field alongside streamBodyCaptured (simplification: the
	// path is always exactly "<request_id>.stream" — see rc.streamBodyCaptured's
	// doc comment in types.go).
	streamBodyPath := ""
	if rc.streamBodyCaptured {
		streamBodyPath = rc.RequestID + ".stream"
	}
	bodyRow := &model.RequestLogBody{
		RequestID:            rc.RequestID,
		RequestHeaders:       string(rc.RequestHeaders),
		RequestBody:          string(rc.RequestBody),
		UpstreamRequestBody:  string(rc.UpstreamRequestBody),
		ResponseBody:         string(rc.ResponseBody),
		UpstreamResponseBody: string(rc.UpstreamResponseBody),
		StreamBodyPath:       streamBodyPath,
		StreamBodyTruncated:  rc.streamBodyTruncated,
	}
	if err := repository.UpsertRequestLogBody(s.db, bodyRow); err != nil {
		logger.Error("gateway: write request log body failed",
			zap.String("request_id", rc.RequestID), zap.Error(err))
	}
}
