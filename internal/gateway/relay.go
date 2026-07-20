package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/repository"
	"github.com/yolorouter/yolorouter-ce/pkg/crypto"
	"github.com/yolorouter/yolorouter-ce/pkg/logger"
)

// upstreamRequestTimeout caps a single upstream attempt's full duration
// (connect + headers + body read). Applied via context.WithTimeout on the
// per-attempt request context. A streaming response longer than this is cut
// — v0.1's documented limit; a per-stage timeout split (dial/header vs body)
// is deferred. Named for what it actually bounds, not "dial only".
const upstreamRequestTimeout = 120 * time.Second

// maxNonStreamResponseBytes caps a single non-stream upstream response body.
// A buggy or hostile provider can return an arbitrarily large body; without
// this cap io.ReadAll would grow the buffer until OOM before the request
// timeout fires (the response body has no bodylimit guard the way the
// request body does). Mirrors the provider-test client's bound
// (provider_client.go). Read up to N+1 so an overflow is detectable.
const maxNonStreamResponseBytes = 32 * 1024 * 1024 // 32 MiB

// RelayService is the gateway orchestrator. One instance lives for the
// process lifetime (created in router.New); it owns the DB, the master key
// for decrypting provider keys, an upstream HTTP client, and the in-memory
// rate limiter.
type RelayService struct {
	db        *gorm.DB
	masterKey []byte
	client    *UpstreamClient
	limiter   *Limiter
}

// NewRelayService wires the gateway with the already-decoded AES master key
// (the same one provider_service uses to decrypt the keys it now routes to).
func NewRelayService(db *gorm.DB, masterKey []byte) *RelayService {
	return &RelayService{
		db:        db,
		masterKey: masterKey,
		client:    NewUpstreamClient(),
		limiter:   NewLimiter(),
	}
}

// requestIDFor returns the request id the RequestID middleware already
// generated (uuid, set on the gin context + X-Request-Id header), so the
// gateway's error messages and request_logs row share ONE id with the
// access log — not a second unrelated id. Falls back to a fresh hex id only
// if some route mounted RelayService without the RequestID middleware.
func requestIDFor(c *gin.Context) string {
	if id := c.GetString("request_id"); id != "" {
		return id
	}
	return generateRequestID()
}

// Handle is POST /v1/chat/completions. apiKey is the already-authenticated
// caller key (middleware.APIKeyAuth resolved and validated it). The handler
// runs the full PRD §6.5.3 pipeline: pre-checks → model lookup → allowlist →
// validate → candidate chain with Key rotation + failover → response rewrite
// → log. Every exit path writes exactly one request_logs row via finalize.
func (s *RelayService) Handle(c *gin.Context, apiKey *model.APIKey) {
	start := time.Now()
	rc := &RelayContext{
		RequestID: requestIDFor(c),
		APIKeyID:  apiKey.ID,
	}
	// Panic-recovery safety net for GATE-13: if any sub-call panics (nil
	// deref, index OOB, type assertion), gin's Recovery middleware catches
	// it upstream, but finalize would otherwise never run and the request
	// would leave no audit/cost row. finalize is idempotent (logWritten
	// guard), so a normal-exit finalize first + this defer on panic writes
	// exactly one row either way.
	defer func() {
		if !rc.logWritten.Load() {
			s.finalize(rc, http.StatusInternalServerError, "panic_recovered", start)
		}
	}()

	if !s.checkKeyStateAndLimits(c, rc, apiKey, start) {
		return
	}
	// Concurrency is the only limit that needs a paired release — acquire it
	// here and defer the release so every return path below frees the slot.
	if apiKey.ConcurrencyLimit != nil && *apiKey.ConcurrencyLimit > 0 {
		if !s.limiter.AcquireConcurrency(apiKey.ID, *apiKey.ConcurrencyLimit) {
			s.finalize(rc, http.StatusTooManyRequests, "concurrency_limit", start)
			WriteOpenAIErrorWithRequestID(c, http.StatusTooManyRequests, errTypeRateLimit, "concurrency limit exceeded", rc.RequestID)
			return
		}
		defer s.limiter.ReleaseConcurrency(apiKey.ID)
	}
	// RPM is checked AFTER concurrency so a concurrency-rejected request does
	// NOT also burn an RPM token (the previous order — RPM in
	// checkKeyStateAndLimits before concurrency — let one served request
	// exhaust the whole minute's RPM under concurrent load).
	if apiKey.RPMLimit != nil && *apiKey.RPMLimit > 0 {
		if !s.limiter.CheckRPM(apiKey.ID, *apiKey.RPMLimit, time.Now()) {
			s.finalize(rc, http.StatusTooManyRequests, "rpm_exceeded", start)
			WriteOpenAIErrorWithRequestID(c, http.StatusTooManyRequests, errTypeRateLimit, "rate limit exceeded (requests per minute)", rc.RequestID)
			return
		}
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		// Caller disconnect during body upload is terminal 499 (mirrors the
		// stream/non-stream response paths), not a malformed-request 400.
		if errors.Is(c.Request.Context().Err(), context.Canceled) {
			s.finalize(rc, 499, "client_disconnected", start)
			return // caller is gone; no response to write
		}
		// http.MaxBytesReader (BodySizeLimit middleware) rejects an oversized
		// body with *http.MaxBytesError — surface that as 413 (OpenAI
		// convention) so SDK clients can shrink and retry, instead of 400.
		status := http.StatusBadRequest
		message := "failed to read request body"
		reason := "read_body: " + err.Error()
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
			message = "request body exceeds the size limit"
			reason = "body_too_large"
		}
		s.finalize(rc, status, reason, start)
		WriteOpenAIErrorWithRequestID(c, status, errTypeInvalidRequest, message, rc.RequestID)
		return
	}

	// One JSON decode of the caller body — parsed.Model/Stream for routing,
	// parsed.validate() for structural checks, parsed.hasTools() for the
	// capability filter. The body itself (in `body`) is forwarded untouched
	// by RewriteRequestModel.
	parsed, err := parseRequest(body)
	if err != nil {
		s.finalize(rc, http.StatusBadRequest, "parse: "+err.Error(), start)
		WriteOpenAIErrorWithRequestID(c, http.StatusBadRequest, errTypeInvalidRequest, "invalid request body", rc.RequestID)
		return
	}
	if parsed.Model == "" {
		s.finalize(rc, http.StatusBadRequest, "empty_model", start)
		WriteOpenAIErrorWithRequestID(c, http.StatusBadRequest, errTypeInvalidRequest, "model is required", rc.RequestID)
		return
	}
	rc.OriginalModel = parsed.Model
	rc.IsStream = parsed.Stream
	rc.WantsStreamUsage = parsed.WantsStreamUsage

	// Step 4: model exists and is enabled (PRD §6.5.3). A model disabled via
	// MOD-01 must not route even if its candidates are still enabled.
	m, err := repository.FindModelByName(s.db, parsed.Model)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			s.finalize(rc, http.StatusNotFound, "model_not_found", start)
			WriteOpenAIErrorWithRequestID(c, http.StatusNotFound, errTypeNotFound, "model does not exist", rc.RequestID)
			return
		}
		logger.Error("gateway: find model", zap.String("request_id", rc.RequestID), zap.Error(err))
		s.finalize(rc, http.StatusInternalServerError, "db_model: "+err.Error(), start)
		WriteOpenAIErrorWithRequestID(c, http.StatusInternalServerError, errTypeServer, "internal error", rc.RequestID)
		return
	}
	if m.ManagementStatus != model.ModelStatusEnabled {
		s.finalize(rc, http.StatusNotFound, "model_disabled", start)
		WriteOpenAIErrorWithRequestID(c, http.StatusNotFound, errTypeNotFound, "model does not exist", rc.RequestID)
		return
	}

	// Step 5: allowlist.
	allowed, err := repository.HasAPIKeyModelAccess(s.db, apiKey.ID, m.ID)
	if err != nil {
		logger.Error("gateway: allowlist", zap.String("request_id", rc.RequestID), zap.Error(err))
		s.finalize(rc, http.StatusInternalServerError, "db_allowlist: "+err.Error(), start)
		WriteOpenAIErrorWithRequestID(c, http.StatusInternalServerError, errTypeServer, "internal error", rc.RequestID)
		return
	}
	if !allowed {
		s.finalize(rc, http.StatusForbidden, "model_not_allowed", start)
		WriteOpenAIErrorWithRequestID(c, http.StatusForbidden, errTypePermission, "model is not in this API key's allowlist", rc.RequestID)
		return
	}

	// Step 6: request structure validation.
	if err := parsed.validate(); err != nil {
		s.finalize(rc, http.StatusBadRequest, "validate: "+err.Error(), start)
		WriteOpenAIErrorWithRequestID(c, http.StatusBadRequest, errTypeInvalidRequest, err.Error(), rc.RequestID)
		return
	}

	// Step 7: candidates filtered by requested capability.
	allCandidates, err := repository.ListModelCandidatesByModelID(s.db, m.ID)
	if err != nil {
		logger.Error("gateway: list candidates", zap.String("request_id", rc.RequestID), zap.Error(err))
		s.finalize(rc, http.StatusInternalServerError, "db_candidates: "+err.Error(), start)
		WriteOpenAIErrorWithRequestID(c, http.StatusInternalServerError, errTypeServer, "internal error", rc.RequestID)
		return
	}
	routable, anyEnabled, anyVerified := filterCandidates(allCandidates, parsed.Stream, parsed.hasTools())
	if len(routable) == 0 {
		reason := "no_enabled_candidate"
		if anyVerified {
			reason = "no_capability_candidate"
		} else if anyEnabled {
			reason = "no_verified_candidate"
		}
		s.finalize(rc, http.StatusServiceUnavailable, reason, start)
		WriteOpenAIErrorWithRequestID(c, http.StatusServiceUnavailable, errTypeUnavailable, "model is not available", rc.RequestID)
		return
	}

	// Steps 8–12.
	s.relayCandidates(c, rc, routable, body, start)
}

// checkKeyStateAndLimits runs the pre-call checks that don't need a paired
// release: status (revoked), expiry, budget (read-only here — the gateway
// writes the spend in finalize), and RPM. Concurrency is handled separately
// in Handle because it needs a deferred release.
func (s *RelayService) checkKeyStateAndLimits(c *gin.Context, rc *RelayContext, apiKey *model.APIKey, start time.Time) bool {
	if apiKey.Status == model.APIKeyStatusRevoked {
		s.finalize(rc, http.StatusUnauthorized, "revoked", start)
		WriteOpenAIErrorWithRequestID(c, http.StatusUnauthorized, errTypeAuthentication, "API key revoked", rc.RequestID)
		return false
	}
	if apiKey.ExpiresAt != nil && apiKey.ExpiresAt.Before(time.Now().UTC()) {
		s.finalize(rc, http.StatusUnauthorized, "expired", start)
		WriteOpenAIErrorWithRequestID(c, http.StatusUnauthorized, errTypeAuthentication, "API key expired", rc.RequestID)
		return false
	}
	if apiKey.BudgetLimitCents != nil && apiKey.BudgetSpentCents >= *apiKey.BudgetLimitCents {
		s.finalize(rc, http.StatusTooManyRequests, "budget_exceeded", start)
		WriteOpenAIErrorWithRequestID(c, http.StatusTooManyRequests, errTypeInsufficientQuota, "budget limit exceeded", rc.RequestID)
		return false
	}
	return true
}

// relayCandidates walks the candidate chain in sort_order. For each
// candidate it loads the provider's enabled keys, decrypts them one at a
// time, and sends the upstream request; Key rotation and candidate failover
// decisions come back from tryKeys.
func (s *RelayService) relayCandidates(c *gin.Context, rc *RelayContext, candidates []model.ModelCandidate, body []byte, start time.Time) {
	for i := range candidates {
		cand := candidates[i]
		rc.Candidate = &cand
		// Reset per iteration: rc.Provider is set only when this candidate's
		// provider is usable, so a `continue` path (provider missing/disabled,
		// load-keys failed, no enabled key, rewrite failed) doesn't leave a
		// stale provider from a previous iteration on rc — which finalize
		// would otherwise record as the "final hit provider" of an all-failed
		// request.
		rc.Provider = nil

		provider := cand.Provider
		if provider == nil {
			rc.Attempts = append(rc.Attempts, makeAttempt(cand, nil, nil, 0, AttemptBadStatus, "provider missing (preload)"))
			continue
		}
		if provider.ManagementStatus != model.ProviderStatusEnabled {
			rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, nil, 0, AttemptBadStatus, "provider disabled"))
			continue
		}
		rc.Provider = provider

		keys, err := repository.ListProviderKeysByProvider(s.db, provider.ID)
		if err != nil {
			logger.Error("gateway: list provider keys", zap.String("request_id", rc.RequestID), zap.Error(err))
			rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, nil, 0, AttemptBadStatus, "load keys failed"))
			continue
		}
		enabled, anyEnabledKey := filterEnabledKeys(keys)
		if len(enabled) == 0 {
			reason := "no enabled key"
			if anyEnabledKey {
				reason = "no verified key"
			}
			rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, nil, 0, AttemptBadStatus, reason))
			continue
		}

		// Step 9: rewrite the model field to this candidate's provider name.
		upstreamBody, err := RewriteRequestModel(body, cand.ProviderModelName)
		if err != nil {
			rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, nil, 0, AttemptBadStatus, "rewrite model: "+err.Error()))
			continue // mapping failure -> skip candidate (PRD §6.5.3 step 9)
		}
		// PRD §1114: for a stream request where the caller didn't ask for
		// usage, force stream_options.include_usage=true upstream so the
		// final usage frame arrives and budget/cost accounting works — the
		// injected usage is stripped before forwarding (StreamUpstreamToClient).
		upstreamBody, err = EnsureStreamUsageInjection(upstreamBody, rc.IsStream, rc.WantsStreamUsage)
		if err != nil {
			rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, nil, 0, AttemptBadStatus, "inject stream usage: "+err.Error()))
			continue
		}

		if s.tryKeys(c, rc, &cand, provider, enabled, upstreamBody, start) == outcomeDone {
			return
		}
		// outcomeNextCandidate: fall through to the next candidate.
	}
	s.allCandidatesFailed(c, rc, start)
}

// relayOutcome is what tryKeys reports back to relayCandidates.
type relayOutcome int

const (
	outcomeDone          relayOutcome = iota // response written, relay finished
	outcomeNextCandidate                     // this candidate's keys are exhausted, try next
)

// tryKeys walks one provider's enabled keys. Returns outcomeDone once a
// response (success OR a non-switchable failure) has been written to the
// client, or outcomeNextCandidate when every key on this provider failed
// with a key-rotation error and the chain should move to the next candidate
// (GATE-09: same-provider no usable key, THEN failover; GATE-10).
func (s *RelayService) tryKeys(c *gin.Context, rc *RelayContext, cand *model.ModelCandidate, provider *model.Provider, keys []model.ProviderKey, upstreamBody []byte, start time.Time) relayOutcome {
	for i := range keys {
		pk := keys[i]
		// Destination-version guard (M2 credential-scope mechanism): a key
		// is only authorized for the provider destination it was verified
		// against. When an admin changes BaseURL, DestinationVersion bumps
		// while existing keys keep their old AuthorizedDestinationVersion —
		// decrypting and sending such a key would exfiltrate the credential
		// to an unapproved destination. Skip and rotate to the next key,
		// matching the destination-matched select in provider_repository.go.
		if pk.AuthorizedDestinationVersion != provider.DestinationVersion {
			rc.Attempts = append(rc.Attempts, makeAttempt(*cand, provider, &pk, 0, AttemptAuthFailed, "destination version mismatch"))
			continue
		}
		plaintext, derr := crypto.Decrypt(s.masterKey, pk.EncryptedKey)
		if derr != nil {
			logger.Warn("gateway: decrypt provider key failed",
				zap.Uint("key_id", pk.ID), zap.String("request_id", rc.RequestID), zap.Error(derr))
			rc.Attempts = append(rc.Attempts, makeAttempt(*cand, provider, &pk, 0, AttemptBadStatus, "decrypt failed"))
			continue
		}
		switch s.attemptOne(c, rc, *cand, provider, pk, plaintext, upstreamBody, start) {
		case attemptSuccess, attemptTerminal:
			return outcomeDone
		case attemptRotateKey:
			continue // next key on the same provider
		case attemptNextCandidate:
			return outcomeNextCandidate
		}
	}
	// Every key failed with a key-rotation error → failover (GATE-09/10).
	return outcomeNextCandidate
}

// attemptResult is what one upstream attempt reports back to tryKeys.
type attemptResult int

const (
	attemptSuccess       attemptResult = iota
	attemptTerminal                    // 4xx client error — surfaced to caller, no switch (GATE-11)
	attemptRotateKey                   // 401/429 — try next key (GATE-09)
	attemptNextCandidate               // 5xx / conn / timeout — try next candidate (GATE-10)
)

// attemptOne sends one upstream request with one decrypted key and routes
// the response. Transport failures, 5xx, and pre-first-byte stream failures
// are candidate-level (failover); 401/429 are key-level (rotate); 2xx is
// success; other 4xx is terminal (caller's problem, GATE-11).
func (s *RelayService) attemptOne(c *gin.Context, rc *RelayContext, cand model.ModelCandidate, provider *model.Provider, pk model.ProviderKey, plaintext string, upstreamBody []byte, start time.Time) attemptResult {
	ctx, cancel := context.WithTimeout(c.Request.Context(), upstreamRequestTimeout)
	defer cancel()

	resp, err := s.client.SendUpstream(ctx, provider.BaseURL, plaintext, upstreamBody)
	if err != nil {
		// Caller disconnected mid-request is terminal (can't switch — the
		// caller is gone). Distinguish context.Canceled (client gone) from
		// context.DeadlineExceeded (server-side/per-attempt timeout, which
		// is candidate-level, not a disconnect) so the log labels the right
		// failure class. Any other transport failure is candidate-level.
		if errors.Is(c.Request.Context().Err(), context.Canceled) {
			rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, 0, AttemptConnError, "client disconnected"))
			s.finalize(rc, 499, "client_disconnected", start) // nginx-style 499
			return attemptTerminal
		}
		rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, 0, AttemptConnError, err.Error()))
		return attemptNextCandidate
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 2xx — dispatch directly instead of through a one-line trampoline.
		if rc.IsStream {
			return s.handleStream(c, rc, cand, provider, pk, resp, start)
		}
		return s.handleNonStream(c, rc, cand, provider, pk, resp, start)
	}

	statusCode := resp.StatusCode
	_ = resp.Body.Close()

	class := classifyUpstreamStatus(statusCode)
	note := fmt.Sprintf("upstream %d", statusCode)
	rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, statusCode, class.Outcome, note))
	switch class.Category {
	case statusRotateKey:
		// GATE-16: a 401 means the credential itself was rejected —
		// persist verification_status=Failed so subsequent requests skip
		// this key (filterEnabledKeys checks verification_status) instead
		// of retrying the dead credential first. 429 (rate limit) is NOT
		// persisted: it's transient and the key is still valid. CAS on
		// (Passed, destination) so a concurrent edit or destination change
		// can't be clobbered; applied=false is a benign lost race.
		if statusCode == http.StatusUnauthorized {
			if applied, mErr := repository.MarkProviderKeyVerificationFailedIfCurrent(s.db, pk.ID, provider.DestinationVersion, time.Now()); mErr != nil {
				logger.Warn("gateway: mark provider key failed",
					zap.Uint("key_id", pk.ID), zap.String("request_id", rc.RequestID), zap.Error(mErr))
			} else if !applied {
				logger.Debug("gateway: provider key invalidation CAS lost race",
					zap.Uint("key_id", pk.ID), zap.String("request_id", rc.RequestID))
			}
		}
		return attemptRotateKey
	case statusFailover:
		return attemptNextCandidate
	default: // statusTerminalClient — caller's request is the problem, no switch (GATE-11).
		if !c.Writer.Written() {
			WriteOpenAIErrorWithRequestID(c, statusCode, class.ErrorType, safeUpstreamMessage(statusCode), rc.RequestID)
		}
		s.finalize(rc, statusCode, fmt.Sprintf("upstream_client_error_%d", statusCode), start)
		return attemptTerminal
	}
}

func (s *RelayService) handleNonStream(c *gin.Context, rc *RelayContext, cand model.ModelCandidate, provider *model.Provider, pk model.ProviderKey, resp *http.Response, start time.Time) attemptResult {
	defer func() { _ = resp.Body.Close() }()
	// Bound the response read: a buggy or hostile upstream can otherwise
	// return an unbounded body and exhaust gateway memory before the
	// request timeout fires (the response body has no bodylimit guard the
	// way the request body does). Mirrors provider_client.go: read up to
	// N+1 bytes so an overflow is detectable, then failover.
	limited := io.LimitReader(resp.Body, maxNonStreamResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		// A caller disconnect during the body read is terminal (the caller
		// is gone), not a candidate failure — recognize it to avoid a
		// wasted failover attempt and to log 499, not bad_status.
		if errors.Is(c.Request.Context().Err(), context.Canceled) {
			rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, resp.StatusCode, AttemptConnError, "client disconnected"))
			s.finalize(rc, 499, "client_disconnected", start)
			return attemptTerminal
		}
		rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, resp.StatusCode, AttemptBadStatus, "read body: "+err.Error()))
		return attemptNextCandidate
	}
	if int64(len(body)) > maxNonStreamResponseBytes {
		rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, resp.StatusCode, AttemptBadStatus, "response too large"))
		return attemptNextCandidate
	}
	rewritten, usage, err := RewriteNonStreamResponse(body, rc.OriginalModel)
	if err != nil {
		rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, resp.StatusCode, AttemptBadStatus, "rewrite: "+err.Error()))
		return attemptNextCandidate
	}
	rc.Usage = usage
	rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, resp.StatusCode, AttemptSuccess, ""))
	c.Header("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(rewritten)
	s.finalize(rc, resp.StatusCode, "", start)
	return attemptSuccess
}

func (s *RelayService) handleStream(c *gin.Context, rc *RelayContext, cand model.ModelCandidate, provider *model.Provider, pk model.ProviderKey, resp *http.Response, start time.Time) attemptResult {
	usage, err := StreamUpstreamToClient(c, resp, rc)
	if usage != nil {
		rc.Usage = usage // preserve partial usage even on error paths (GATE-21)
	}
	if err == nil {
		rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, resp.StatusCode, AttemptSuccess, ""))
		s.finalize(rc, http.StatusOK, "", start)
		return attemptSuccess
	}
	if errors.Is(err, errClientDisconnected) {
		rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, resp.StatusCode, AttemptConnError, "client disconnected"))
		s.finalize(rc, 499, "client_disconnected", start)
		return attemptSuccess // already streamed partial content, terminal
	}
	if errors.Is(err, errStreamNoDoneTerminator) {
		// Upstream sent content but closed without the [DONE] terminator.
		// Do NOT inject a synthetic error event — the caller already
		// received the content bytes, and most OpenAI SDKs handle a plain
		// EOF gracefully. Injecting an error event would break a possibly-
		// complete completion (some upstreams stably omit [DONE]). Only
		// mark the log row partial so the missing terminator is traceable.
		rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, resp.StatusCode, AttemptServerError, "stream ended without [DONE]"))
		s.finalize(rc, http.StatusOK, "stream_no_done", start)
		return attemptSuccess
	}
	if rc.FirstByteSent {
		// Mid-stream failure after the first byte — can't switch (GATE-19),
		// can't change HTTP status; emit one inline SSE error event + close.
		writeStreamErrorEvent(c, rc.RequestID)
		rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, resp.StatusCode, AttemptServerError, "stream mid: "+err.Error()))
		s.finalize(rc, http.StatusOK, "stream_partial: "+err.Error(), start)
		return attemptSuccess
	}
	// Pre-first-byte failure: nothing written yet, can still failover.
	rc.Attempts = append(rc.Attempts, makeAttempt(cand, provider, &pk, resp.StatusCode, AttemptServerError, "stream start: "+err.Error()))
	return attemptNextCandidate
}

// allCandidatesFailed is reached only when every candidate was tried without
// a response being written — the writer is guaranteed not yet written, but
// the guard is kept defensively in case a future caller changes that.
func (s *RelayService) allCandidatesFailed(c *gin.Context, rc *RelayContext, start time.Time) {
	if c.Writer.Written() {
		status := rc.StatusCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		s.finalize(rc, status, "partial_then_exhausted", start)
		return
	}
	status := http.StatusBadGateway
	WriteOpenAIErrorWithRequestID(c, status, errTypeUpstream, "all upstream candidates failed", rc.RequestID)
	s.finalize(rc, status, "all_candidates_failed", start)
}

// filterCandidates returns the subset of candidates eligible for this
// request: enabled, and supporting the stream / function-calling capability
// the caller asked for (PRD §6.5.3 step 7). anyEnabled is reported in the
// same pass so the caller can distinguish "no candidate at all" from "no
// candidate matched the capability" without walking the slice twice. Order
// is preserved (sort_order was applied by the repository) so failover still
// walks the chain in the admin's configured order.
// filterCandidates returns routable candidates plus two diagnostic flags so
// the caller can pick the right "why empty" reason: anyEnabled = at least one
// management-enabled candidate (distinguishes "all disabled" from anything
// else); anyVerified = at least one enabled AND verification-passed
// (distinguishes "enabled but unverified/failed" from "capability mismatch").
func filterCandidates(all []model.ModelCandidate, isStream, hasTools bool) (routable []model.ModelCandidate, anyEnabled, anyVerified bool) {
	for _, c := range all {
		if c.ManagementStatus != model.ModelCandidateStatusEnabled {
			continue
		}
		anyEnabled = true
		// An enabled-but-verification-failed candidate is NOT routable:
		// TestModelCandidate flips verification_status to failed while
		// leaving management status enabled, and ModelService's own
		// routability check (model_service.go) already rejects these —
		// the gateway must match that gate or it routes a known-broken
		// mapping (PRD §6.5.3 step 7).
		if c.VerificationStatus != model.ModelVerificationStatusPassed {
			continue
		}
		anyVerified = true
		if isStream && !c.SupportsStreaming {
			continue
		}
		if hasTools && !c.SupportsFunctionCalling {
			continue
		}
		routable = append(routable, c)
	}
	return routable, anyEnabled, anyVerified
}

// filterEnabledKeys returns keys that are both management-enabled AND
// verification-passed (the gateway must match ModelService's routability
// gate). anyEnabled lets the caller distinguish "all keys disabled" from
// "enabled but none verified" for an accurate log reason.
func filterEnabledKeys(keys []model.ProviderKey) (out []model.ProviderKey, anyEnabled bool) {
	out = make([]model.ProviderKey, 0, len(keys))
	for _, k := range keys {
		if k.ManagementStatus != model.ProviderKeyStatusEnabled {
			continue
		}
		anyEnabled = true
		// Match ModelService routability: a key whose verification_status
		// is not Passed (never tested, or failed a retest) must not be
		// sent to the upstream — the gateway would otherwise keep using a
		// credential already known to be invalid.
		if k.VerificationStatus != model.VerificationStatusPassed {
			continue
		}
		out = append(out, k)
	}
	return out, anyEnabled
}

// makeAttempt builds one AttemptRecord. provider and key are nil-able: nil
// provider marks a candidate whose provider was missing/disabled; nil key
// marks a candidate-level failure before any key was tried (load failed, no
// enabled key, rewrite failed). One constructor replaces the former
// makeAttempt/makeAttemptWithKey pair.
func makeAttempt(cand model.ModelCandidate, provider *model.Provider, key *model.ProviderKey, status int, outcome, failReason string) AttemptRecord {
	rec := AttemptRecord{
		CandidateID:       cand.ID,
		ProviderModelName: cand.ProviderModelName,
		StatusCode:        status,
		Outcome:           outcome,
		FailReason:        failReason,
	}
	if provider != nil {
		rec.ProviderID = provider.ID
		rec.ProviderName = provider.Name
	} else {
		rec.ProviderID = cand.ProviderID
	}
	if key != nil {
		rec.KeyID = key.ID
		rec.KeyLabel = key.Label
	}
	return rec
}
