package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/decision"
	"github.com/yolorouter/yolorouter/internal/fact"
)

// relayContextKey is the gin.Context key Handle stores the in-flight
// Exchange under (relay.go: c.Set(relayContextKey, rc)), so
// WriteIngressError can stash the local error JSON it is about to return
// into the response-body capture without threading an
// *Exchange parameter through every call site. Absent on paths that
// never call Handle (e.g. unit tests, or middleware.APIKeyAuth's own 401s
// before Handle ever runs) — the stash is then a no-op.
const relayContextKey = "relay_context"

// relayContextFrom retrieves the in-flight Exchange Handle stashed on c
// (see relayContextKey), or nil when none is present — e.g. a path that
// never called Handle (unit tests, or an early 401 in middleware.APIKeyAuth
// before Handle ever runs). The single lookup + two-step type assertion
// every local-error stash path needs.
func relayContextFrom(c *gin.Context) *Exchange {
	v, ok := c.Get(relayContextKey)
	if !ok {
		return nil
	}
	rc, _ := v.(*Exchange)
	return rc
}

// Caller-facing error "type" values. The canonical constants live in the
// decision package — its table speaks them — and are aliased here so the many
// envelope call sites in this package stay short.
const (
	errTypeAuthentication    = decision.ErrTypeAuthentication
	errTypePermission        = decision.ErrTypePermission
	errTypeRateLimit         = decision.ErrTypeRateLimit
	errTypeInvalidRequest    = decision.ErrTypeInvalidRequest
	errTypeNotFound          = decision.ErrTypeNotFound
	errTypeUpstream          = decision.ErrTypeUpstream
	errTypeServer            = decision.ErrTypeServer
	errTypeUnavailable       = decision.ErrTypeUnavailable
	errTypeInsufficientQuota = decision.ErrTypeInsufficientQuota
)

// StatusClientClosedRequest is the non-standard status used to record that the
// caller went away before the response was delivered. It is never written to
// the wire — there is no caller left to receive it — but it must be
// distinguishable in the audit row from a gateway fault, because the two demand
// opposite responses from whoever reads it.
const StatusClientClosedRequest = decision.StatusClientClosedRequest

// statusCategory classifies a non-2xx upstream HTTP status into the relay
// loop's three branches: rotate to another Key on the same
// provider, failover to the next candidate, or surface as terminal (no
// switch).
type statusCategory int

const (
	statusRotateKey      statusCategory = iota // 401/429: Key-scoped, try next key
	statusFailover                             // 5xx: provider-scoped, try next candidate
	statusTerminalClient                       // other 4xx: caller's problem, no switch
)

// ErrorType is stated on EVERY classification, including the two that used to
// leave it empty because their callers never read it. A verdict a capability
// reports can now carry any of these statuses to the terminal, and an empty
// error type reaches the caller as `"type": ""` — a field their client library
// will branch on and find nothing. What used to protect this was a caller that
// only ever asked on the one path that filled it in; that protection is not
// something the type can state, so the type states the value instead.
//
// upstreamStatusClass is the full classification attemptOne needs from one
// upstream HTTP status: which branch to take, what outcome label to log,
// and (for terminal 4xx) which OpenAI error type to surface.
type upstreamStatusClass struct {
	Category  statusCategory
	Outcome   string
	ErrorType string
}

// kindForUpstreamStatus is the kernel's own reading of a non-2xx upstream
// status, spoken in the fact vocabulary so the decision table can resolve it
// like any other report. It exists because the kernel is itself a reporter:
// when no observer recognised anything in the response body, the status line
// is the only evidence there is, and the routing it implies must come out of
// the same table as everything else rather than out of a parallel switch.
//
// This is the single reading of "which statuses mean what":
// classifyUpstreamStatus derives the label-side vocabulary (attempt outcome,
// caller-facing error type) from the kind returned here, so the two cannot
// disagree by construction.
func kindForUpstreamStatus(status int) fact.Kind {
	switch {
	case status == http.StatusUnauthorized:
		return fact.KindUpstreamAuthRejected
	case status == http.StatusTooManyRequests:
		return fact.KindUpstreamRateLimited
	case status >= 500:
		return fact.KindUpstreamServerError
	default:
		return fact.KindUpstreamClientError
	}
}

// payloadRepairableUpstreamStatus reports whether a non-2xx status describes
// a failure a body repair could plausibly address. It is deliberately a
// whitelist: a malformed request, an oversized payload, or an unprocessable
// entity are statements about the BYTES, and different bytes can change the
// answer. Everything else a provider says with a 4xx — no permission (403),
// no funds (402), no such route (404), wrong method (405), and notably an
// unsupported media type (415, which judges the Content-Type header a
// rewriter cannot touch, not the body) — is a statement about the caller,
// the account, the URL, or request metadata, and re-sending edited bytes
// cannot change it: retrying there would replace an actionable status with a
// budget-exhaustion 504 while loading the upstream for nothing.
func payloadRepairableUpstreamStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// kernelUpstreamFact is the kernel's baseline report for a non-2xx response,
// built whole in one place so the fact and its persisted audit code cannot
// drift apart.
//
// Reason is set explicitly for every reading that can end up persisted as a
// fail_reason — a client error surfaced to the caller, a rate limit quoted
// when the chain exhausts. The persisted column is a contract with dashboards
// and log viewers, so the code must never be derived from the Kind's name:
// Kind names are internal and get renamed as the vocabulary is refined, and a
// rename must not silently change a column somebody queries. Readings that
// are never persisted leave Reason empty.
func kernelUpstreamFact(status int) fact.Fact {
	kind := kindForUpstreamStatus(status)
	switch kind {
	case fact.KindUpstreamRateLimited:
		return fact.Fact{Kind: kind, Status: status, Reason: "upstream_rate_limited"}
	case fact.KindUpstreamClientError:
		return fact.Fact{
			Kind:   kind,
			Status: status,
			Reason: fmt.Sprintf("upstream_client_error_%d", status),
		}
	default:
		return fact.Fact{Kind: kind, Status: status}
	}
}

// classifyUpstreamStatus maps a non-2xx upstream status to its relay
// classification — the label-side vocabulary (routing category, attempt
// outcome, caller-facing error type) for the reading kindForUpstreamStatus
// gives in the fact vocabulary. Deriving from the kind rather than
// re-reading the status is what makes "which statuses mean what" a single
// definition, and only this direction of derivation works: the kind
// distinguishes 401 from 429 while the rotate-Key category folds them
// together.
//
// 403 is intentionally NOT a rotate-Key status: a 403 from an
// OpenAI-compatible provider is usually account/permission scoped (the whole
// provider is forbidden), so rotating Keys within it is futile and we fall
// through to terminal.
func classifyUpstreamStatus(status int) upstreamStatusClass {
	switch kindForUpstreamStatus(status) {
	case fact.KindUpstreamAuthRejected:
		return upstreamStatusClass{Category: statusRotateKey, Outcome: AttemptAuthFailed, ErrorType: errTypeAuthentication}
	case fact.KindUpstreamRateLimited:
		return upstreamStatusClass{Category: statusRotateKey, Outcome: AttemptRateLimited, ErrorType: errTypeRateLimit}
	case fact.KindUpstreamServerError:
		return upstreamStatusClass{Category: statusFailover, Outcome: AttemptServerError, ErrorType: errTypeUpstream}
	default:
		errType := errTypeInvalidRequest
		switch status {
		case http.StatusNotFound:
			errType = errTypeNotFound
		case http.StatusForbidden:
			errType = errTypePermission
		}
		return upstreamStatusClass{Category: statusTerminalClient, Outcome: AttemptClientError, ErrorType: errType}
	}
}

// quotaExhaustedBody reports whether an upstream 429 error body claims an
// account that cannot pay, as opposed to transient rate limiting. The verdict
// sets routing state — a match takes the key out of rotation until a retest —
// so the signal set is deliberately narrower than the connection tester's
// display-only labels: the explicit "insufficient_quota" enum in error.code
// (OpenAI's convention; a string or a number depending on the upstream, hence
// the loose decode) or in error.type (OpenAI sets both; a Yolorouter upstream
// reporting budget exhaustion sets only the type), or "billing"/"credit" in
// the message. The bare word "quota" is deliberately NOT a signal:
// Gemini-style upstreams describe ordinary per-minute throttling as "Quota
// exceeded for quota metric ...", and matching it would strand a healthy key
// on a limit that heals itself.
func quotaExhaustedBody(body []byte) bool {
	var parsed struct {
		Error *struct {
			Code    any    `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Error == nil {
		return false
	}
	if code, ok := parsed.Error.Code.(string); ok && code == "insufficient_quota" {
		return true
	}
	if parsed.Error.Type == "insufficient_quota" {
		return true
	}
	msg := strings.ToLower(parsed.Error.Message)
	return strings.Contains(msg, "billing") || strings.Contains(msg, "credit")
}
