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
	cost := float64(usage.PromptTokens)/1_000_000*cand.InputPrice +
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
	var inputTokens, outputTokens int
	if rc.Usage != nil {
		inputTokens = rc.Usage.PromptTokens
		outputTokens = rc.Usage.CompletionTokens
	}

	logRow := &model.RequestLog{
		RequestID:    rc.RequestID,
		APIKeyID:     &apiKeyID,
		ModelName:    rc.OriginalModel,
		ProviderID:   providerID,
		IsStream:     rc.IsStream,
		StatusCode:   statusCode,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		CostCents:    costCents,
		CostKnown:    costKnown,
		FailReason:   failPtr,
		Attempts:     len(rc.Attempts),
		DurationMs:   durationMs,
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
}
