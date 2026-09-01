package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// The order Payload's methods are called in is part of its contract, not an
// implementation detail of the kernel.
//
// Supports is allowed to work something out about a candidate and leave it on
// the Payload for PrepareUpstream to read back — a codec choice, a re-encoded
// body worth not building twice. That makes the two methods coupled through
// state rather than through their arguments, and state-coupled methods called
// in the wrong order do not fail: they read whatever the LAST call left behind.
// A body prepared for one candidate and sent to another is a request that
// looks entirely well-formed and is addressed to the wrong provider.
//
// So the order is asserted rather than documented. The kernel is what makes
// these calls, so a violation is a bug in the kernel and the assertion is
// aimed at us, not at modality authors.

// errNoUpstreamCall marks a build that reported success and produced nothing.
var errNoUpstreamCall = errors.New("gateway: PrepareUpstream returned no call and no error")

// errPayloadBusy is what a method returns instead of running when another call
// on the same payload has not finished. It is a gateway fault: the payload did
// nothing wrong, something called it twice at once.
var errPayloadBusy = errors.New("gateway: payload is already running another call")

// payloadStage is how far through its life a payload has got.
type payloadStage uint8

const (
	stageAdmitted payloadStage = iota
	stageRouted
	stageEstimated
	stageSupported
	stagePrepared
	// stageDelivered is terminal for the attempt cycle: a delivery that
	// reached the caller, or that ended the request, leaves nothing for
	// another candidate to do.
	stageDelivered
	stageFinalized
)

func (s payloadStage) String() string {
	switch s {
	case stageRouted:
		return "routed"
	case stageEstimated:
		return "estimated"
	case stageSupported:
		return "supported"
	case stagePrepared:
		return "prepared"
	case stageDelivered:
		return "delivered"
	case stageFinalized:
		return "finalized"
	default:
		return "admitted"
	}
}

// orderedPayload wraps a Payload and refuses calls that arrive out of order.
//
// Ordering violations panic. Those calls come from the kernel, on the request
// goroutine, where the recovery middleware turns the panic into a 500 and
// Handle's own defer still writes the audit row — contained to the one request
// and impossible to overlook. An assertion that only logged would let exactly
// the bug it watches for reach a caller, and this class of bug produces a
// plausible-looking response rather than an error.
//
// One qualification: once a response is committed, no 500 can be sent, and the
// recovery middleware re-panics with http.ErrAbortHandler so the caller gets a
// severed connection instead. That is the right outcome for the case it
// arises in — refusing to serve a second candidate on top of a response the
// caller already has — but it is a dropped connection, not a tidy error.
//
// The overlap check in enter is the one exception, and for the opposite reason
// — see there.
type orderedPayload struct {
	inner Payload
	// requestID is carried only so a refusal can be traced back to the request
	// it happened on.
	requestID string
	stage     payloadStage
	// supported is the candidate the last successful Supports call was about.
	// PrepareUpstream is checked against it by value: "some Supports ran" is
	// not the guarantee that matters, "Supports ran for THIS candidate" is.
	supported Candidate
	// inCall detects two calls overlapping. The contract is that every method
	// runs on one goroutine, serially; a second entry while the first has not
	// returned means something is calling a payload from a goroutine it does
	// not own, and the memoized state has no protection whatsoever.
	//
	// It is a detector, not a guard: it reports the violation but cannot undo
	// the unsynchronised access that has already happened. The fields beside it
	// are plain, so a genuine race is still a race — what this buys is that the
	// bug gets a name in the log instead of showing up later as an inexplicable
	// response.
	inCall atomic.Bool
}

func newOrderedPayload(p Payload, requestID string) *orderedPayload {
	return &orderedPayload{inner: p, requestID: requestID}
}

// enter marks a call in flight. The second return is false when another call is
// already running, and the method must then run none of its body.
//
// What the wrapper hands back in that case is a value the caller cannot always
// tell apart from a real answer -- an empty RoutingIntent looks like a request
// with no model. Every one of them is chosen to be the safe reading: no model
// routes nowhere, an unknown cost estimates nothing, a zero LogPolicy stores
// nothing. Supports, PrepareUpstream and Deliver do carry an explicit refusal,
// because for those a wrong reading would send a request.
//
// Not a panic, and not a warning it proceeds past either. The case this detects
// is a modality calling back into its own payload from a goroutine it spawned,
// and a panic there happens on THAT goroutine, where the request's recovery
// cannot reach it and the process goes down with it — so the first version
// logged and carried on. That was worse than useless: the method body it let
// through runs the ordering assertions, which panic on the same foreign
// goroutine, and touches the same unguarded state the detector exists to
// protect. Refusing to run the body is what makes the detector a guard.
func (p *orderedPayload) enter(method string) (func(), bool) {
	if !p.inCall.CompareAndSwap(false, true) {
		logger.Error("gateway: payload method called while another call is still running",
			zap.String("method", method),
			zap.String("detail", "a payload is serial and its memoized state is unguarded"))
		// Nothing to release: the call already in flight owns the flag, and
		// clearing it here would let a third caller in.
		return func() {}, false
	}
	return func() { p.inCall.Store(false) }, true
}

// require asserts the payload has reached at least the stage a method needs.
func (p *orderedPayload) require(method string, at payloadStage) {
	if p.stage < at {
		panic(fmt.Sprintf("gateway: payload method %s called at stage %q, needs at least %q",
			method, p.stage, at))
	}
}

// refuseAfterFinalize stops any delivery work once usage has been stated.
// Everything downstream of that point has already read the numbers.
func (p *orderedPayload) refuseAfterFinalize(method string) {
	if p.stage == stageFinalized {
		panic(fmt.Sprintf("gateway: payload method %s called after usage was finalized", method))
	}
}

// refuseAfterDelivered additionally stops the attempt cycle once a delivery has
// ended the request.
//
// The stage number alone cannot express this: stageDelivered is higher than
// every stage the cycle's own require calls ask for, so without this the
// checks would all pass and a second candidate would be dispatched on top of a
// response the caller has already received. That is a duplicate charge, and
// for anything with a side effect, a duplicate execution.
func (p *orderedPayload) refuseAfterDelivered(method string) {
	if p.stage == stageDelivered {
		panic(fmt.Sprintf("gateway: payload method %s called after a delivery ended the request", method))
	}
}

func (p *orderedPayload) Routing() RoutingIntent {
	release, ok := p.enter("Routing")
	defer release()
	if !ok {
		return RoutingIntent{}
	}
	p.refuseAfterFinalize("Routing")
	if p.stage < stageRouted {
		p.stage = stageRouted
	}
	return p.inner.Routing()
}

func (p *orderedPayload) EstimateCost(pv PricingView) CostEstimate {
	release, ok := p.enter("EstimateCost")
	defer release()
	if !ok {
		return CostEstimate{}
	}
	p.refuseAfterFinalize("EstimateCost")
	p.require("EstimateCost", stageRouted)
	if p.stage < stageEstimated {
		p.stage = stageEstimated
	}
	return p.inner.EstimateCost(pv)
}

func (p *orderedPayload) Supports(cand Candidate) CandidateVerdict {
	release, ok := p.enter("Supports")
	defer release()
	if !ok {
		return CandidateVerdict{OK: false, Reason: "payload is busy on another call"}
	}
	p.refuseAfterFinalize("Supports")
	p.refuseAfterDelivered("Supports")
	p.require("Supports", stageEstimated)
	v := p.inner.Supports(cand)
	if v.OK {
		p.supported = cand
		p.stage = stageSupported
		return v
	}
	// A refused candidate must not leave the payload looking ready to prepare
	// one. Falling back to estimated is what makes the next PrepareUpstream
	// fail loudly instead of building a body against a candidate that was
	// turned down.
	p.supported = Candidate{}
	p.stage = stageEstimated
	return v
}

func (p *orderedPayload) PrepareUpstream(cand Candidate) (*UpstreamCall, error) {
	release, ok := p.enter("PrepareUpstream")
	defer release()
	if !ok {
		return nil, errPayloadBusy
	}
	p.refuseAfterFinalize("PrepareUpstream")
	p.refuseAfterDelivered("PrepareUpstream")
	p.require("PrepareUpstream", stageSupported)
	if cand != p.supported {
		panic("gateway: PrepareUpstream called for a candidate that Supports did not accept: " +
			"anything Supports memoized belongs to a different candidate")
	}
	call, err := p.inner.PrepareUpstream(cand)
	if err == nil {
		if call == nil {
			// Reported success and produced nothing. Left alone this advances
			// to prepared, so the kernel would dereference a request that does
			// not exist and Deliver would already be unlocked.
			err = errNoUpstreamCall
		} else {
			err = validateUpstreamPath(call.Path)
		}
	}
	if err != nil {
		// The build failed, so there is nothing to deliver and this candidate
		// is over. Staying at supported would let a Deliver through on a body
		// that was never built.
		p.stage = stageEstimated
		p.supported = Candidate{}
		return nil, err
	}
	p.stage = stagePrepared
	return call, nil
}

func (p *orderedPayload) Deliver(tools DeliveryTools, resp *http.Response) fact.Delivery {
	release, ok := p.enter("Deliver")
	defer release()
	if !ok {
		// Still reconciled: the call already in flight may have committed a
		// response, and reporting "nothing reached the caller" for a caller who
		// has bytes is the same lie this function exists to catch.
		return reconcileWithResponse(tools.Client,
			fact.Undelivered(http.StatusInternalServerError, fact.VerdictSettled,
				fact.FaultGateway, "payload_busy", errPayloadBusy), p.requestID)
	}
	p.refuseAfterFinalize("Deliver")
	p.refuseAfterDelivered("Deliver")
	p.require("Deliver", stagePrepared)
	d := p.inner.Deliver(tools, resp)
	d = reconcileWithResponse(tools.Client, d, p.requestID)
	p.supported = Candidate{}
	// The delivery says whether the request is over, and this is the one place
	// holding that answer at the moment it matters. Committed means bytes
	// reached the caller and a settled verdict means the request ended; either
	// way no other candidate can serve it, and the next stage has to refuse
	// one. Only a delivery that reached nobody sends the cycle round again.
	if d.Committed || d.Verdict == fact.VerdictSettled {
		p.stage = stageDelivered
		return d
	}
	p.stage = stageEstimated
	return d
}

// NormalizeUpstreamError is deliberately unordered. An upstream can fail at any
// point a request has reached it, including before a candidate was ever
// accepted, and refusing to translate an error because of what stage a payload
// is at would leave the kernel holding a failure it cannot describe.
func (p *orderedPayload) NormalizeUpstreamError(status int, body []byte, ct string) ErrorEnvelope {
	release, ok := p.enter("NormalizeUpstreamError")
	defer release()
	if !ok {
		// Status 0 would reach a header writer that treats it as a programming
		// error; an envelope has to name a status even when we could not ask
		// for one.
		return ErrorEnvelope{Status: http.StatusInternalServerError, ErrorType: errTypeServer,
			Message: "internal error"}
	}
	return p.inner.NormalizeUpstreamError(status, body, ct)
}

func (p *orderedPayload) FinalizeUsage(d fact.Delivery) *fact.UsageReported {
	release, ok := p.enter("FinalizeUsage")
	defer release()
	if !ok {
		return nil
	}
	if p.stage == stageFinalized {
		panic("gateway: FinalizeUsage called twice: the request would be billed against two answers")
	}
	p.require("FinalizeUsage", stageRouted)
	p.stage = stageFinalized
	return p.inner.FinalizeUsage(d)
}

// LogPolicy and SanitizeForLog belong last, after the request is settled. They
// are unordered for the same reason NormalizeUpstreamError is: recording happens
// whatever else went wrong, and a payload that refused to describe its own
// bodies would leave the audit row unable to say anything about them.
//
// Both are read by the kernel now: the policy once at admission (it describes
// the whole request), the sanitizer at enforcement time, after settlement and
// before anything records. Both calls go through this wrapper, which is why
// they are unordered rather than stage-checked — enforcement happens after
// FinalizeUsage, and a stage gate here would refuse the call it exists for.
func (p *orderedPayload) LogPolicy() LogPolicy {
	release, ok := p.enter("LogPolicy")
	defer release()
	if !ok {
		return LogPolicy{}
	}
	return p.inner.LogPolicy()
}

func (p *orderedPayload) SanitizeForLog(k BodyKind, contentType string, body []byte) string {
	release, ok := p.enter("SanitizeForLog")
	defer release()
	if !ok {
		return ""
	}
	return p.inner.SanitizeForLog(k, contentType, body)
}

// orderedPayload is a Payload, so the kernel cannot accidentally hold the
// unwrapped one.
var _ Payload = (*orderedPayload)(nil)

// reconcileWithResponse checks a modality's account of a delivery against what
// the response itself records, and refuses the account when they disagree.
//
// A Delivery is a report. The report can be honest and still be wrong, and a
// wrong one here is not a logging problem: a modality that committed a response
// and then reported that nothing reached the caller sends the chain on to
// another candidate, on top of a response the caller already has. That is a
// second charge, and for a payload with side effects, a second execution.
// Validate cannot catch it — such a Delivery is perfectly self-consistent. Only
// the thing that actually wrote the bytes can.
//
// The response wins. It is the only participant that observed the wire.
func reconcileWithResponse(client ClientResponse, d fact.Delivery, requestID string) fact.Delivery {
	if client == nil {
		return d
	}
	committed := client.Committed()
	served := client.CommittedStatus()
	switch {
	case committed && !d.Committed:
		logger.Error("gateway: delivery denies a response the caller already received",
			zap.String("request_id", requestID),
			zap.Int("served_status", served),
			zap.String("fail_reason", d.FailReason))
		return contradiction(fact.Truncated(served, http.StatusInternalServerError,
			fact.FaultGateway, contradictionReason, errDeliveryContradictsResponse), d)
	case !committed && d.Committed:
		logger.Error("gateway: delivery claims a response the caller never received",
			zap.String("request_id", requestID),
			zap.Int("claimed_status", d.ClientStatus),
			zap.String("fail_reason", d.FailReason))
		return contradiction(fact.Undelivered(http.StatusInternalServerError, fact.VerdictSettled,
			fact.FaultGateway, contradictionReason, errDeliveryContradictsResponse), d)
	case committed && d.ClientStatus != served:
		// Both agree something was served; they disagree about what. The number
		// the caller actually got is the one the audit row has to carry, or a
		// support question is answered against a status nobody ever saw.
		logger.Error("gateway: delivery reports a status the caller was not served",
			zap.String("request_id", requestID),
			zap.Int("served_status", served),
			zap.Int("claimed_status", d.ClientStatus))
		return contradiction(fact.Truncated(served, http.StatusInternalServerError,
			fact.FaultGateway, contradictionReason, errDeliveryContradictsResponse), d)
	}
	return d
}

// contradictionReason is the fail reason every substitution above settles on.
const contradictionReason = "delivery_contradicts_response"

// contradiction carries forward what the refused delivery can still be trusted
// about.
//
// Its account of the response was wrong; its account of what the upstream
// charged was not, and the tokens were spent either way. Dropping the usage
// would bill the request at zero because our own adapter misreported — the
// caller would be getting a free request out of our bug.
func contradiction(substitute, original fact.Delivery) fact.Delivery {
	return substitute.WithUsage(original.Usage)
}

// errDeliveryContradictsResponse marks the substitution above.
var errDeliveryContradictsResponse = errors.New("gateway: delivery contradicts what was written to the caller")

// validateUpstreamPath refuses a path that could move the request off the
// candidate's own origin.
//
// Renaming the field from URL to Path removed the obvious way to redirect a
// request, not the ability. The kernel joins this onto a base by
// concatenation, so a value that starts with "@" turns the real host into
// userinfo — "https://api.provider.com" + "@evil.example/v1" addresses
// evil.example — and one starting with "//" is protocol-relative and names its
// own host outright. Either would send the provider's credentials, which the
// kernel attaches after this point, to somewhere the operator never configured.
// A name is not a constraint; this is.
func validateUpstreamPath(p string) error {
	if p == "" {
		return errors.New("gateway: upstream path is empty")
	}
	if p[0] != '/' {
		return fmt.Errorf("gateway: upstream path must be absolute, got %q", firstRunes(p, 16))
	}
	if strings.HasPrefix(p, "//") {
		return errors.New("gateway: upstream path is protocol-relative, which names its own host")
	}
	u, err := url.Parse(p)
	if err != nil {
		return fmt.Errorf("gateway: upstream path does not parse: %w", err)
	}
	if u.Scheme != "" || u.Host != "" || u.User != nil {
		return errors.New("gateway: upstream path carries an origin of its own")
	}
	// A base URL may itself carry a path prefix, and "/../" walks out of it —
	// still the same host, but no longer the endpoint the operator configured.
	if path.Clean(u.EscapedPath()) != strings.TrimSuffix(u.EscapedPath(), "/") &&
		path.Clean(u.EscapedPath()) != u.EscapedPath() {
		return errors.New("gateway: upstream path is not already clean, so it can walk out of the base path")
	}
	return nil
}

// firstRunes trims a value for an error message. The path came from a
// modality, and an error that echoed the whole of it would put attacker-shaped
// text into the log verbatim.
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
