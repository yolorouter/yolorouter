// Package gateway implements the OpenAI-compatible /v1/chat/completions
// relay (PRD §6.5). This is the second auth path — independent of the admin
// session — and routes caller requests through the model's candidate chain
// to an upstream provider, with Key rotation and candidate failover before
// the first streamed byte. See design doc
// .claude/docs/2026-07-20-m5-gateway-design.md.
//
// v0.1 is OpenAI-in / OpenAI-out only (PRD §6.5.10), so there is no IR /
// cross-protocol layer: the request body is forwarded with only the model
// field swapped to the candidate's provider_model_name, and every model
// field in the response is rewritten back to the external name.
package gateway

import (
	"sync"
	"sync/atomic"

	"github.com/yolorouter/yolorouter-ce/internal/model"
)

// RelayContext is one gateway request's lifecycle state. Sharply trimmed vs
// the reference project's RelayContext: no IR/compression/custom-prompt/
// Vision Bridge fields (PRD §6.5.10 drops all of those) — only what v0.1's
// pass-through + Key rotation + failover + logging actually needs.
type RelayContext struct {
	RequestID     string
	OriginalModel string // external model name; every response model field is rewritten to this
	IsStream      bool
	// WantsStreamUsage is true when the caller set
	// stream_options.include_usage=true. Controls whether usage frames
	// collected upstream are forwarded to the caller (PRD §1114: the
	// gateway always requests usage upstream for its own cost accounting,
	// but only forwards it when the caller asked).
	WantsStreamUsage bool
	APIKeyID         uint

	// Current-attempt target (overwritten on each candidate switch).
	Candidate *model.ModelCandidate
	Provider  *model.Provider

	StatusCode int // set by finalize when the log row is written

	// Usage from the successful attempt, if any — drives cost + the log row.
	Usage *Usage

	// Attempts records every candidate try in order (GATE-13).
	Attempts []AttemptRecord

	// FirstByteSent flips true once any byte has been written to the client
	// (GATE-19: after this, no more Key/candidate switching is allowed).
	FirstByteSent bool

	// logWritten guards finalize against double-write: Handle installs a
	// panic-recovery defer that calls finalize if no normal path did, and
	// finalize itself is idempotent via this flag (GATE-13: exactly one row
	// per request, even under panic).
	logWritten atomic.Bool

	mu sync.Mutex // protects FirstByteSent flips from racing the flusher
}

// MarkFirstByteSent flips FirstByteSent true under the lock. Returns whether
// this call was the one that flipped it — the stream path uses that to decide
// whether a mid-stream upstream error can still switch (no) or must be
// surfaced inline (yes).
func (rc *RelayContext) MarkFirstByteSent() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.FirstByteSent {
		return false
	}
	rc.FirstByteSent = true
	return true
}

// AttemptRecord is one candidate try (GATE-13: the log keeps every attempt,
// not just the final one). Outcome is one of the AttemptOutcome* constants.
type AttemptRecord struct {
	CandidateID       uint   `json:"candidate_id"`
	ProviderID        uint   `json:"provider_id"`
	ProviderName      string `json:"provider_name"`
	ProviderModelName string `json:"provider_model_name"`
	KeyID             uint   `json:"key_id"`
	KeyLabel          string `json:"key_label"`
	StatusCode        int    `json:"status_code"`
	Outcome           string `json:"outcome"`
	FailReason        string `json:"fail_reason"`
}

// Attempt outcomes — drive both the log's fail_reason text and the relay
// loop's switch decision.
const (
	AttemptSuccess     = "success"
	AttemptAuthFailed  = "auth_failed"   // 401 from upstream -> rotate Key
	AttemptRateLimited = "rate_limited"  // 429 -> rotate Key
	AttemptConnError   = "conn_error"    // network/timeout -> failover candidate
	AttemptServerError = "server_error"  // 5xx -> failover candidate
	AttemptClientError = "client_error"  // 4xx (non-auth) -> do NOT switch (GATE-11)
	AttemptBadStatus   = "bad_status"    // unmapped non-2xx -> do NOT switch
)

// Usage is the token usage pulled from an OpenAI-compatible response or
// final SSE chunk (PRD §6.5.4/6.5.5). Cache breakdown fields are deferred
// (design doc §3.3 — cache pricing applied later); the totals are enough for
// v0.1 cost math.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
