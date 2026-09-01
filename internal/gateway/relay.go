package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/config"
	"github.com/yolorouter/yolorouter/internal/decision"
	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/gateway/circuit"
	"github.com/yolorouter/yolorouter/internal/loopback"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// maxNonStreamResponseBytes caps a single non-stream upstream response body.
// A buggy or hostile provider can return an arbitrarily large body; without
// this cap io.ReadAll would grow the buffer until OOM before the request
// timeout fires (the response body has no bodylimit guard the way the
// request body does). Mirrors the provider-test client's bound
// (provider_client.go). Read up to N+1 so an overflow is detectable.
const maxNonStreamResponseBytes = 32 * 1024 * 1024 // 32 MiB

// BodyAuditCap bounds every early-rejection audit read of the caller's
// request body (captureRejectedBody here, and middleware.logAuthRejection's
// own read for the auth-gate rejection paths — the two
// packages each defined their own identical copy of this constant, which
// nothing enforced staying in sync). Exported so middleware can share this
// single definition instead of duplicating it. Mirrors the /v1 route group's
// middleware.BodySizeLimit(20<<20) (router.go) — this is a memory-safety cap
// on our read, not a re-enforcement of that limit (http.MaxBytesReader
// already enforces it upstream of us, before this code ever runs).
const BodyAuditCap = 20 << 20 // 20 MiB

// ReadAuditBody drains r (bounded by BodyAuditCap) — the shared bounded-read
// step the post-auth early-rejection audit paths need (captureRejectedBody
// below: revoked/expired/budget/RPM/concurrency, all for an ALREADY-
// authenticated caller who is rate-limited). Best-effort: nil on a read error
// or a nil/absent body. v0.1 stores the caller body verbatim — no content
// scrubbing (only request headers are masked, via SanitizeHeaders).
func ReadAuditBody(r io.Reader) []byte {
	return ReadAuditBodyCapped(r, BodyAuditCap+1)
}

// ReadAuditBodyCapped is ReadAuditBody with a caller-chosen byte ceiling, so
// the UNauthenticated auth-rejection path (middleware.logAuthRejection) can
// bound its capture far below BodyAuditCap: without a valid key, an attacker
// could otherwise make the gateway read + persist a 20 MiB body per rejected
// request and inflate request_log_bodies without ever authenticating.
// Best-effort: nil on a read error or a nil/absent
// body, never an error the caller must handle.
func ReadAuditBodyCapped(r io.Reader, limit int64) []byte {
	if r == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return nil
	}
	return b
}

// captureRejectedBody drains the caller request body for the audit row, so
// records the request body even when the request is rejected before
// the normal body read (revoked/expired/budget/concurrency/RPM, all before
// io.ReadAll in Handle).
func captureRejectedBody(c *gin.Context, rc *Exchange) {
	if rc.bodies.Request() != nil {
		return // already captured (e.g. body read succeeded then a later check failed)
	}
	if c.Request == nil {
		return
	}
	if body := ReadAuditBody(c.Request.Body); body != nil {
		rc.bodies.SetRequest(body)
	}
}

// testHookHandleDone, when non-nil, is invoked with the Exchange at the
// end of every Handle call (success or failure). Test-only wiring — Handle
// intentionally doesn't expose its internal Exchange in its public
// signature, so tests needing to inspect it (e.g. the captured request/
// response bodies) set this hook instead. Always nil in production.
var testHookHandleDone func(*Exchange)

// Service is the gateway orchestrator. One instance lives for the
// process lifetime (created in router.New); it owns the DB, the master key
// for decrypting provider keys, an upstream HTTP client, and the in-memory
// rate limiter.
type Service struct {
	db      *gorm.DB
	secrets crypto.SecretBox
	client  *UpstreamClient
	// settingsProvider is the read-only window into the cached global custom
	// system prompt. Nil when no provider is wired in (router passes nil until
	// the system settings service is registered); Handle nil-checks before
	// reading it, so a nil provider simply means no global prompt is applied.
	settingsProvider SettingsProvider
	// gateway carries the resolved gateway timeouts (connect/header/body-idle/
	// attempt/request) from config.GatewayConfig. The per-attempt timeout
	// orchestration (attemptOne, RequestDeadline) reads the individual fields
	// off this struct instead of re-deriving them per call.
	gateway config.GatewayConfig

	// breaker is the per-provider health record the decision table's circuit
	// effects are booked against, and the candidate loop consults before
	// walking a candidate.
	breaker *circuit.Breaker

	// keyPool is the per-provider key rotation cursor and per-key rate-limit
	// bench: it decides the ORDER tryKeys walks a provider's keys in — spread
	// across the pool rather than always through its first key. The
	// demotion-not-exclusion guarantee lives with the type (keypool.go).
	keyPool *keyPool

	// bindings is the sticky-binding registry balanced models route through:
	// caller keys pinned to providers, least-bound-first on assignment. Only
	// balanced models consult it; failover chains never do. Exposed via
	// Bindings so the assembly layer can share the one instance with the
	// admin model view (the per-candidate binding counts).
	bindings *BindingRegistry

	// secondaryFetch is the shared client for downloading responses an upstream
	// referred to rather than returned. Built once, on first use: a transport
	// per request would pool connections nobody ever reuses or closes.
	secondaryFetchOnce sync.Once
	secondaryFetch     *http.Client

	// upstreamErrorObservers are wired in by the assembly layer. They see
	// non-2xx upstream responses and report what they recognise; they never
	// decide what happens next. Order is irrelevant by construction — reported
	// judgements fold together by a rule that does not depend on who reported
	// first.
	upstreamErrorObservers []upstreamErrorObserver

	// deliveryObservers see how the exchange ended, successfully or not. This
	// is where an observation drawn from a SERVED response lands: the streaming
	// and non-streaming paths both settle here, so there is one call site
	// rather than one per response shape.
	deliveryObservers []deliveryObserver

	// ingressRewriters rewrite the caller's own body once, before any
	// candidate is chosen, ordered by stage at registration so no per-request
	// sort is needed.
	ingressRewriters []ingressRewriter

	// egressRewriters rewrite the outbound body, ordered by stage at
	// registration so no per-request sort is needed.
	egressRewriters []egressRewriter

	// responseCodecWrappers decorate the encoders that turn a converted
	// response back into the caller's protocol, ordered by stage at
	// registration. They reach only the cross-protocol path: a response
	// relayed in the caller's own protocol has no encoder to wrap.
	responseCodecWrappers []responseCodecWrapper

	// failureRewriters see a non-2xx upstream response together with the body
	// that provoked it, and may offer a repaired body. They never decide what
	// happens next: whether the repair is worth an attempt is the decision
	// table's call, made from the facts they report.
	failureRewriters []failureRewriter

	// admissions gate the exchange before any upstream work. Registration
	// order is acquisition order; release runs in reverse.
	admissions []admission

	// recorders receive the settled exchange exactly once, on every exit path.
	recorders []recorder
}

// NewService wires the gateway with the already-decoded AES master key
// (the same one provider_service uses to decrypt the keys it now routes to).
// allowPrivate is forwarded to the upstream client's SSRF transport (config.
// SecurityConfig.AllowPrivateUpstreams) so LAN/localhost providers relay.
// sp is the read-only custom system prompt provider; nil is valid and
// disables global prompt injection (per-key overrides still apply).
// gatewayCfg is the resolved config.GatewayConfig; its ConnectTimeout seeds
// the upstream transport's TCP dial bound and its HeaderTimeout seeds the
// ResponseHeaderTimeout, while the remaining fields are read by the
// per-attempt timeout orchestration.
func NewService(db *gorm.DB, secrets crypto.SecretBox, allowPrivate bool, sp SettingsProvider, gatewayCfg config.GatewayConfig) *Service {
	// Normalise the count budgets here rather than at every read: a config
	// assembled without them (unit tests, older config files) means the
	// defaults, not a request that stops before its first dispatch.
	if gatewayCfg.MaxUpstreamAttempts <= 0 {
		gatewayCfg.MaxUpstreamAttempts = config.DefaultMaxUpstreamAttempts
	}
	if gatewayCfg.MaxCandidateProbes <= 0 {
		gatewayCfg.MaxCandidateProbes = config.DefaultMaxCandidateProbes
	}
	if gatewayCfg.CircuitFailureThreshold <= 0 {
		gatewayCfg.CircuitFailureThreshold = config.DefaultCircuitFailureThreshold
	}
	if gatewayCfg.CircuitSuccessThreshold <= 0 {
		gatewayCfg.CircuitSuccessThreshold = config.DefaultCircuitSuccessThreshold
	}
	if gatewayCfg.CircuitOpenTimeout <= 0 {
		gatewayCfg.CircuitOpenTimeout = config.DefaultCircuitOpenTimeout
	}
	if gatewayCfg.KeyRateLimitCooldown <= 0 {
		gatewayCfg.KeyRateLimitCooldown = config.DefaultKeyRateLimitCooldown
	}
	return &Service{
		db:               db,
		secrets:          secrets,
		client:           NewUpstreamClient(allowPrivate, gatewayCfg.HeaderTimeout, gatewayCfg.ConnectTimeout, gatewayCfg.TLSHandshakeTimeout),
		settingsProvider: sp,
		gateway:          gatewayCfg,
		breaker: circuit.New(circuit.Config{
			FailureThreshold: gatewayCfg.CircuitFailureThreshold,
			SuccessThreshold: gatewayCfg.CircuitSuccessThreshold,
			OpenTimeout:      gatewayCfg.CircuitOpenTimeout,
		}),
		keyPool:  newKeyPool(time.Now),
		bindings: NewBindingRegistry(time.Now),
	}
}

// Bindings exposes the sticky-binding registry so the assembly layer can
// hand the same instance to consumers outside the gateway (the admin model
// view reads per-candidate binding counts from it). The registry is created
// with the service and lives exactly as long; there is deliberately no
// setter — two registries would mean two spreads.
func (s *Service) Bindings() *BindingRegistry {
	return s.bindings
}

// derefLimit reads an optional limit. An absent limit and a zero limit both
// mean unlimited, so they collapse to the same value rather than being carried
// as two states nothing downstream distinguishes.
func derefLimit(v *int) int {
	if v == nil || *v < 0 {
		return 0
	}
	return *v
}

// requestIDFor returns the request id the RequestID middleware already
// generated (uuid, set on the gin context + X-Request-Id header), so the
// gateway's error messages and request_logs row share ONE id with the
// access log — not a second unrelated id. Falls back to a fresh hex id only
// if some route mounted Service without the RequestID middleware.
func requestIDFor(c *gin.Context) string {
	if id := c.GetString("request_id"); id != "" {
		return id
	}
	return generateRequestID()
}

// isClientDisconnected reports whether the CALLER's own connection was
// canceled, as opposed to a derived context (e.g. rc.requestCtx, which also
// expires when the request-level budget in RequestDeadline runs out).
// Checking c.Request.Context().Err() directly — rather than inspecting the
// error a failing DB call returns — is what makes this distinction
// possible: c.Request.Context() only ever becomes Canceled when the client
// itself hangs up, never when a context derived from it (with its own
// shorter deadline) simply times out on its own schedule. Mirrors the
// disconnect check already used around the body read and the upstream send
// (Handle, attemptOne) in this package.
func isClientDisconnected(c *gin.Context) bool {
	return errors.Is(c.Request.Context().Err(), context.Canceled)
}

// Handle is POST /v1/chat/completions. apiKey is the already-authenticated
// caller key (middleware.APIKeyAuth resolved and validated it). The handler
// runs the full pipeline: pre-checks → model lookup → allowlist →
// validate → candidate chain with Key rotation + failover → response rewrite
// → log. Every exit path writes exactly one request_logs row via finalize.
func (s *Service) Handle(c *gin.Context, apiKey *model.APIKey) {
	start := time.Now()
	rc := &Exchange{
		requestID:        requestIDFor(c),
		apiKeyID:         apiKey.ID,
		userID:           apiKey.UserID,
		concurrencyLimit: derefLimit(apiKey.ConcurrencyLimit),
		rpmLimit:         derefLimit(apiKey.RPMLimit),
		tpmLimit:         derefLimit(apiKey.TPMLimit),
	}
	// Stamp the per-request total-budget deadline up front, before any
	// attempt logic, so every exit path (including early rejections below)
	// carries a consistent RequestDeadline. Each upstream attempt
	// derives its own cap as min(attempt_timeout, time.Until(RequestDeadline)).
	rc.requestDeadline = time.Now().Add(s.gateway.RequestTimeout)
	// Derive a context carrying the total-budget deadline so candidate
	// queries (model/candidate/key GORM reads) and each per-attempt context
	// are all bounded by RequestDeadline. Without this, the GORM calls used
	// s.db with no deadline and a stuck query could block past the request
	// cap. The per-attempt ctx in attemptOne derives from this, so
	// RequestCtx deadline => attempt ctx deadline too.
	requestCtx, requestCancel := context.WithDeadline(c.Request.Context(), rc.requestDeadline)
	defer requestCancel()
	// The whole ending of the exchange — safety-net settlement, admission
	// release, recording, the test hook, in that load-bearing order — is one
	// call, so the choreography is written once (and tested once) in
	// concludeExchange instead of being encoded in the registration order of
	// defers here. Armed immediately after the context cancel so it covers a
	// panic anywhere below, including inside the admission calls themselves.
	// held is declared here and read by the closure at unwind time, so
	// tickets acquired by BOTH admission phases below are released.
	var held []heldTicket
	defer func() {
		s.concludeExchange(c, rc, held, start)
	}()
	rc.requestCtx = requestCtx
	// The ingress protocol is a property of the request path, computed once
	// up front so every error write in this function (and the pre-candidate
	// validation below) uses the wire envelope the caller actually expects
	// instead of always assuming OpenAI.
	ingress := IngressProtocol(c.Request.URL.Path)
	rc.ingress = ingress
	// Capture the raw path for the custom system prompt injection allowlist.
	// Gemini's route is a wildcard :modelaction, so the path (not the resolved
	// protocol) is the only thing that distinguishes generateContent from
	// countTokens / embedContent.
	rc.ingressPath = c.Request.URL.Path
	// Compute once so the compression gate and the CSP injection gate read
	// the bool instead of recomputing IsChatEndpoint(path) per call site.
	rc.isChatEndpoint = IsChatEndpoint(rc.ingressPath)
	// Put rc on the gin context so WriteIngressError
	// (called from many exit paths below, and potentially from further down
	// the chain) can stash the local error JSON into the response-body capture without
	// every call site threading an *Exchange parameter through.
	c.Set(relayContextKey, rc)
	// Capture the caller's request headers once at entry (masked
	// via SanitizeHeaders) so even an early rejection below still records
	// them. c.Request is always non-nil here (gin populates it), but guard
	// anyway for direct-call tests.
	if c.Request != nil {
		rc.requestHeaders = SanitizeHeaders(c.Request.Header)
	}
	// Admissions gate the exchange before any work is done on its behalf.
	// Whatever they take lands in held (declared with the conclude defer
	// above) and is given back on every exit path, including a panic inside
	// an admission: tickets are appended as they are acquired, so the
	// already-armed conclude defer sees everything taken so far.

	// The loopback marker resolves before any admission: a request bearing
	// this process's own token is the gateway calling itself on behalf of a
	// caller request that already holds the key's admission slots — running
	// the child through concurrency/RPM/TPM again would make it compete
	// with (and starve behind) its own parent. Key-state checks (revoked/
	// expired/budget) still apply; they need no slot and cost nothing.
	// Known tradeoff: skipping admission also skips the TPM/RPM debit, so
	// describe-call tokens never enter the key's rate windows. The budget
	// check still caps what a key can spend, and every sub-call bills and
	// logs normally, so the volume is visible and paid for — accepted
	// rather than teaching the limiter about ticketless debits.
	rc.visionFallbackSubCall = c.GetHeader(loopback.HeaderInternal) == loopback.Token
	if rc.visionFallbackSubCall {
		// The parent linkage is only trusted alongside a valid token — an
		// outside caller must not be able to attach itself to someone
		// else's request row.
		rc.parentRequestID = c.GetHeader(loopback.HeaderParent)
	}
	rc.authCredential = GatewayCredential(c)

	if !s.checkKeyStateAndLimits(c, rc, apiKey, start) {
		return
	}
	if !rc.visionFallbackSubCall {
		verdict := s.admit(rc.requestCtx, rc, AdmitOnArrival, &held)
		if verdict.Loop >= decision.LoopNextCandidate {
			captureRejectedBody(c, rc)
			status, errType := decision.AdmissionRejectionResponse(verdict)
			s.rejectRequest(c, rc, status, errType, verdict.RejectDetail(), verdict.FailReason(), fact.FaultClient, start)
			return
		}
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		// Caller disconnect during body upload is terminal 499 (mirrors the
		// stream/non-stream response paths), not a malformed-request 400.
		if errors.Is(c.Request.Context().Err(), context.Canceled) {
			s.abandonRequest(rc, "client_disconnected", start, settleOptions{})
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
		s.rejectRequest(c, rc, status, errTypeInvalidRequest, message, reason, fact.FaultClient, start)
		return
	}
	// Stash the caller-facing request body for the
	// request_log_bodies row, verbatim (v0.1 does not scrub body content).
	rc.bodies.SetRequest(body)

	// Every settings-dependent value this request uses, resolved in one
	// place: per-key overrides short-circuit the global read, and a failed
	// global read keeps the provider's last-known-good. Resolved here —
	// after the key is known and before the first consumer: the ingress
	// rewriters below read compression and vision through the capability
	// views, the allowlist reads the fallback model, and the egress prompt
	// stage reads the prompt — and that is the last moment anything may
	// change those bytes.
	rc.settings = resolveRequestSettings(c.Request.Context(), s.settingsProvider, apiKey, rc.requestID)

	// The rewriters run before the request is admitted, not after it is
	// validated, because the modality admits ONE body and everything
	// downstream builds from that one. Running them later would leave the
	// payload holding the original bytes while the exchange recorded the
	// rewritten ones. Safe in this order: a rewriter leaves a body it cannot
	// parse untouched, so an invalid request is still rejected by the
	// validation below rather than slipping past one that declined. The
	// captured request body keeps what the caller actually sent.
	admitBody, rewritten, ingressVerdict := s.rewriteIngress(requestCtx, rc, body)
	if ingressVerdict.Loop >= decision.LoopNextCandidate {
		// A rewriter declared the body unsendable. No candidate has been
		// chosen, so there is nothing to fail over to and nothing to reverse:
		// the table's terminal status is the whole answer.
		status, errType := decision.PreDispatchRejectionResponse(ingressVerdict,
			http.StatusInternalServerError, errTypeServer)
		s.rejectRequest(c, rc, status, errType, ingressVerdict.RejectDetail(),
			ingressVerdict.FailReason(), fact.FaultGateway, start)
		return
	}
	if rewritten {
		// Recorded separately from the caller's verbatim body: the audit row
		// has to be able to show both what arrived and what was carried
		// forward, or a rewrite becomes invisible the moment it succeeds.
		rc.bodies.SetCompressedRequest(admitBody)
	}

	// The modality that serves this ingress protocol decides whether the
	// request is one it can carry at all, and every refusal it can make is one
	// no candidate could have changed: a body that does not parse, a field the
	// protocol requires, a path that names no model.
	modality, ok := modalityFor(ingress)
	if !ok {
		// Nothing routes an unregistered protocol here today. Serving it with
		// whichever modality was nearest would answer the caller in a shape
		// they cannot read.
		logger.Error("gateway: no modality registered", zap.String("request_id", rc.requestID), zap.String("ingress", string(ingress)))
		s.rejectRequest(c, rc, http.StatusInternalServerError, errTypeServer, "internal error", "no_modality: "+string(ingress), fact.FaultGateway, start)
		return
	}
	payload, rej := modality.Admit(requestCtx, Ingress{
		Protocol:    ingress,
		Path:        c.Request.URL.Path,
		ContentType: c.GetHeader("Content-Type"),
		Body:        admitBody,
	})
	if rej != nil {
		s.rejectRequest(c, rc, rej.Status, rej.ErrorType, rej.Message, rej.FailReason, rej.Fault, start)
		return
	}
	// Wrapped before anything calls it: the wrapper is what holds the call
	// order and reconciles what a modality claims against what actually went
	// out to the caller.
	adm := admitted{payload: newOrderedPayload(payload, rc.requestID), limits: modality.Limits()}
	// The payload's policy for its own bodies is read once, here, so the
	// record path can enforce it without holding the payload for two more
	// calls. Settled requests only: a refusal above leaves nil, which the
	// policy applier reads as "keep the kernel's own view".
	rc.payloadLog = admitBodyLog(adm.payload, c.GetHeader("Content-Type"))
	// A modality that declared a total budget narrows the request deadline to
	// it. The declaration is a real cap, not advice: everything downstream —
	// per-attempt budgets, the request context the DB reads run on — derives
	// from this deadline, so narrowing it here is narrowing all of them at
	// once. Only ever narrowed: a modality may shorten the kernel's budget,
	// never outlive it.
	if adm.limits.TotalBudget > 0 {
		if capped := start.Add(adm.limits.TotalBudget); capped.Before(rc.requestDeadline) {
			rc.requestDeadline = capped
			budgetCtx, budgetCancel := context.WithDeadline(c.Request.Context(), rc.requestDeadline)
			defer budgetCancel()
			requestCtx = budgetCtx
			rc.requestCtx = requestCtx
		}
	}
	routing := adm.payload.Routing()
	rc.originalModel = routing.Model
	rc.isStream = routing.Stream

	// Every write to the caller slides a deadline forward, from the response
	// object the delivery was handed rather than from a package-wide default,
	// so a modality that asked for a shorter window gets the one it asked for.
	// This bounds a slow-reading client without clearing the server
	// WriteTimeout entirely.
	// The server WriteTimeout (RequestTimeout + 60s) covers the pre-first-
	// write gap (e.g. a long TTFT on a reasoning model); once the first
	// write lands, the sliding per-write deadline takes over. Non-streaming
	// endpoints keep the server WriteTimeout as a slow-read DoS guard.

	// Step 4: model exists and is enabled. A model disabled by an admin
	// must not route even if its candidates are still enabled.
	m, err := repository.FindModelByName(s.db.WithContext(requestCtx), rc.originalModel)
	if err != nil {
		if isClientDisconnected(c) {
			// The client hung up while this query was in flight — a
			// context.Canceled from the DB driver here is a disconnect, not
			// a server-side DB fault; nothing to write back to a gone caller.
			s.abandonRequest(rc, "client_disconnected", start, settleOptions{})
			return
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			s.rejectRequest(c, rc, http.StatusNotFound, errTypeNotFound, "model does not exist", "model_not_found", fact.FaultClient, start)
			return
		}
		logger.Error("gateway: find model", zap.String("request_id", rc.requestID), zap.Error(err))
		s.rejectRequest(c, rc, http.StatusInternalServerError, errTypeServer, "internal error", "db_model: "+err.Error(), fact.FaultGateway, start)
		return
	}
	if m.ManagementStatus != model.ModelStatusEnabled {
		s.rejectRequest(c, rc, http.StatusNotFound, errTypeNotFound, "model does not exist", "model_disabled", fact.FaultClient, start)
		return
	}

	// The model must declare the output modality this endpoint serves. The
	// vocabulary is the modality's own id, so the gate stays generic: a new
	// modality registers an id, model rows declare it, and nothing here
	// names an endpoint family. The refusal comes before any candidate is
	// walked, so a mismatched pairing costs no upstream call.
	if !m.ServesOutputModality(string(modality.ID())) {
		s.rejectRequest(c, rc, http.StatusBadRequest, errTypeInvalidRequest,
			fmt.Sprintf("model %q does not serve %s requests", rc.originalModel, modality.ID()),
			"model_modality_mismatch", fact.FaultClient, start)
		return
	}

	// Step 5: allowlist. A key flagged allow_all_models skips the per-model
	// check and may call any enabled model. A loopback sub-call skips it
	// ONLY for the admin-configured describe model — the exemption exists
	// because that target is the admin's choice, not the caller's, so the
	// code checks exactly that instead of trusting the marker alone; the
	// marker is process-token-gated on top.
	if !apiKey.AllowAllModels && (!rc.visionFallbackSubCall || m.Name != rc.settings.VisionFallbackModel) {
		allowed, err := repository.HasAPIKeyModelAccess(s.db.WithContext(requestCtx), apiKey.ID, m.ID)
		if err != nil {
			if isClientDisconnected(c) {
				s.abandonRequest(rc, "client_disconnected", start, settleOptions{})
				return
			}
			logger.Error("gateway: allowlist", zap.String("request_id", rc.requestID), zap.Error(err))
			s.rejectRequest(c, rc, http.StatusInternalServerError, errTypeServer, "internal error", "db_allowlist: "+err.Error(), fact.FaultGateway, start)
			return
		}
		if !allowed {
			s.rejectRequest(c, rc, http.StatusForbidden, errTypePermission, "model is not in this API key's allowlist", "model_not_allowed", fact.FaultClient, start)
			return
		}
	}

	// Step 7: candidates filtered by requested capability.
	allCandidates, err := repository.ListModelCandidatesByModelID(s.db.WithContext(requestCtx), m.ID)
	if err != nil {
		if isClientDisconnected(c) {
			s.abandonRequest(rc, "client_disconnected", start, settleOptions{})
			return
		}
		logger.Error("gateway: list candidates", zap.String("request_id", rc.requestID), zap.Error(err))
		s.rejectRequest(c, rc, http.StatusInternalServerError, errTypeServer, "internal error", "db_candidates: "+err.Error(), fact.FaultGateway, start)
		return
	}
	routable, anyEnabled := filterCandidates(allCandidates)
	if len(routable) == 0 {
		// The two states call for different people: no enabled route is a
		// configuration an operator switched off, routes pending verification
		// is a probe that has not passed yet. One shared "not available"
		// hid that difference from the caller reporting the problem.
		reason, message := "no_enabled_candidate", "model is not available: no enabled route"
		if anyEnabled {
			reason, message = "no_verified_candidate", "model is not available: routes not verified yet"
		}
		// No candidate was usable, so no provider was ever contacted. Blaming
		// upstream here would point an operator at a provider that had no part
		// in it; what is actually wrong is on our side of the wire.
		s.rejectRequest(c, rc, http.StatusServiceUnavailable, errTypeUnavailable, message, reason, fact.FaultGateway, start)
		return
	}

	// The prices come from the first routable candidate, and that is a seam
	// showing. Prices live on candidates, one per provider, while the question
	// is asked once for the request — so pricing up front prices against
	// whichever provider happened to sort first, and the chain can end on a
	// different one. Fixed here, in one place, so there is a single answer to
	// pick apart rather than one per call site. Nothing outside this file reads
	// it; the field's own note says why not. Read BEFORE the balanced reorder
	// below, so the basis is the admin's sort_order head for every request of
	// a model — a per-key bound provider must not make the estimate drift
	// between callers. Nothing consumes the estimate today (the text modality
	// answers that it cannot say), so the head-vs-walked-candidate gap is
	// presently harmless either way.
	rc.pricingBasis = PricingView{
		InputPricePerMillion:  routable[0].InputPrice,
		OutputPricePerMillion: routable[0].OutputPrice,
	}

	// Balanced models reorder the chain per caller key before the walk enters
	// it. Failover models keep the repository's sort_order untouched — the
	// historical behaviour, byte for byte.
	if m.IsBalanced() {
		routable = s.reorderBalanced(rc, m.ID, routable)
	}

	// Asked before any candidate is tried, because that is the only window in
	// which the answer could still change what happens: an estimate produced
	// after the request was sent is a number nobody can act on. Text answers
	// that it cannot say and nothing acts on the estimate yet; a modality that
	// CAN answer is the reason to settle whether this question is per-request
	// or per-candidate.
	_ = adm.payload.EstimateCost(rc.pricingBasis)

	// The second admission phase: after every ingress rewriter has had the
	// body, after a routable candidate exists, and before anything is dialled.
	// An admission is asked here rather than on arrival because on arrival
	// none of that was true.
	//
	// This is the last point where all of it holds at once, NOT a point where
	// the request is fully determined — the outbound body is built per
	// candidate further down, and the candidate itself is not committed to. The
	// phase constant's own documentation carries that limit.
	//
	// Tickets land in the same stack the first phase used, so the deferred
	// release already armed above gives them back newest-first without knowing
	// phases exist.
	if verdict := s.admit(requestCtx, rc, AdmitWhenPriced, &held); verdict.Loop >= decision.LoopNextCandidate {
		// The caller is refused something they asked for — an amount they
		// cannot cover — so this is their fault to fix, the same as the
		// arrival-phase refusals above. The body was captured when it was read,
		// so unlike those there is nothing to capture here.
		status, errType := decision.AdmissionRejectionResponse(verdict)
		s.rejectRequest(c, rc, status, errType, verdict.RejectDetail(), verdict.FailReason(), fact.FaultClient, start)
		return
	}

	// Steps 8–12.
	s.relayCandidates(c, rc, adm, routable, start)
}

// checkKeyStateAndLimits runs the pre-call checks that don't need a paired
// release: status (revoked), expiry, budget (read-only here — the gateway
// writes the spend in finalize), and RPM. Concurrency is handled separately
// in Handle because it needs a deferred release. captureRejectedBody is
// called on each rejection (these three checks all run
// before Handle's normal body read, so the audit row would otherwise have an
// empty request_body).
func (s *Service) checkKeyStateAndLimits(c *gin.Context, rc *Exchange, apiKey *model.APIKey, start time.Time) bool {
	if apiKey.Status == model.APIKeyStatusRevoked {
		captureRejectedBody(c, rc)
		s.rejectRequest(c, rc, http.StatusUnauthorized, errTypeAuthentication, "API key revoked", "revoked", fact.FaultClient, start)
		return false
	}
	if apiKey.ExpiresAt != nil && apiKey.ExpiresAt.Before(time.Now().UTC()) {
		captureRejectedBody(c, rc)
		s.rejectRequest(c, rc, http.StatusUnauthorized, errTypeAuthentication, "API key expired", "expired", fact.FaultClient, start)
		return false
	}
	if apiKey.BudgetLimitMicros != nil && apiKey.BudgetSpentMicros >= *apiKey.BudgetLimitMicros {
		captureRejectedBody(c, rc)
		s.rejectRequest(c, rc, http.StatusTooManyRequests, errTypeInsufficientQuota, "budget limit exceeded", "budget_exceeded", fact.FaultClient, start)
		return false
	}
	return true
}

// executeCircuit books a resolved decision's circuit effect against the
// current provider's health record.
//
// The record is keyed by PROVIDER, deliberately: finer keys (per protocol
// endpoint, per model) would split the failure signal until low-traffic
// routes never reach a threshold, and a provider whose infrastructure is
// falling over usually falls over as a whole. A deployment that hosts truly
// independent backends behind one provider row can model them as separate
// providers.
//
// PenalizeSoft books half a fault: the
// table uses it for signals that say more about load than about health (a
// rate limit, a truncated stream), so they open the breaker only at twice
// the threshold — tolerant of a traffic peak, but a provider that throttles
// or truncates persistently is still taken out of rotation eventually.
func (s *Service) executeCircuit(rc *Exchange, eff decision.CircuitEffect) {
	p := rc.attempt.Provider()
	if p == nil {
		return
	}
	switch eff {
	case decision.CircuitPenalize:
		s.breaker.RecordFailure(p.ID, rc.circuitGen)
	case decision.CircuitPenalizeSoft:
		s.breaker.RecordSoftFailure(p.ID, rc.circuitGen)

	case decision.CircuitReset:
		s.breaker.RecordSuccess(p.ID, rc.circuitGen)
		// The key's bench is NOT released here: that already happened on
		// the 2xx status line in attemptOne, where the acceptance is known
		// minutes before this delivery verdict — and is known even when the
		// delivery never concludes cleanly.
	}
}

// exhaustedBudget reports whether any of the request's budgets — wall-clock,
// attempts, probes — is spent. One predicate shared by the candidate-loop
// gate and the exhausted-chain terminal, so the walk stops and the answer is
// chosen by the same rule: a budget spent by the final candidate must produce
// the same verdict as one spent with candidates still waiting.
func (s *Service) exhaustedBudget(rc *Exchange) bool {
	return (!rc.requestDeadline.IsZero() && time.Until(rc.requestDeadline) <= 0) ||
		rc.attemptsSpent >= s.gateway.MaxUpstreamAttempts ||
		rc.probesSpent >= s.gateway.MaxCandidateProbes
}

// relayCandidates walks the candidate chain in sort_order. For each
// candidate it loads the provider's enabled keys, decrypts them one at a
// time, and sends the upstream request; Key rotation and candidate failover
// decisions come back from tryKeys.
func (s *Service) relayCandidates(c *gin.Context, rc *Exchange, adm admitted, candidates []model.ModelCandidate, start time.Time) {
	// The ingress protocol is a property of the request path, not of any
	// individual candidate — threaded through every candidate/key attempt
	// below.
	ingress := rc.ingress
	// skipCandidate books the probe a pre-dispatch abandonment costs and
	// records the row for it, in one place so a new skip path cannot forget
	// the charge. Every candidate walked past without a dispatch spends a
	// probe — the table prices all such judgements identically — which is
	// what keeps a large pool from being walked end to end for free. Key
	// rotations inside a candidate deliberately spend nothing (the
	// key-unusable row's call): a provider with several unusable keys must
	// not eat the walk budget without the candidate itself being abandoned.
	skipCandidate := func(cand model.ModelCandidate, provider *model.Provider, outcome, note string) {
		rc.spendBudget(decision.BudgetConsumeProbe)
		rc.recordAttempt(cand, provider, nil, 0, outcome, note)
	}
	// skipDeadEndCandidate is skipCandidate for provider-level dead ends the
	// circuit breaker cannot see: provider row missing or disabled, no
	// usable or decryptable key, destination mismatch. Beyond the skip row,
	// it quarantines the provider — EVERY dead end observed on the walk, so
	// none of them sits at zero bindings attracting the next new key — and,
	// when the dead end IS the caller's bound candidate, flags the binding
	// for replacement: the binding itself is NOT released here, so a walk
	// that ends with no candidate serving (everything else breaker-refused,
	// say) leaves the caller's affinity intact; the candidate that actually
	// writes the response replaces it instead. Everything else stays on
	// plain skipCandidate: request-shaped refusals (capability, build,
	// egress verdicts) and transient errors say nothing about the
	// provider's health, and the breaker's own refusal is the recovery path
	// working, not a dead end.
	skipDeadEndCandidate := func(cand model.ModelCandidate, provider *model.Provider, outcome, note string) {
		skipCandidate(cand, provider, outcome, note)
		s.bindings.Quarantine(cand.ProviderID)
		if rc.binding.candidateID != 0 && cand.ID == rc.binding.candidateID {
			rc.binding.invalidated = true
		}
	}
	for i := range candidates {
		// Cleared here as well as on entry to each attempt, because the two
		// clears cover paths the other cannot reach. A candidate can be dropped
		// before any attempt is built — no usable key, nothing to send — and
		// the budget gate below exits the loop mid-iteration; neither reaches
		// attemptOne, and a verdict left over from the previous candidate would
		// be reported as what ended the request when what ended it was running
		// out of candidates or out of time.
		//
		// Placed ABOVE the budget gate for that second reason: a reset after it
		// is skipped exactly when the previous attempt had just left a verdict
		// behind.
		//
		// A known asymmetry follows and is accepted: a budget spent by the
		// previous candidate's last attempt answers with that attempt's
		// verdict when no further candidate exists, but with the budget
		// verdict when one does — entering the next candidate is what retires
		// the old verdict. The alternative (clearing after the gate) would
		// let a chain that ran out of time quote a stale refusal from a
		// candidate it had already moved past, sending the caller to edit a
		// payload that was never the problem.
		rc.attempt.ClearVerdict()
		cand := candidates[i]
		// Per-request budget gate: the wall-clock and count caps span every
		// candidate and key rotation. Checking only at the first attempt left
		// later candidates reachable after a budget had already run out,
		// burning work on a chain that could never succeed. Stop walking as
		// soon as any budget is gone and fall through to the exhausted-chain
		// terminal, which recomputes the same predicate to pick its answer.
		if s.exhaustedBudget(rc) {
			break
		}
		// Entering the candidate replaces the whole attempt state: provider is
		// re-bound only when this candidate's provider proves usable, so a
		// `continue` path (provider missing/disabled, load-keys failed, no
		// enabled key, rewrite failed) doesn't leave a stale provider from a
		// previous iteration on rc — which finalize would otherwise record as
		// the "final hit provider" of an all-failed request — and the same for
		// the dispatch URL. Deliberately AFTER the budget gate above: an
		// iteration that exits there keeps the previous attempt's identity,
		// which is what the audit row is supposed to show.
		rc.attempt.BeginCandidate(&cand)

		// Guard, not gate: filterCandidates already keeps candidates without a
		// provider, or on a switched-off one, out of the chain this loop
		// walks. The checks stay for callers that hand relayCandidates an
		// unfiltered slice — TestRelayCandidatesGuardsUnfilteredSlice does,
		// and pins both skip rows — and cost nothing when idle.
		provider := cand.Provider
		if provider == nil {
			skipDeadEndCandidate(cand, nil, AttemptBadStatus, "provider missing (preload)")
			continue
		}
		if provider.ManagementStatus != model.ProviderStatusEnabled {
			skipDeadEndCandidate(cand, provider, AttemptBadStatus, "provider disabled")
			continue
		}
		// The health record answers before any work is done for the
		// candidate: an open breaker means this provider kept falling over
		// moments ago, and walking it again would spend keys, rewrites and
		// an attempt on an answer the record already knows. After the open
		// window, Allow admits a bounded number of requests as the probes.
		allowed, circuitGen := s.breaker.Allow(provider.ID, provider.DestinationVersion)
		if !allowed {
			skipCandidate(cand, provider, AttemptBadStatus, skipReasonCircuitRefused)
			continue
		}
		rc.circuitGen = circuitGen
		rc.attempt.BindProvider(provider)

		keys, err := repository.ListProviderKeysByProvider(s.db.WithContext(rc.requestCtx), provider.ID)
		if err != nil {
			if isClientDisconnected(c) {
				// The client is gone — stop walking the candidate chain
				// entirely rather than burning the remaining candidates
				// only to land on allCandidatesFailed's 502; record 499
				// instead, mirroring attemptOne's disconnect handling.
				rc.recordAttempt(cand, provider, nil, 0, AttemptConnError, "client disconnected")
				s.abandonRequest(rc, "client_disconnected", start, settleOptions{againstRecordedAttempt: true})
				return
			}
			logger.Error("gateway: list provider keys", zap.String("request_id", rc.requestID), zap.Error(err))
			skipCandidate(cand, provider, AttemptBadStatus, "load keys failed")
			continue
		}
		enabled, anyEnabledKey := filterEnabledKeys(keys)
		if len(enabled) == 0 {
			reason := "no enabled key"
			if anyEnabledKey {
				reason = "no verified key"
			}
			skipDeadEndCandidate(cand, provider, AttemptBadStatus, reason)
			continue
		}
		// Destination-version guard (credential-scope mechanism): a key is
		// only authorized for the provider destination it was verified
		// against. When an admin changes BaseURL, DestinationVersion bumps
		// while existing keys keep their old AuthorizedDestinationVersion —
		// decrypting and sending such a key would exfiltrate the credential
		// to an unapproved destination. Filtered here, BEFORE rotation, like
		// disabled and unverified keys: a stale key occupying a cursor slot
		// would hand its every turn to the same following key and
		// concentrate traffic there instead of round-robining.
		n := 0
		for _, k := range enabled {
			if k.AuthorizedDestinationVersion == provider.DestinationVersion {
				enabled[n] = k
				n++
			}
		}
		if n == 0 {
			skipDeadEndCandidate(cand, provider, AttemptAuthFailed, "destination version mismatch")
			continue
		}
		enabled = enabled[:n]
		// An enabled, verified, destination-authorized key whose ciphertext
		// will not decrypt — corruption, a rotated master secret — is
		// unroutable all the same, and is filtered here with the rest:
		// letting it into the walk would burn a cursor slot on a key that
		// can never dispatch and hand its every turn to the same neighbour.
		// Decryption is deterministic, so this is also the ONLY decrypt:
		// the plaintexts ride alongside into tryKeys, keyed by ID because
		// the walk below reorders the slice. The map lives and dies in this
		// frame — credentials still never park on the Exchange.
		plain := make(map[uint]string, n)
		n = 0
		for _, k := range enabled {
			pt, derr := s.secrets.Decrypt(k.EncryptedKey)
			if derr != nil {
				logger.Warn("gateway: decrypt provider key failed",
					zap.Uint("key_id", k.ID), zap.String("request_id", rc.requestID), zap.Error(derr))
				continue
			}
			plain[k.ID] = pt
			enabled[n] = k
			n++
		}
		if n == 0 {
			skipDeadEndCandidate(cand, provider, AttemptBadStatus, "no decryptable key")
			continue
		}
		enabled = enabled[:n]

		// Step 9: negotiate the wire protocol to speak to this candidate's
		// provider — the ingress protocol when the provider accepts it
		// directly (passthrough, no IR round trip), otherwise the
		// provider's own primary protocol, which the payload decodes to and
		// encodes back from.
		egress, err := Negotiate(ingress, provider)
		if err != nil {
			skipCandidate(cand, provider, AttemptBadStatus, "negotiate egress: "+err.Error())
			continue // mapping failure -> skip candidate
		}

		// The modality answers for this candidate before anything is built for
		// it. A refusal costs one candidate rather than the request, which is
		// the difference between this and the refusals Admit makes.
		offer := Candidate{
			ProviderModelName: cand.ProviderModelName,
			EgressProtocol:    egress.Protocol,
			Passthrough:       egress.Passthrough,
			BaseURL:           egress.BaseURL,
			// Unprobed reads as unsupported rather than supported: a capability
			// nobody has confirmed is one a modality must not be told it has.
			SupportsStreaming:       cand.SupportsStreaming != nil && *cand.SupportsStreaming,
			SupportsFunctionCalling: cand.SupportsFunctionCalling != nil && *cand.SupportsFunctionCalling,
			MaxOutput:               cand.MaxOutput,
		}
		if v := adm.payload.Supports(offer); !v.OK {
			skipCandidate(cand, provider, AttemptBadStatus, v.Reason)
			continue
		}
		// Built once per candidate: it depends on the candidate and the
		// negotiated protocol, not on which key ends up sending it, so every
		// key attempt below reuses the same bytes.
		call, err := adm.payload.PrepareUpstream(offer)
		if err != nil {
			skipCandidate(cand, provider, AttemptBadStatus, "build request: "+err.Error())
			continue // build failure -> skip candidate, nothing sent yet
		}
		// The origin and the credentials stay on this side of the interface:
		// the modality states a path within a provider it was already talking
		// to, and the kernel decides which host that provider is.
		url := protocols.JoinUpstreamURL(egress.BaseURL, call.Path, egress.Protocol)
		if call.OriginRelative {
			url = protocols.OriginURL(egress.BaseURL, call.Path)
		}
		// Rewriters run over the finished egress body, after the modality
		// built it and before anything is sent. A rewriter that refuses comes
		// back as a verdict for this loop to act on, not as an error: what a
		// refusal costs the request is the table's call.
		outBody, verdict := s.rewriteEgress(rc.requestCtx, rc, egress.Protocol, call.Body)
		// Anything as strong as "abandon this candidate" stops the send. The
		// full effect may be more than this path can execute — a terminate
		// verdict also wants a specific status, which the exhausted-chain
		// terminal below cannot always reproduce — but the floor is absolute:
		// once a fact resolved to a verdict this strong, dispatching the body
		// anyway would mean a reported judgement was overridden by omission.
		// Under-executing a verdict is recoverable; ignoring it is not.
		if verdict.Loop >= decision.LoopNextCandidate {
			skipCandidate(cand, provider, AttemptBadStatus,
				"egress rewrite verdict "+verdict.LoopFrom().String())
			continue
		}
		// Retry-same and rotate-key have no meaning before anything was sent;
		// they are logged so the reporting capability's misunderstanding is
		// visible rather than silently absorbed.
		if verdict.Loop > decision.LoopContinue {
			logger.Warn("gateway: reported verdict is not executable before dispatch",
				zap.String("request_id", rc.requestID),
				zap.String("verdict", verdict.LoopFrom().String()))
		}

		// A candidate can also die INSIDE the key loop without a single
		// dispatch — every key stale or undecryptable, or the request
		// impossible to build. Attempts are charged at the wire, so "nothing
		// was dispatched" is visible as an unchanged attempt ledger, and such
		// a candidate is charged the same one probe as any other pre-dispatch
		// abandonment. Without this, a pool full of stale keys would repeat
		// database and decryption work bounded by nothing but the wall clock.
		// The pool decides the order these keys are walked in: rotation
		// spreads load across the pool, benched keys trail healthy ones.
		// Semantics and guarantees live with the type (keypool.go). Ordered
		// HERE, after every pre-dispatch skip above — negotiation, modality
		// refusal, request build, egress-rewrite verdicts — because the
		// cursor advances per walk: consuming a turn for a candidate that
		// then never reaches its keys would skew consecutive real dispatches
		// onto the same key.
		enabled = s.keyPool.walkOrder(provider.ID, enabled)
		attemptsBefore := rc.attemptsSpent
		if s.tryKeys(c, rc, adm, enabled, plain, egress, outBody, url, call, start) == outcomeDone {
			// The response was written by this candidate — a success, or a
			// terminal answer that is the caller's own to act on; either
			// way the provider was reachable and served. A caller whose
			// bound candidate proved a dead end mid-walk re-binds here to
			// the candidate that actually answered — deferred to this
			// point so a walk that never finds a server leaves the old
			// affinity intact instead of deleting it. A walk that ended
			// because the CLIENT hung up proves nothing about the
			// candidate, so a disconnect keeps the old affinity too.
			if rc.binding.invalidated && !isClientDisconnected(c) {
				if served := rc.attempt.Candidate(); served != nil {
					s.bindings.Rebind(rc.apiKeyID, rc.binding.modelID, served.ProviderID, served.ID, rc.binding.candidateID)
					rc.binding.invalidated = false
				}
			}
			return
		}
		if rc.attemptsSpent == attemptsBefore {
			rc.spendBudget(decision.BudgetConsumeProbe)
		}
		// outcomeNextCandidate: fall through to the next candidate.
	}
	s.allCandidatesFailed(c, rc, start)
}

// admitted is what a modality handed back for one request: the payload it built
// and the budgets its modality asked for.
//
// They travel together because a delivery needs both and neither can be derived
// from the other — the payload is per-request and the limits belong to the
// modality that made it.
type admitted struct {
	payload Payload
	limits  TransferLimits
}

// attemptNoteFor is what one attempt's row says happened, in words.
//
// The stable code and the error read differently and both are wanted: a
// dashboard groups by the first, and whoever opens the row needs the second.
// Built here rather than by each delivery path, which is what let four paths
// spell the same failure four ways.
func attemptNoteFor(d fact.Delivery) string {
	if d.FailReason == "" {
		return ""
	}
	// Several construction sites already fold the error's text into the reason
	// they build — "read_body: EOF" carrying the same EOF as its Err. Appending
	// it again writes "read_body: EOF: EOF" into a persisted column, so the
	// suffix is only added when it brings something the reason does not already
	// say. The check matches the folded shape (": " + error, or the error alone)
	// rather than any suffix, so a reason that merely ENDS with the error's
	// words — "client_write_timeout" against a bare "timeout" — keeps its
	// suffix instead of being mistaken for a fold.
	if d.Err == nil || d.FailReason == d.Err.Error() ||
		strings.HasSuffix(d.FailReason, ": "+d.Err.Error()) {
		return d.FailReason
	}
	return d.FailReason + ": " + d.Err.Error()
}

// usageFromReport turns what a modality reported into what the kernel bills on.
//
// The two are separate types on purpose: a modality states quantities in the
// unit it counts, and this is where the kernel decides what to do with them.
// Nil in, nil out — an attempt that reported nothing is not an attempt that
// reported zeros, and billing the difference is real money.
func usageFromReport(u *fact.UsageReported) *protocols.IRUsage {
	if u == nil {
		return nil
	}
	return &protocols.IRUsage{
		PromptTokens:          u.Prompt,
		CompletionTokens:      u.Completion,
		TotalTokens:           u.Total,
		CacheReadTokens:       u.CacheRead,
		CacheWriteTokens:      u.CacheWrite,
		CacheIncludedInPrompt: u.CacheIncludedInPrompt,
		ReasoningTokens:       u.Reasoning,
		Invalid:               u.Incoherent,
		WebSearchCount:        u.WebSearchCount,
	}
}

// relayOutcome is what tryKeys reports back to relayCandidates.
type relayOutcome int

const (
	outcomeDone          relayOutcome = iota // response written, relay finished
	outcomeNextCandidate                     // this candidate's keys are exhausted, try next
)

// tryKeys walks one provider's enabled keys, sending the same pre-built
// upstream body/URL (outBody/url — built once per candidate, by asking the
// payload to prepare it) with each key's own auth header.
// Returns outcomeDone once a response (success OR a non-switchable failure)
// has been written to the client, or outcomeNextCandidate when every key on
// this provider failed with a key-rotation error and the chain should move
// to the next candidate (same-provider no usable key, THEN failover).
func (s *Service) tryKeys(c *gin.Context, rc *Exchange, adm admitted, keys []model.ProviderKey, plain map[uint]string, egress *EgressDecision, outBody []byte, url string, call *UpstreamCall, start time.Time) relayOutcome {
	provider := rc.attempt.Provider()
	// Indexed by hand because one iteration can legitimately not advance: a
	// repaired-body retry re-enters the same key with the new body. One
	// repair per candidate: a capability that kept producing
	// fresh-but-ineffective repairs would otherwise burn the whole attempt
	// budget on one candidate — the kernel does not rely on rewriters being
	// well-behaved. The allowance is passed INTO attemptOne so that an offer
	// it cannot honour is judged unexecutable there, where the failure still
	// gets its full baseline handling — surfacing the upstream's own status —
	// rather than being discarded after the attempt already closed as a
	// retry.
	repairsUsed := 0
	for i := 0; i < len(keys); {
		// The attempt budget spans key rotations too: a rotation that spent
		// the last attempt must not dispatch the next key. Surfacing as a
		// candidate switch lands on the loop gate above, which recognises the
		// exhaustion and ends the walk.
		if rc.attemptsSpent >= s.gateway.MaxUpstreamAttempts {
			return outcomeNextCandidate
		}
		// The admission can be revoked mid-rotation: a fault booked by an
		// earlier key in this very loop can be the one that opens the
		// breaker, and rotating on would dispatch to a provider the record
		// just declared down — with results that arrive pre-revocation
		// stamped stale and discarded. A pure read, so it costs no probe.
		if !s.breaker.StillAllowed(provider.ID, rc.circuitGen) {
			return outcomeNextCandidate
		}
		pk := keys[i]
		// Cleared and bound as each key is ENTERED, not inside attemptOne,
		// so the budget/breaker exits above always report the last entered
		// key and its own verdict — never an earlier key's.
		rc.attempt.ClearVerdict()
		rc.attempt.BindKey(&pk)
		// No per-key skip checks remain here: unroutable keys — disabled,
		// unverified, destination-stale, undecryptable — are all filtered
		// before rotation, which is also why this lookup cannot miss: every
		// walked key decrypted in that filter.
		result, repaired := s.attemptOne(c, rc, adm, plain[pk.ID], egress, outBody, url, call, start, repairsUsed == 0)
		if result == attemptSuccess || result == attemptTerminal {
			return outcomeDone
		}
		if result == attemptRotateKey {
			i++ // next key on the same provider
			continue
		}
		if result == attemptRetrySame {
			// The table judged the repaired body worth another attempt against
			// THIS candidate. The body is candidate-level, exactly like the
			// egress-rewritten one it replaces: the same key retries first,
			// and a later rotation reuses it too.
			repairsUsed++
			outBody = repaired
			continue
		}
		return outcomeNextCandidate
	}
	// Every key failed with a key-rotation error → failover.
	return outcomeNextCandidate
}

// deliverAndSettle hands a 2xx upstream response to the modality and settles
// whatever comes back.
//
// The modality delivers, this side settles. Keeping those apart is what stops
// "how a response is delivered" and "what the request cost and how it is
// recorded" from having to be known in one place.
func (s *Service) deliverAndSettle(c *gin.Context, rc *Exchange, adm admitted, resp *http.Response, call *UpstreamCall, start time.Time) attemptResult {
	// The payloads close the body themselves, from a defer INSIDE Deliver — but
	// a panic can fire before that defer is registered: the call-order wrapper
	// asserts before it forwards, and an assertion tripping there unwinds with
	// the body still open, pinning the upstream connection until the attempt
	// context expires. Closing twice is safe; leaking on the one path that
	// panics before anyone armed a close is not.
	defer func() { _ = resp.Body.Close() }()
	// Delivery tooling is built for the caller's stream ask OR the payload's
	// progressive declaration. They are different questions: a response that
	// cannot be buffered whole must be forwarded as it arrives whether or not
	// the caller marked the request as streaming.
	tools, release := s.newDeliveryTools(c, rc, adm.limits, rc.isStream || call.Progressive)
	defer release()
	return s.recordAndSettle(c, rc, adm, adm.payload.Deliver(tools, resp), resp.StatusCode, start)
}

// recordAndSettle turns what a delivery reported into the request's record.
//
// Separate from the delivery itself because the two answer to different things:
// a modality states what happened, and this decides what the request is
// therefore billed, logged and answered as. Nothing here reads the response
// body — by this point the only evidence is the Delivery.
func (s *Service) recordAndSettle(c *gin.Context, rc *Exchange, adm admitted, d fact.Delivery, upstreamStatus int, start time.Time) attemptResult {
	// The order below is not arrangement. A delivery is checked before it is
	// labelled, because checkAndNote replaces an impossible Delivery with one
	// that says so, and the attempt row is built from whatever it is handed.
	// Label first and the row is built from the original: the outcome still
	// comes out wrong-ish either way, but the fail reason comes out EMPTY,
	// because the reason only exists on the substitute. An operator opening
	// that row to find out what happened is told nothing at all, which is
	// worse than being told the wrong thing.
	sink := newKernelSink(rc)
	s.checkAndNote(rc, &d, sink)
	// A settled, complete delivery is the healthy interaction the table's
	// upstream-succeeded row resets the breaker for. A settled but
	// INCOMPLETE delivery on the provider's fault is the truncated stream,
	// booked at the soft weight its row prices — the request cannot fail
	// over (bytes are committed), so the record is the only thing that can
	// protect the NEXT request from a provider that truncates persistently.
	// A delivery the chain continues past because the provider failed it — a
	// 2xx whose body would not decode, would not read, or died before the
	// first byte — is the full fault the undecodable-payload row penalises.
	// The caller's and the gateway's own faults book nothing against the
	// provider.
	switch {
	case d.Verdict == fact.VerdictSettled && d.Complete:
		s.executeCircuit(rc, decision.CircuitReset)
	case d.Fault != fact.FaultUpstream:
		// Client or gateway fault: blameless for the provider.
	case d.Verdict == fact.VerdictSettled:
		// Routine endings are SUCCESSES, not merely non-faults: a provider
		// that omits the stream terminator after delivering a complete answer
		// is a documented vernacular, and every other ledger records it as
		// served. Booking nothing was tried and is not enough — a provider
		// whose every stream ends that way could never close a half-open
		// breaker, because only successes close one. A genuinely truncated
		// stream pays the soft penalty.
		if settlementIsRoutine(d) {
			s.executeCircuit(rc, decision.CircuitReset)
		} else {
			s.executeCircuit(rc, decision.CircuitPenalizeSoft)
		}
	case d.Verdict == fact.VerdictNextCandidate:
		s.executeCircuit(rc, decision.CircuitPenalize)
	}
	rc.recordCurrentAttempt(upstreamStatus, attemptOutcomeFor(d, rc.isStream), attemptNoteFor(d))
	if d.Verdict == fact.VerdictSettled {
		// The one delivery that ended the request, which is the only one this
		// question is asked about. Asking per attempt would tell the payload the
		// request was over while the chain was still walking.
		//
		// Folded back into the Delivery rather than stashed on the exchange:
		// what a request is billed for belongs to the delivery that ended it,
		// and an attempt that reported tokens and then failed has no way to
		// leave them lying around for a later settlement to pick up.
		d = d.WithUsage(adm.payload.FinalizeUsage(d))
	}
	return s.settleCheckedDelivery(c, rc, d, sink, start)
}

// redactedFailure renders a transport-layer failure for the audit trail with
// the upstream URL redacted.
//
// net/http wraps its failures in a *url.Error carrying the URL it was handed.
// url.Error hides the userinfo password and nothing else — a base URL
// configured with the key in a query parameter comes through intact. That
// string goes into the attempt record and is persisted, so the credential that
// RedactURL strips at dispatch time walks straight back in through the error
// text. Rebuilding the message around the already-redacted URL is what keeps
// the two in step.
func redactedFailure(err error, redactedURL string) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Op + " " + redactedURL + ": " + uerr.Err.Error()
	}
	return err.Error()
}

// attemptResult is what one upstream attempt reports back to tryKeys.
type attemptResult int

const (
	attemptSuccess       attemptResult = iota
	attemptTerminal                    // 4xx client error — surfaced to caller, no switch
	attemptRotateKey                   // 401/429 — try next key
	attemptNextCandidate               // 5xx / conn / timeout — try next candidate
	// attemptRetrySame re-sends a repaired body to the same candidate. Only
	// attemptOne's failure-rewrite path produces it, and only together with
	// the body to re-send; the attempt budget bounds how often it can recur.
	attemptRetrySame
)

// upstreamDecision is one failed upstream response folded into an executable
// answer: which way the chain routes, what the attempt record says, whether
// the kernel's own reading of the status line joined the fold. Every field is
// set by foldUpstreamDecision alone and read through the methods below, so no
// caller can assemble a self-contradictory combination — a terminating
// verdict paired with a next-candidate route, a relabelled outcome riding the
// retry path.
type upstreamDecision struct {
	status     int
	folded     decision.Resolved
	route      attemptResult
	cls        upstreamStatusClass
	noteText   string
	baseFolded bool
	warn       bool
}

func (d upstreamDecision) final() decision.Resolved   { return d.folded }
func (d upstreamDecision) routing() attemptResult     { return d.route }
func (d upstreamDecision) class() upstreamStatusClass { return d.cls }
func (d upstreamDecision) note() string               { return d.noteText }
func (d upstreamDecision) baselineFolded() bool       { return d.baseFolded }
func (d upstreamDecision) warnUnexecuted() bool       { return d.warn }

// sticky returns the verdict worth quoting if the chain then runs out,
// derived rather than stored — the routing decides where the chain goes next,
// the table decides whether this verdict is worth quoting, and keeping one
// answer means the two cannot disagree. A verdict with no status to offer
// returns false: quoting a zero would tell the caller the request ended
// without ever being answered.
func (d upstreamDecision) sticky() (decision.StickyVerdict, bool) {
	if d.folded.Sticky == decision.StickyNone {
		return decision.StickyVerdict{}, false
	}
	status, errType := d.folded.CallerFacing(d.status, d.cls.ErrorType)
	if status == 0 {
		return decision.StickyVerdict{}, false
	}
	return decision.StickyVerdict{
		Status:  status,
		ErrType: errType,
		Detail:  d.folded.RejectDetail(),
		Reason:  d.folded.FailReason(),
	}, true
}

// foldUpstreamDecision folds every opinion about one failed upstream
// response — what the observers and the failure rewriters reported, whether a
// repaired body exists to re-send, whether this candidate may still spend a
// repair — into one executable answer. Pure: the inputs are the opinions and
// the status they were formed from, nothing is read from or written to the
// exchange, and the same inputs always fold to the same decision. The
// side-effecting half (the loud warning, the baseline's timeline entry, the
// circuit booking, the attempt record, the caller-facing surfacing) lives in
// attemptOne, which executes what this decides.
//
// A repair addresses the payload, so it executes only when the status says
// the payload is what failed AND the candidate's repair allowance remains. A
// rejected credential or a throttled key is not a payload problem — no body
// change can address it, and re-sending on the same key would burn the
// budget against a cause the repair cannot touch (a 401 has also just marked
// the key failed, so retrying it would dispatch on a credential already
// known bad). A provider fault is not one either, and neither are the 4xx
// that judge the caller, the account, or the route rather than the bytes.
// And a repair already spent is an answer already given.
//
// A retry-same verdict that cannot be executed is treated as no routing
// opinion, so the kernel's baseline decides: routing, sticky and status all
// together, exactly as if the repair had never been offered. Substituting
// the routing alone was tried and is not enough: it left the baseline's
// sticky behind, and a chain exhausted on rate limits then answered with a
// generic 502 instead of the 429 the caller should back off on.
//
// The kernel is a reporter too. When the observers expressed no executable
// routing opinion, the status line is the only evidence there is, and the
// kernel files its own reading of it through the same vocabulary, so the
// routing comes out of one table however the judgement was reached. The
// baseline is folded in ONLY on that condition, and the asymmetry is
// deliberate: an observer that steered has read the body, the kernel has
// read three digits, and a body-informed verdict must beat a status-informed
// one outright. Folding the two unconditionally would let the baseline's
// "terminate" (any unrecognised 4xx) out-rank a moderation refusal's "try
// the next candidate" — the exact upgrade the observation point exists to
// make possible.
func foldUpstreamDecision(observed decision.Resolved, statusCode int, hasRepair, repairAllowed bool) upstreamDecision {
	cls := classifyUpstreamStatus(statusCode)
	note := fmt.Sprintf("upstream %d", statusCode)

	retryExecutable := observed.Loop == decision.LoopRetrySameCandidate &&
		hasRepair &&
		repairAllowed &&
		payloadRepairableUpstreamStatus(statusCode)
	warn := observed.Loop == decision.LoopRetrySameCandidate && !retryExecutable

	folded := observed
	baseFolded := observed.Loop <= decision.LoopContinue ||
		(observed.Loop == decision.LoopRetrySameCandidate && !retryExecutable)
	if baseFolded {
		baseline := decision.ResolveBatch([]fact.Fact{kernelUpstreamFact(statusCode)})
		folded = decision.Combine(observed, baseline)
	}

	if retryExecutable {
		return upstreamDecision{status: statusCode, folded: folded, route: attemptRetrySame,
			cls: cls, noteText: note, baseFolded: baseFolded, warn: warn}
	}

	// A body-informed refusal relabels the attempt record: the payload was
	// judged, not the provider, and the row should say which happened.
	if folded.LoopFrom() == fact.KindPayloadRefused {
		cls.Outcome = AttemptContentFiltered
		note = fmt.Sprintf("upstream %d content inspection", statusCode)
	}

	// The resolved Loop routes the chain. Terminate is a floor, not an
	// equality: LoopCommitted (bytes already reached the caller) sits above
	// it and lands in the same bucket. The default is unreachable while the
	// baseline fold above holds — every kernel row steers at least as
	// strongly as a key rotation — and routes rather than panicking so a
	// future weakening fails toward failover, the least damaging wrong
	// answer.
	var route attemptResult
	switch {
	case folded.Loop >= decision.LoopTerminate:
		route = attemptTerminal
	case folded.Loop == decision.LoopNextCandidate:
		route = attemptNextCandidate
	case folded.Loop == decision.LoopRotateKey:
		route = attemptRotateKey
	default:
		route = attemptNextCandidate
	}
	return upstreamDecision{status: statusCode, folded: folded, route: route,
		cls: cls, noteText: note, baseFolded: baseFolded, warn: warn}
}

// attemptOne sends one upstream request with one decrypted key and routes
// the response. outBody/url are the pre-built upstream body/URL for this
// candidate — this key's only contribution is the auth header
// (SetupRequest). Transport failures, 5xx,
// and pre-first-byte stream failures are candidate-level (failover); 401/429
// are key-level (rotate); 2xx is success; other 4xx is terminal (caller's
// problem).
// NoteKeyRetestPassed records a completed, PASSED retest of a provider key:
// proof of recovery, delivered by the provider service's commit path (the
// only place proof exists — TestGeneration alone advances when a retest is
// merely claimed, and an inconclusive probe proves nothing). Booked as a
// success observed at observedAt — the caller stamps it BEFORE its probe
// runs, not at callback time, so a 429 that benches the key between the
// probe and this delayed callback stays the newer evidence: like a served
// request, the proof releases only benches older than itself.
func (s *Service) NoteKeyRetestPassed(keyID uint, configVersion int, observedAt time.Time) {
	s.keyPool.clearKey(keyID, configVersion, observedAt)
}

// markProviderKeyForRetest persists a key-scoped upstream rejection as a
// verification failure, so routing stops offering the key and the console
// shows it needs a retest. The CAS uses a context deliberately detached from
// the attempt/request context so that a client disconnect or attempt-deadline
// expiry arriving between the upstream's response headers and this DB write
// cannot cancel the UPDATE — a cancelled CAS would leave a dead key marked as
// valid, causing every subsequent request to burn a full upstream timeout on
// it. WithoutCancel decouples from request cancellation; the 5s budget bounds
// a stuck DB so it cannot hang the goroutine indefinitely. The CAS's own
// version guard (expectedDestinationVersion) already protects against
// concurrent edits, so the detached context is safe.
func (s *Service) markProviderKeyForRetest(ctx context.Context, rc *Exchange, pk *model.ProviderKey, provider *model.Provider) {
	// The invalidation's observation time, taken before the CAS: should this
	// goroutine stall through the DB write, a retest, and fresh verdicts on
	// the recovered key, the late dropKey below must count against the
	// moment the invalidating response was seen, not against now-at-last.
	observed := s.keyPool.stamp()
	casCtx, casCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer casCancel()
	if applied, mErr := repository.MarkProviderKeyVerificationFailedIfCurrent(s.db.WithContext(casCtx), pk.ID, provider.DestinationVersion, pk.ConfigVersion, pk.TestGeneration, time.Now()); mErr != nil {
		logger.Warn("gateway: mark provider key failed",
			zap.Uint("key_id", pk.ID), zap.String("request_id", rc.requestID), zap.Error(mErr))
	} else if !applied {
		// A lost CAS must NOT touch the bench: "lost" can mean a retest
		// already refreshed this key, and a fresh bench installed after that
		// recovery would be deleted here by a stale in-flight response. The
		// winning invalidation owns the bench.
		logger.Debug("gateway: provider key invalidation CAS lost race",
			zap.Uint("key_id", pk.ID), zap.String("request_id", rc.requestID))
	} else {
		// The key left rotation for the retest path, so its transient bench
		// is dropped: retest keeps ConfigVersion, and a leftover bench would
		// still match after a successful retest and demote the recovered key
		// until expiry. On a DB error the key stays routable — and possibly
		// still rate-limited — so the bench stays too.
		s.keyPool.dropKey(pk.ID, pk.ConfigVersion, observed)
	}
}

func (s *Service) attemptOne(c *gin.Context, rc *Exchange, adm admitted, plaintext string, egress *EgressDecision, outBody []byte, url string, call *UpstreamCall, start time.Time, repairAllowed bool) (attemptResult, []byte) {
	// The attempt's identity — candidate, provider, key — lives on rc.attempt,
	// staged by the loops above; plaintext alone stays a parameter, because a
	// credential parked on the exchange would outlive the one call that needs
	// it. Whatever the previous send left behind describes that send, not
	// this one.
	pk := rc.attempt.Key()
	provider := rc.attempt.Provider()
	rc.beginUpstreamAttempt()
	// Stamped from the pool's clock, before the send: if a bench lands on
	// this key between here and the response, the stamp already predates it
	// and this attempt's success cannot release it.
	rc.keyDispatchedAt = s.keyPool.stamp()

	// Per-attempt deadline = min(attempt_timeout, remaining request budget).
	// The request-level budget (RequestDeadline, set at Handle entry) spans
	// all failover candidates; each attempt gets at most its own attempt_timeout,
	// capped by whatever budget remains so a long chain of slow candidates
	// cannot overrun the request cap.
	remaining := time.Until(rc.requestDeadline)
	if remaining <= 0 {
		rc.recordCurrentAttempt(0, AttemptConnError, "request budget exhausted")
		return attemptNextCandidate, nil
	}
	attemptBudget := min(s.gateway.AttemptTimeout, remaining)
	// Derive from rc.requestCtx (carries RequestDeadline) rather than
	// c.Request.Context() directly, so the request-level deadline
	// propagates: when RequestCtx expires, the attempt ctx expires too,
	// cutting the upstream request even mid-stream.
	ctx, cancel := context.WithTimeout(rc.requestCtx, attemptBudget)
	defer cancel()

	// Record the rewritten (provider_model_name) request
	// actually sent upstream, verbatim. Overwritten on every attempt — the
	// last write wins, matching the "successful attempt, else the last
	// attempt" rule.
	rc.bodies.SetUpstreamRequest(outBody)
	// The content type the payload stated for those bytes, kept beside them
	// so the log policy can hand a sanitizer the type (and any multipart
	// boundary in it) the body was actually encoded with.
	rc.upstreamContentType = call.ContentType
	// Record the dispatched URL for the log row and each AttemptRecord.
	// Redacted (userinfo/query/fragment stripped) so a base URL that embeds
	// credentials never reaches the audit log or UI; the raw url is used
	// only for the actual HTTP request below.
	rc.attempt.SetUpstreamURL(protocols.RedactURL(url))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(outBody))
	if err != nil {
		// A build failure here is a candidate-level problem, mirroring the
		// old rewrite-model-failed skip: nothing has been sent, so fail
		// over. Every key on this candidate would fail identically (url is
		// candidate-invariant), so the first key attempt already exhausts
		// this candidate via tryKeys' immediate return on attemptNextCandidate.
		rc.recordCurrentAttempt(0, AttemptBadStatus,
			"build request: "+redactedFailure(err, rc.attempt.UpstreamURL()))
		return attemptNextCandidate, nil
	}
	codecsFor(egress.Protocol).RequestEncoder.SetupRequest(req, plaintext)
	// A content type the payload stated for the body it built is authoritative
	// over the egress codec's. The codec's header is right for the JSON
	// protocols and wrong for a body that is not JSON — a re-encoded multipart
	// carries a boundary nothing but its builder knows — so the override comes
	// after SetupRequest rather than instead of it: the codec still owns every
	// header that is about transport and credentials.
	if call.ContentType != "" {
		req.Header.Set("Content-Type", call.ContentType)
	}

	resp, err := s.client.SendUpstreamRequest(req)
	// The request reached for the wire, and that is what the attempt budget
	// counts. Every row the table prices for a dispatched upstream —
	// transport failure, any status, a 2xx whose delivery later fails —
	// says one attempt, so the charge lives at the dispatch itself rather
	// than on each outcome branch, where the delivery failures that return
	// through the 2xx path used to escape it.
	rc.spendBudget(decision.BudgetConsumeAttempt)
	if err != nil {
		// Caller disconnected mid-request is terminal (can't switch — the
		// caller is gone). Distinguish context.Canceled (client gone) from
		// context.DeadlineExceeded (server-side/per-attempt timeout, which
		// is candidate-level, not a disconnect) so the log labels the right
		// failure class. Any other transport failure is candidate-level.
		if errors.Is(c.Request.Context().Err(), context.Canceled) {
			rc.recordCurrentAttempt(0, AttemptConnError, "client disconnected")
			s.abandonRequest(rc, "client_disconnected", start, settleOptions{againstRecordedAttempt: true}) // nginx-style 499
			return attemptTerminal, nil
		}
		// Reported as the kernel's own fact so the timeline shows the
		// judgement; the attempt was already charged at the dispatch above,
		// and the routing (fail over) matches the row while staying explicit
		// here because the disconnect branch above must keep precedence.
		s.executeCircuit(rc,
			reportKernelFact(rc, fact.Fact{Kind: fact.KindUpstreamTransportFailure}).Circuit)
		rc.recordCurrentAttempt(0, AttemptConnError,
			redactedFailure(err, rc.attempt.UpstreamURL()))
		return attemptNextCandidate, nil
	}

	// Wrap the body with two-phase idle enforcement:
	//   - firstByteTimeout (open -> first chunk): covers the reasoning-model
	//     "flush 200 header then think for minutes" gap that
	//     transport.ResponseHeaderTimeout cannot reach.
	//   - idle (inter-chunk): nginx proxy_read_timeout — a steady reasoning
	//     stream resets the timer on every chunk and stays alive indefinitely,
	//     while a stalled stream is cut.
	// This single wrap point covers the stream relay, the non-stream ReadAll,
	// and the upstream error-body read below — all of which consume resp.Body.
	// The per-attempt ctx carries both the attempt budget and the caller's
	// disconnect, either of which cuts the stream with ctx.Err().
	//
	// Non-2xx error bodies get a short firstByte budget: a stalled retryable
	// 503/429 failover would otherwise burn the full 600s default before
	// ErrFirstByteTimeout surfaces; error bodies are small, so 10s is ample
	// for a healthy upstream while bounding a stuck one.
	if resp.Body != nil {
		// firstByteBudgetFor picks the short errorBodyFirstByteTimeout for
		// any non-2xx status and the full configured firstByteTimeout for
		// 2xx (reasoning models may silently think for minutes before the
		// first token).
		firstByte := firstByteBudgetFor(resp.StatusCode, s.gateway.FirstByteTimeout)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			// 2xx: a steady stream should stay alive indefinitely (up to
			// the request-level deadline), so idle uses the configured
			// BodyIdleTimeout rather than the short error-body budget.
			resp.Body = NewIdleReadCloser(resp.Body, firstByte, s.gateway.BodyIdleTimeout, ctx)
		} else {
			// Non-2xx error body: use a short total read budget so a
			// slow-trickle upstream cannot burn the full attempt_timeout.
			// The idle timeout is also tightened to the same short budget
			// so inter-byte gaps longer than that cut the read short,
			// preventing the "one byte every <idle gap" trickle attack.
			errBodyCtx, errBodyCancel := context.WithTimeout(ctx, errorBodyTotalBudget)
			resp.Body = NewIdleReadCloser(resp.Body, firstByte, firstByte, errBodyCtx)
			// Re-wrap cancel so the deferred cancel below frees it. The
			// original ctx cancel is deferred at the top of attemptOne;
			// errBodyCancel is a nested timeout that must also be released.
			defer errBodyCancel()
		}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// The upstream accepted this key: release its bench NOW, on the
		// status line, not on the delivery verdict — a long response would
		// hold the release for its whole duration, and a delivery that
		// never concludes cleanly (client disconnect mid-stream) would skip
		// it entirely, keeping a key the upstream just accepted demoted.
		// Acceptance already refutes a rate limit; how delivery ends
		// cannot un-refute it.
		s.keyPool.clearKey(pk.ID, pk.ConfigVersion, rc.keyDispatchedAt)
		return s.deliverAndSettle(c, rc, adm, resp, call, start), nil
	}

	statusCode := resp.StatusCode

	// For 401, persist the key verification failure BEFORE reading the
	// error body: the status line alone is proof, and the body read can be
	// cut short by a disconnect.
	if statusCode == http.StatusUnauthorized {
		s.markProviderKeyForRetest(ctx, rc, pk, provider)
	}

	// A 429's bench is likewise booked from the headers, BEFORE the error
	// body is read: the status line and Retry-After carry the whole verdict,
	// and the bounded body read below can still take the full
	// errorBodyTotalBudget against a slow-trickle upstream — waiting would
	// leave concurrent requests dispatching the limited key for that whole
	// window and then start the stated Retry-After late. If the body then
	// reveals an exhausted quota, the invalidation path below drops this
	// bench again — that key is leaving rotation entirely.
	if statusCode == http.StatusTooManyRequests {
		s.keyPool.coolKey(pk.ID, pk.ConfigVersion, rc.keyDispatchedAt,
			cooldownFromRetryAfter(resp.Header.Get("Retry-After"), s.keyPool.stamp(), s.gateway.KeyRateLimitCooldown))
	}

	// Capture the obtainable upstream error body before close, verbatim.
	// Error bodies are small; cap at 1MiB — beyond that is
	// truncation of an error diagnostic, not a response body, and 1MiB is
	// ample for debugging. Unconditionally overwritten (even when empty) so
	// this matches the upstream request capture's "last attempt wins" rule above —
	// an empty errBody from THIS attempt must clear out a stale non-empty body
	// left by an earlier failed candidate, not leave it looking current.
	// A subsequent SUCCESSFUL stream candidate clears it entirely.
	//
	// The body is already wrapped with a short total budget
	// (errorBodyTotalBudget) so a slow-trickle upstream cannot hold this read
	// open beyond that window.
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	rc.bodies.SetUpstreamResponse(errBody)

	// A 429 whose body claims exhausted quota or billing is not the
	// transient rate limit its status pretends to be: the key keeps failing
	// until the account is topped up. Mark it for retest exactly as a 401
	// is — rotation already carries this request to the next key — so
	// routing stops burning attempts on a key that cannot pay. The
	// invalidation also drops the bench booked from the headers above: the
	// key is leaving rotation, and a leftover bench would outlive the
	// retest. A plain rate-limit 429 keeps that bench and stays unmarked —
	// it heals on its own, and flagging it would take a healthy key out of
	// rotation. The current request keeps rotating per the table; the bench
	// only reorders later walks.
	if statusCode == http.StatusTooManyRequests && quotaExhaustedBody(errBody) {
		s.markProviderKeyForRetest(ctx, rc, pk, provider)
	}
	_ = resp.Body.Close()

	// Observers get the response and report what they recognise in it; the
	// decision table turns those reports into a verdict. A terminal
	// classification can be upgraded to a failover this way — status alone
	// cannot tell a moderated payload apart from a malformed one, only the body
	// can — so the upgrade happens here, after the read above, and before the
	// attempt record is appended, so the log shows why the chain continued.
	// The verdict is remembered for allCandidatesFailed: if every candidate
	// moderates the payload, the caller must still get that verdict rather than
	// a generic 502 that reads like an outage.
	up := fact.Upstream{
		StatusCode: statusCode,
		Header:     resp.Header,
		Body:       errBody,
		Elapsed:    time.Since(start),
	}
	observed := s.observeUpstreamError(ctx, rc, up)
	// Failure rewriters see the same failed response and may offer a
	// repaired body. What they report folds with what the observers
	// reported: one verdict, however many mouths spoke.
	repaired, offered := s.rewriteAfterFailure(ctx, rc, egress.Protocol, outBody, up)
	observed = decision.Combine(observed, offered)

	d := foldUpstreamDecision(observed, statusCode, repaired != nil, repairAllowed)

	// A retry-same verdict that cannot be executed — no repaired body behind
	// it, or a failure no payload repair can address — is logged loudly; the
	// fold above has already treated it as no routing opinion.
	if d.warnUnexecuted() {
		logger.Warn("gateway: reported verdict is not executed on this path",
			zap.String("request_id", rc.requestID),
			zap.String("verdict", observed.LoopFrom().String()),
			zap.Int("upstream_status", statusCode))
	}
	// The kernel's own reading joins the fold only on the decision's say-so,
	// and joining is also an audit fact: the report appends one timeline
	// entry, stamped with kernel provenance by construction. The verdict is
	// discarded — d already folded this reading; only the entry matters here.
	if d.baselineFolded() {
		reportKernelFact(rc, kernelUpstreamFact(statusCode))
	}
	// The resolved circuit effect is booked whatever the routing below
	// decides: the provider's health record describes the provider, not
	// where this particular chain goes next.
	s.executeCircuit(rc, d.final().Circuit)
	// The retry-same executor. The table judged a repaired body worth
	// another attempt against this candidate, the failure was one a repair
	// can address, and the body to re-send exists: the attempt is recorded
	// and the pair goes back to the key loop, whose budget gate bounds how
	// often a repair can recur. The record carries the unrelabelled class —
	// the repair path describes the provider's own answer, not a payload
	// judgement.
	if d.routing() == attemptRetrySame {
		rc.recordCurrentAttempt(statusCode, d.class().Outcome, d.note())
		return attemptRetrySame, repaired
	}
	if sticky, ok := d.sticky(); ok {
		rc.attempt.HoldVerdict(sticky)
	}
	rc.recordCurrentAttempt(statusCode, d.class().Outcome, d.note())

	switch d.routing() {
	case attemptTerminal:
		// The caller's request is the problem, or a reporter ended the chain:
		// surfaced as-is, no rotation, no failover.
		res, cls := d.final(), d.class()
		status, errType := res.CallerFacing(statusCode, cls.ErrorType)
		if status == 0 {
			status, errType = statusCode, cls.ErrorType
		}
		if !c.Writer.Written() {
			detail := res.RejectDetail()
			if detail == "" {
				detail = safeUpstreamMessage(status)
			}
			WriteIngressError(c, rc.ingress, status, errType, detail, rc.requestID)
		}
		s.settle(rc, fact.Rejected(status, fact.FaultUpstream,
			res.FailReason(), nil), start, settleOptions{againstRecordedAttempt: true})
		return attemptTerminal, nil
	default:
		// attemptNextCandidate and attemptRotateKey pass straight through;
		// anything unrecognized was already collapsed to next-candidate by
		// the fold, the least damaging wrong answer.
		return d.routing(), nil
	}
}

// allCandidatesFailed is reached only when every candidate was tried without
// a response being written — the writer is guaranteed not yet written, but
// the guard is kept defensively in case a future caller changes that.
func (s *Service) allCandidatesFailed(c *gin.Context, rc *Exchange, start time.Time) {
	if c.Writer.Written() {
		status := rc.statusCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		s.settle(rc, fact.Truncated(status, status, fact.FaultUpstream,
			"partial_then_exhausted", nil), start, settleOptions{})
		return
	}
	// The chain ended on a verdict somebody thought worth quoting — a payload
	// refusal is the motivating case. That is a judgement on the request, not
	// an outage, so the caller is shown what was actually said: a 502 "all
	// upstream candidates failed" would send them looking for a broken provider
	// instead of at their own prompt.
	//
	// One slot, holding the last attempt's verdict: whoever ends the chain
	// leaves theirs behind, and everyone before them has already had theirs
	// overwritten or cleared.
	if v := rc.attempt.Verdict(); v.Held() {
		detail := v.Detail
		if detail == "" {
			detail = "upstream refused this request"
		}
		s.rejectRequest(c, rc, v.Status, v.ErrType, detail, v.Reason, fact.FaultUpstream, start)
		return
	}
	// The chain ended with a budget — wall-clock or count — spent, and the
	// table's request-budget row supplies the verdict for that. Recomputed
	// here rather than flagged at the loop gate, because the gate only sees
	// exhaustion when ANOTHER candidate iteration reaches it: a budget spent
	// by the final candidate would otherwise change the answer depending on
	// whether an unused candidate happened to remain. The sticky check above
	// deliberately still wins: a held verdict names what actually went wrong
	// before the allowance ran dry, and that is the answer the caller can
	// act on.
	if s.exhaustedBudget(rc) {
		// Reason is an explicit stable code (persisted, mapped in the
		// frontend), never derived from the Kind's internal name.
		v := reportKernelFact(rc,
			fact.Fact{Kind: fact.KindRequestBudgetExhausted, Reason: "request_budget_exhausted"})
		status, errType := v.CallerFacing(0, "")
		s.rejectRequest(c, rc, status, errType,
			"request budget exhausted", v.FailReason(), fact.FaultUpstream, start)
		return
	}
	s.rejectRequest(c, rc, http.StatusBadGateway, errTypeUpstream,
		"all upstream candidates failed", "all_candidates_failed", fact.FaultUpstream, start)
}

// reorderBalanced puts the caller key's bound candidate at the head of the
// chain and leaves the rest in sort_order relative order. It is the whole of
// what balanced scheduling changes on the hot path: everything downstream —
// failure handling, key rotation, circuit booking, budgets — is the same
// machinery a failover chain runs, so the mode's only degree of freedom is
// which candidate the walk enters first.
//
// A zero return from Route (no bindings wired, or every provider currently
// dead) keeps the caller's slice untouched: sort_order order, failover
// behaviour — the degrade is "not balanced right now", never "not routed".
func (s *Service) reorderBalanced(rc *Exchange, modelID uint, candidates []model.ModelCandidate) []model.ModelCandidate {
	if len(candidates) < 2 {
		// A single-candidate chain has nothing to balance; Route would bind
		// it all the same, but the binding would only burn LRU capacity —
		// the chain's order is forced either way.
		return candidates
	}
	// The routable filter guarantees a preloaded, enabled provider on every
	// candidate, so each provider's current destination version is at hand —
	// the snapshot must not hold a repaired destination's predecessor
	// against it.
	destByProvider := make(map[uint]int, len(candidates))
	for _, cand := range candidates {
		if cand.Provider != nil {
			destByProvider[cand.ProviderID] = cand.Provider.DestinationVersion
		}
	}
	first := s.bindings.Route(rc.apiKeyID, modelID, candidates, func(providerID uint) bool {
		return s.breaker.IsOpen(providerID, destByProvider[providerID])
	})
	if first == 0 {
		return candidates
	}
	rc.binding.modelID, rc.binding.candidateID = modelID, first
	for i, cand := range candidates {
		if cand.ID != first {
			continue
		}
		if i == 0 {
			return candidates
		}
		out := make([]model.ModelCandidate, 0, len(candidates))
		out = append(out, candidates[i])
		out = append(out, candidates[:i]...)
		out = append(out, candidates[i+1:]...)
		return out
	}
	return candidates
}

// filterCandidates returns the subset of candidates eligible for this request:
// on an enabled provider, management-enabled, verification-passed. Order is
// preserved (sort_order was applied by the repository) so failover still walks
// the chain in the admin's configured order.
//
// It deliberately does NOT consult the streaming / function-calling capability
// flags. Those are recorded for the admin UI only. Filtering on them looks
// appealing but cannot be made to pay off here: a request this gate rejects
// produces "model is not available" with no attempt at all, while a capability
// the upstream genuinely lacks is reported as a 4xx, which attemptOne classifies
// as terminal — so excluding a candidate cannot be recovered by failover either
// way. Meanwhile a probe that merely failed to confirm a capability would take a
// working candidate out of rotation. Letting the upstream be the authority costs
// one failed request in the rare genuine case and removes a whole class of
// self-inflicted outages.
//
// anyEnabled is reported in the same pass so the caller can tell "all disabled"
// apart from "enabled but unverified" without walking the slice twice. A
// candidate on a switched-off provider counts toward neither: it is
// configuration an operator turned down — the "no enabled route" answer — not
// a route waiting on verification.
func filterCandidates(all []model.ModelCandidate) (routable []model.ModelCandidate, anyEnabled bool) {
	for _, c := range all {
		// The provider gate comes first, as it does in
		// modeladmin.CandidateBlockedBy, which also rules on the provider
		// before the candidate's own state. A candidate whose provider is
		// switched off must not enter the chain at all: a chain position is
		// not free — walking it later costs a probe of the request budget and
		// a "provider disabled" attempt row — and enough of them sorted ahead
		// of a live provider can spend the whole budget before the request
		// reaches the provider that would have served it. A nil provider
		// (broken association, a missed preload) falls to the same gate.
		if c.Provider == nil || c.Provider.ManagementStatus != model.ProviderStatusEnabled {
			continue
		}
		if c.ManagementStatus != model.ModelCandidateStatusEnabled {
			continue
		}
		anyEnabled = true
		// An enabled-but-unverified candidate is NOT routable. The two states can
		// coexist — a candidate is stored before its first probe, and a probe can
		// reset verification without touching enablement — and ModelService's own
		// routability check (modeladmin.CandidateBlockedBy) already rejects these, so the
		// gateway must match that gate or it routes a mapping known to be
		// unverified.
		if c.VerificationStatus != model.ModelVerificationStatusPassed {
			continue
		}
		routable = append(routable, c)
	}
	return routable, anyEnabled
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

// recordCurrentAttempt records the attempt rc.attempt currently describes —
// the form every post-BeginCandidate path uses, so the identity on the row
// and the identity on the state cannot disagree. The explicit recordAttempt
// below stays for the loop's pre-bind skip rows, which deliberately name a
// provider the state never bound (a provider missing or switched off is on
// the row so the operator sees who was skipped — a guard path now that
// filterCandidates excludes such candidates before the chain — and off the
// state so finalize never reports it as the final hit).
func (rc *Exchange) recordCurrentAttempt(status int, outcome, failReason string) {
	rc.recordAttempt(*rc.attempt.Candidate(), rc.attempt.Provider(), rc.attempt.Key(), status, outcome, failReason)
}

// recordAttempt builds one AttemptRecord and appends it to the exchange's
// attempt log — the one place that log grows, so every recorded try passes
// through the same construction. provider and key are nil-able: nil provider
// marks a candidate whose provider was missing/disabled; nil key marks a
// candidate-level failure before any key was tried (load failed, no enabled
// key, rewrite failed).
//
// It is an Exchange method so it can stamp the attempt with
// rc.attempt.UpstreamURL() — the URL the gateway dispatched to for this
// attempt — without every caller threading the URL through. That URL is reset
// per candidate in relayCandidates and set in attemptOne, so it reflects the
// current attempt: empty for attempts that failed before any request was
// sent.
func (rc *Exchange) recordAttempt(cand model.ModelCandidate, provider *model.Provider, key *model.ProviderKey, status int, outcome, failReason string) {
	rec := AttemptRecord{
		CandidateID:       cand.ID,
		ProviderModelName: cand.ProviderModelName,
		StatusCode:        status,
		Outcome:           outcome,
		FailReason:        failReason,
		UpstreamURL:       rc.attempt.UpstreamURL(),
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
	rc.attempts = append(rc.attempts, rec)
}
