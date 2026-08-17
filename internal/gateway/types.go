// Package gateway implements the OpenAI-compatible /v1/chat/completions
// relay. This is the second auth path — independent of the admin
// session — and routes caller requests through the model's candidate chain
// to an upstream provider, with Key rotation and candidate failover before
// the first streamed byte.
//
// v0.1 is OpenAI-in / OpenAI-out only, so there is no IR /
// cross-protocol layer: the request body is forwarded with only the model
// field swapped to the candidate's provider_model_name, and every model
// field in the response is rewritten back to the external name.
package gateway

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yolorouter/yolorouter/internal/decision"
	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/gateway/attempt"
	"github.com/yolorouter/yolorouter/internal/gateway/capture"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// Exchange holds per-request relay state for the gateway pass-through,
// key rotation, failover, and request logging.
type Exchange struct {
	requestID     string
	originalModel string // external model name; every response model field is rewritten to this
	isStream      bool
	// ingress is the wire protocol of the caller's request path, computed
	// once in Handle from c.Request.URL.Path. Every gateway call site that
	// already has rc in scope reads this instead of recomputing it.
	ingress protocols.ProtocolID
	// ingressPath is the caller's request path, captured at Handle entry. The
	// custom system prompt injection allowlist keys off it: Gemini's route is
	// a wildcard :modelaction and the ingress protocol falls back to
	// ProtocolOpenAI for non-generateContent actions, so the path alone
	// distinguishes countTokens / embedContent from real chat.
	ingressPath string
	// isChatEndpoint is computed once in Handle from IngressPath. Both the
	// compression gate and the CSP injection gate read this bool instead of
	// recomputing isChatEndpoint(path) independently.
	isChatEndpoint bool
	// settings is every settings-dependent value this request uses — the
	// compression switch, the custom system prompt, the vision-fallback
	// configuration — two-level-resolved once at entry: a per-key override
	// short-circuits the global cached read, and a failed global read keeps
	// the provider's last-known-good. The kernel resolves it because it is
	// configuration, not an observation; what a capability does with it is
	// the capability's own business, and everything a pass produces comes
	// back as a record on the timeline rather than as fields here.
	settings requestSettings
	// authCredential is the caller's presented API key, kept ONLY so a
	// capability's loopback self-call can act as the same caller — it must
	// never reach logs (the header capture is sanitized separately).
	// visionFallbackSubCall marks a request the gateway made to itself
	// (loopback token matched): the capability reads it as its recursion
	// guard.
	authCredential        string
	visionFallbackSubCall bool
	// parentRequestID names the caller request a loopback sub-call works
	// for; empty on normal requests. Captured from the loopback parent
	// header only when the internal token matched.
	parentRequestID string
	// pricingBasis is the per-million rates the pre-dispatch estimate is taken
	// against: the FIRST routable candidate's, fixed once so everything asking
	// that question reads one answer instead of each picking its own candidate.
	//
	// It is deliberately not exposed to capabilities, and the reason is a real
	// unsolved question rather than an oversight. The chain can skip past that
	// candidate or fail over off it, and settlement prices what actually served
	// — so a reservation made from this basis is wrong by the difference
	// between two providers' rates whenever the request does not end on the
	// first one. Cheap-first ordering under-reserves and lets a caller overspend;
	// the reverse refuses requests they could afford.
	//
	// Neither answer is the kernel's to invent: reserving per candidate means
	// releasing and re-taking across failover, and reserving the dearest
	// candidate's rate up front changes who gets refused. Whoever brings the
	// first real reservation decides it, against what the paid deployment
	// actually does, and adds the accessor then.
	pricingBasis PricingView
	apiKeyID     uint
	// userID is the key owner's account id, resolved once from the key at
	// Handle entry so the request-log row can carry ownership without a
	// join at write time.
	userID uint
	// concurrencyLimit / rpmLimit are the caller's allowance, resolved once
	// from the key. Zero means unlimited, which is also what an absent limit
	// means — the distinction has no consumer, so it is not preserved.
	concurrencyLimit int
	rpmLimit         int
	tpmLimit         int

	// requestDeadline is the absolute cutoff for the whole request across all
	// failover candidates (the request_timeout budget). Set once at Handle
	// entry as now + gateway.RequestTimeout; each upstream attempt reads it
	// to derive its own per-attempt cap as min(attempt_timeout,
	// time.Until(requestDeadline)) so a request near its total budget can't
	// start a fresh full-length attempt. Zero before Handle assigns it.
	requestDeadline time.Time

	// attemptsSpent and probesSpent are the request's count-budget ledger,
	// the decision table's BudgetEffect made real: attempts count upstream
	// dispatches, probes count candidates abandoned before anything was
	// sent. The exchange records spending only — the limits live on the
	// service's gateway config, so a zero-valued Exchange starts with a full
	// budget rather than an exhausted one, and no reporter can refresh what
	// was spent. Spending happens in one place (spendBudget), driven by the
	// resolved decision's Budget effect, so what a judgement costs is the
	// table's call rather than each call site's.
	attemptsSpent int
	probesSpent   int

	// circuitGen is the health-record generation the current candidate's
	// provider was admitted under, set when the candidate loop consults the
	// breaker and handed back when a result is booked, so a result that
	// straddled a breaker transition cannot be booked against the wrong era.
	circuitGen uint64

	// requestCtx is the context carrying RequestDeadline, set once at Handle
	// entry. Candidate queries (model/candidate/key GORM reads) and each
	// per-attempt context derive from this, so a stalled DB cannot overrun
	// the total request budget. Without this, the GORM calls used s.db with
	// no deadline and a stuck query could block past RequestDeadline.
	requestCtx context.Context

	// attempt is the current attempt's identity and outcome: candidate,
	// provider, dispatch URL, and the verdict held for the terminal to quote.
	// The attempt package owns every write and the two lifetimes inside the
	// family (the verdict clears before a loop's early exits; the rest resets
	// only when a candidate is actually entered) — see its package comment.
	// The verdict slot names no capability: which verdicts are worth quoting
	// is a property of the decision table, and a second capability wanting
	// that treatment adds a table row, not a field here.
	attempt attempt.State

	statusCode int // set by finalize when the log row is written

	// attempts records every candidate try in order; recordAttempt is the one
	// place it grows. Usage itself is not held here — the successful
	// delivery's usage travels as a finalize parameter.
	attempts []AttemptRecord

	// timeline is the append-only log of everything capabilities reported
	// during this exchange. The kernel owns it: capabilities report through a
	// sink and never hold it, which is what keeps provenance stamping and
	// ordering in one place.
	timeline fact.Timeline

	// outcome is what finalize settled on, held until every release has run so
	// the recorders see a timeline nothing will be appended to.
	outcome        fact.Outcome
	outcomeSettled bool

	// firstByteSent flips true once any byte has been written to the client
	// (after this, no more Key/candidate switching is allowed).
	firstByteSent bool

	// logWritten guards finalize against double-write: Handle installs a
	// panic-recovery defer that calls finalize if no normal path did, and
	// finalize itself is idempotent via this flag (exactly one row
	// per request, even under panic).
	logWritten atomic.Bool

	mu sync.Mutex // protects FirstByteSent flips from racing the flusher

	// bodies is the audit record for the request_log_bodies row: the caller's
	// request, its compressed variant, what was sent upstream, what came
	// back, what the caller received, and the stream capture file. v0.1
	// stores them VERBATIM — body content is not scrubbed (only request
	// headers are masked; see RequestHeaders below). The capture package owns
	// every write, including the stream file's name (capture.StreamFileName,
	// reported back through StreamName); this struct only holds it.
	bodies capture.Bodies
	// requestHeaders is the caller's request headers as a JSON object, with
	// sensitive headers already masked (SanitizeHeaders). This header-name
	// masking is the ONLY redaction v0.1 does — body content is stored
	// verbatim. Captured once at Handle entry so it survives even an early
	// rejection.
	requestHeaders []byte
}

// markFirstByteSent flips firstByteSent true under the lock. Returns whether
// this call was the one that flipped it — the stream path uses that to decide
// whether a mid-stream upstream error can still switch (no) or must be
// surfaced inline (yes).
//
// Unexported because nothing outside this package calls it, and what it sets is
// worth more than most: `Delivered: rc.firstByteSent` is what the admissions are
// released against, so a caller that flipped it would turn off a refund.
//
// This narrows the surface; it does not close it. Capabilities reach an Exchange
// through a bind function they write themselves, and a bind that hands over the
// Exchange itself reaches every exported method on it — which is why Exchange
// exports readers only: the body mutators that once had to stay exported for
// the protocol layer's buffer interface are gone (the relay helpers take a
// small adapter instead), and a gate in internal/gates pins the exported
// method set so it cannot quietly grow back. What keeps a capability honest
// is the narrow view it binds, which is a property of the assembly and not of
// this file.
// spendBudget books the count budget a resolved decision asks for. One spend
// point for every call site keeps the cost of a judgement the table's call: a
// path cannot decide its own price, and nothing ever books a refund.
func (rc *Exchange) spendBudget(b decision.BudgetEffect) {
	switch b {
	case decision.BudgetConsumeAttempt:
		rc.attemptsSpent++
	case decision.BudgetConsumeProbe:
		rc.probesSpent++
	}
}

func (rc *Exchange) markFirstByteSent() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.firstByteSent {
		return false
	}
	rc.firstByteSent = true
	return true
}

// The getters below exist for capabilities, which read exchange state through
// the narrow view each one declares for itself rather than by reaching into
// the struct. They are added as capabilities ask for them, not pre-emptively:
// a getter with no caller is a field that has quietly stayed public.

// APIKeyID identifies the caller's key.
func (rc *Exchange) APIKeyID() uint { return rc.apiKeyID }

// UserID identifies the account that owns the caller's key.
func (rc *Exchange) UserID() uint { return rc.userID }

// ConcurrencyLimit is how many of this key's requests may be in flight at once.
// Zero means unlimited.
func (rc *Exchange) ConcurrencyLimit() int { return rc.concurrencyLimit }

// RPMLimit is how many requests this key may make per minute. Zero means
// unlimited.
func (rc *Exchange) RPMLimit() int { return rc.rpmLimit }

// TPMLimit is how many tokens this key may settle per minute. Zero means
// unlimited.
func (rc *Exchange) TPMLimit() int { return rc.tpmLimit }

// CustomSystemPromptEnabled reports whether a prompt was resolved for this
// request, from either the global setting or a per-key override.
func (rc *Exchange) CustomSystemPromptEnabled() bool { return rc.settings.CustomSystemPromptEnabled }

// CustomSystemPrompt returns the resolved prompt text, empty when none applies.
func (rc *Exchange) CustomSystemPrompt() string { return rc.settings.CustomSystemPrompt }

// IsChatEndpoint reports whether the caller's route is one where a system
// prompt means anything. Computed once from the request path, because the
// answer cannot change mid-exchange and recomputing it invites two call sites
// to disagree.
func (rc *Exchange) IsChatEndpoint() bool { return rc.isChatEndpoint }

// The getters below serve the recorder, which needs the widest view of any
// capability — an audit row is by nature a summary of everything. That width is
// not a failure of the split: it is a list, written in the recorder's own
// package, of exactly what it reads, which is what the previous arrangement
// (any code in the package reaching for any field) could never produce.

// RequestID is the id shared with the access log and the caller's error
// messages, so one identifier locates a request everywhere it was recorded.
func (rc *Exchange) RequestID() string { return rc.requestID }

// OriginalModel is the model name the caller asked for.
func (rc *Exchange) OriginalModel() string { return rc.originalModel }

// CandidateMaxOutput is the most output the candidate now being tried will
// produce, zero when it states no limit or when no candidate is bound yet.
//
// It is a property of the candidate rather than of the request, so it changes
// under failover: the same request can be clamped to one limit on the first
// provider and a different one on the next. Anything reading it must therefore
// read it per attempt, not once.
func (rc *Exchange) CandidateMaxOutput() int {
	cand := rc.attempt.Candidate()
	if cand == nil {
		return 0
	}
	return cand.MaxOutput
}

// IsStream reports whether the caller asked for a streamed response.
func (rc *Exchange) IsStream() bool { return rc.isStream }

// IngressPath is the caller's request path.
func (rc *Exchange) IngressPath() string { return rc.ingressPath }

// UpstreamURL is where the last attempt was dispatched, empty if none was.
func (rc *Exchange) UpstreamURL() string { return rc.attempt.UpstreamURL() }

// ProviderID identifies the provider of the last attempt, nil when no candidate
// was reached.
func (rc *Exchange) ProviderID() *uint {
	p := rc.attempt.Provider()
	if p == nil {
		return nil
	}
	id := p.ID
	return &id
}

// CompressEnabled is the resolved input-compression switch for this request.
func (rc *Exchange) CompressEnabled() bool { return rc.settings.CompressEnabled }

// VisionFallbackModel is the resolved global describe model ("" = feature off).
func (rc *Exchange) VisionFallbackModel() string { return rc.settings.VisionFallbackModel }

// VisionFallbackPrompt is the resolved describe prompt ("" = built-in default).
func (rc *Exchange) VisionFallbackPrompt() string { return rc.settings.VisionFallbackPrompt }

// AuthCredential is the caller's presented API key, for loopback self-calls
// only — never for logging.
func (rc *Exchange) AuthCredential() string { return rc.authCredential }

// IsVisionFallbackSubCall reports whether this request is the gateway calling
// itself (loopback token matched) — the describe capability's recursion guard.
func (rc *Exchange) IsVisionFallbackSubCall() bool { return rc.visionFallbackSubCall }

// CallSource is what the audit row's source column records: who initiated
// this request ("" = the caller, or the vision-fallback marker for a
// loopback sub-call).
func (rc *Exchange) CallSource() string {
	if rc.visionFallbackSubCall {
		return model.RequestLogSourceVisionFallback
	}
	return ""
}

// ParentRequestID names the caller request a loopback sub-call works for
// ("" on normal requests).
func (rc *Exchange) ParentRequestID() string { return rc.parentRequestID }

// IngressProtocol is the wire protocol of the caller's request path.
func (rc *Exchange) IngressProtocol() protocols.ProtocolID { return rc.ingress }

// RequestHeaders is the masked header capture.
func (rc *Exchange) RequestHeaders() []byte { return rc.requestHeaders }

// RequestBody is the caller's body, verbatim.
func (rc *Exchange) RequestBody() []byte { return rc.bodies.Request() }

// CompressedRequestBody is what the ingress rewriters produced, empty when none
// of them acted. Named for compression because that is what fills it today and
// what the audit column it feeds is called; any ingress rewrite lands here.
func (rc *Exchange) CompressedRequestBody() []byte { return rc.bodies.CompressedRequest() }

// UpstreamRequestBody is what the last attempt sent.
func (rc *Exchange) UpstreamRequestBody() []byte { return rc.bodies.UpstreamRequest() }

// ResponseBody is what the caller received.
func (rc *Exchange) ResponseBody() []byte { return rc.bodies.Response() }

// UpstreamResponseBody is what the upstream returned, unaltered.
func (rc *Exchange) UpstreamResponseBody() []byte { return rc.bodies.UpstreamResponse() }

// StreamBodyPath is where a streamed response was captured, empty when the
// response was not streamed or nothing was captured.
func (rc *Exchange) StreamBodyPath() string { return rc.bodies.StreamName() }

// StreamBodyTruncated reports whether the stream capture hit its cap.
func (rc *Exchange) StreamBodyTruncated() bool { return rc.bodies.StreamTruncated() }

// clearResponseBodies drops UpstreamResponseBody/ResponseBody before this
// attempt commits to writing a 2xx response to the client. A prior failed
// candidate may have stashed a non-2xx error body in these fields
// (attemptOne's non-2xx path, "last attempt wins"); without this clear, a
// stale earlier-candidate error body would be persisted as this (successful)
// request's upstream/response body. Only the success path re-populates them
// afterward (or, for a stream request, leaves them empty — the sent SSE is
// captured to the stream capture file instead).
func (rc *Exchange) clearResponseBodies() {
	rc.bodies.ClearResponses()
}

// AttemptRecord is one candidate try (the log keeps every attempt,
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
	// UpstreamURL is the full URL this attempt dispatched to. Empty for
	// attempts that failed before any request was sent (provider missing,
	// negotiate / build / decrypt failures) — they never reached an upstream.
	UpstreamURL string `json:"upstream_url"`
}

// Attempt outcomes — drive both the log's fail_reason text and the relay
// loop's switch decision.
const (
	AttemptSuccess     = "success"
	AttemptAuthFailed  = "auth_failed"  // 401 from upstream -> rotate Key
	AttemptRateLimited = "rate_limited" // 429 -> rotate Key
	AttemptConnError   = "conn_error"   // network/timeout -> failover candidate
	// AttemptServerError/AttemptBadStatus were coined for the two status classes
	// named below and have both outgrown those names: they are now also written
	// for stream cuts, decode failures, disabled providers and undecryptable
	// keys. What separates them depends on where the record comes from — see
	// attemptOutcomeFor for the delivery paths, classifyUpstreamStatus for the
	// status ones. Kept as they are because the attempts table has always shown
	// these two words.
	AttemptServerError = "server_error" // 5xx -> failover candidate
	AttemptClientError = "client_error" // 4xx (non-auth) -> do NOT switch
	AttemptBadStatus   = "bad_status"   // unmapped non-2xx -> do NOT switch
	// AttemptContentFiltered is the one 4xx that DOES switch: the upstream's
	// input inspection refused the payload, which another candidate may not.
	AttemptContentFiltered = "content_filtered"
)

// beginUpstreamAttempt drops whatever the previous send left on the exchange,
// so nothing this attempt did not produce is read as belonging to it.
//
// The boundary is the send, not the candidate: a single candidate rotates
// through its provider's keys, and each rotation is a real request with its own
// response. Tying invalidation to the candidate would leave one key's leftovers
// standing while the next key runs.
//
// The captured bodies move on this boundary. The
// audit row has one field for them and a request may make several attempts, so
// whatever ends up stored is read as belonging to the attempt the row describes.
// Keeping the last body that happened to exist meant a chain whose final
// attempt never reached an upstream still stored an EARLIER provider's error
// response, with nothing in the row saying it came from somewhere else. The
// per-attempt records carry each attempt's own status and error text, so
// storing nothing here loses the raw payload of an already-recorded failure —
// while storing the wrong one invites a diagnosis of the wrong provider.
//
// Keeping every attempt's body would lose nothing at all, and is the better
// answer. It is not this change: bodies live in their own table precisely
// because they are large, so attributing them per attempt is a schema change,
// not a boundary fix.
func (rc *Exchange) beginUpstreamAttempt() {
	rc.bodies.BeginUpstreamAttempt()
	rc.attempt.BeginUpstreamAttempt()
}
