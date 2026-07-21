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
	"os"
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

	// Bodies captured for the request_log_bodies row (PRD §6.8.4, LOG-06).
	// v0.1 stores them VERBATIM — body content is not scrubbed (only request
	// headers are masked; see RequestHeaders below). RequestBody is set as
	// soon as the caller body is read. UpstreamRequestBody is overwritten on
	// each attempt (success => successful attempt; total failure => last
	// attempt). ResponseBody is the caller-FACING response (post-rewrite,
	// post-usage-strip, including local error JSON); UpstreamResponseBody is
	// the raw upstream response (non-stream full / non-2xx error body
	// bounded-read). For stream, the sent SSE is appended to streamBodyFile
	// instead (§4.3) and handleStream clears these two so they stay empty.
	// Nil/empty on early failure or body-read failure.
	RequestBody          []byte
	UpstreamRequestBody  []byte
	ResponseBody         []byte
	UpstreamResponseBody []byte
	// RequestHeaders is the caller's request headers as a JSON object, with
	// sensitive headers already masked (SanitizeHeaders). This header-name
	// masking is the ONLY redaction v0.1 does — body content above is stored
	// verbatim. Captured once at Handle entry so it survives even an early
	// rejection (PRD §6.8.6).
	RequestHeaders []byte

	// streamBodyFile/streamBodyCaptured/streamBodyTruncated are the
	// stream-only counterpart of the four body fields above (Task 5, PRD
	// §6.8.4/§6.8.6): the sent SSE lines are appended to streamBodyFile as
	// they go out instead of being buffered in memory. streamBodyCaptured is
	// true once a capture file was successfully opened for this request —
	// finalize derives the persisted stream_body_path from RequestID
	// (simplification: the path is always exactly "<request_id>.stream", so
	// this field only ever needs to answer "was a file captured?", not carry
	// the string itself). streamBodyTruncated flips true only if the 1GiB
	// anti-OOM backstop was hit (never a silent content cut). Unexported —
	// accessed only from within this package (stream.go/relay.go).
	streamBodyFile      *os.File
	streamBodyCaptured  bool
	streamBodyTruncated bool
	// streamBodyBytesWritten mirrors the capture file's current size so
	// appendStreamBodyLine can check the 1GiB backstop with a plain integer
	// comparison instead of an os.File.Stat() syscall per appended line
	// (code-review efficiency finding: a chat stream can append hundreds of
	// lines, each previously costing its own Stat() call).
	streamBodyBytesWritten int64
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
	AttemptAuthFailed  = "auth_failed"  // 401 from upstream -> rotate Key
	AttemptRateLimited = "rate_limited" // 429 -> rotate Key
	AttemptConnError   = "conn_error"   // network/timeout -> failover candidate
	AttemptServerError = "server_error" // 5xx -> failover candidate
	AttemptClientError = "client_error" // 4xx (non-auth) -> do NOT switch (GATE-11)
	AttemptBadStatus   = "bad_status"   // unmapped non-2xx -> do NOT switch
)

// Usage is the token usage pulled from an OpenAI-compatible response or
// final SSE chunk (PRD §6.5.4/6.5.5). Prompt/Completion/Total are the
// always-present totals; CacheWrite/CacheRead are the prompt-cache counts
// some upstreams report, driving the cache line items in computeCost
// (PRD §6.7.5).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CacheWriteTokens / CacheReadTokens are the prompt-cache counts some
	// upstreams report (OpenAI exposes cache READ via
	// prompt_tokens_details.cached_tokens; Anthropic splits cache writes via
	// cache_creation_input_tokens). They drive the cache line items in
	// computeCost (PRD §6.7.5). Zero when the upstream didn't report them.
	CacheWriteTokens int `json:"cache_write_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
}
