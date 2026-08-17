package gateway

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/decision"
	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// Extension points are declared here; implementations live in their own
// packages and never import this one.
//
// Each shape is generic over V, the view the implementation wants of the
// exchange. That is not decoration: Go interface satisfaction requires
// signatures to match type for type, so an interface written against one fixed
// view type could only ever be satisfied by implementations that name that same
// type — which would force every implementation to import this package and undo
// the separation. With V as a parameter, an implementation declares the narrow
// view it actually needs, and the binding function supplied at assembly is the
// compile-time proof that an Exchange satisfies it.
//
// An implementation that needs nothing from the exchange uses struct{} as its
// V; the generic degenerates and no second calling convention is needed.

// UpstreamErrorObserverOf sees one complete NON-2xx upstream response and
// reports what it recognises in it. It cannot alter the response, write to the
// caller, or say what should happen next — its only output is what it reports.
//
// The name is narrow on purpose. A successful response never reaches this
// shape: the relay dispatches 2xx straight into the response pipeline, so an
// observer registered here would never see one. Observations drawn from a
// successful exchange — usage, stop reasons, first-token latency — arrive
// through the streaming and codec shapes instead, and calling this one
// "upstream observer" would promise a reach it does not have.
type UpstreamErrorObserverOf[V any] interface {
	Name() string
	ObserveUpstreamError(ctx context.Context, view V, up fact.Upstream, sink fact.Sink)
}

// upstreamErrorObserver is the kernel-side, view-erased form. One per shape.
type upstreamErrorObserver interface {
	name() string
	observe(ctx context.Context, e *Exchange, up fact.Upstream, sink fact.Sink)
}

type upstreamErrorObserverAdapter[V any] struct {
	inner UpstreamErrorObserverOf[V]
	bind  func(*Exchange) V
	// registeredName is captured once at assembly. Name() is capability code
	// like any other; reading it on a settlement or recovery path would put an
	// unguarded call into third-party code exactly where a guard matters most.
	registeredName string
}

func (a upstreamErrorObserverAdapter[V]) name() string { return a.registeredName }

func (a upstreamErrorObserverAdapter[V]) observe(ctx context.Context, e *Exchange, up fact.Upstream, sink fact.Sink) {
	a.inner.ObserveUpstreamError(ctx, a.bind(e), up, sink)
}

// RegisterUpstreamErrorObserver wires an observer into the service. The bind
// function is where an Exchange is checked against the observer's own view: a
// getter the observer needs and the Exchange lacks fails to compile at this
// call, not at run time.
func RegisterUpstreamErrorObserver[V any](s *Service, o UpstreamErrorObserverOf[V], bind func(*Exchange) V) {
	s.upstreamErrorObservers = append(s.upstreamErrorObservers, upstreamErrorObserverAdapter[V]{inner: o, bind: bind, registeredName: o.Name()})
}

// IngressRewriterOf rewrites the caller's own body, ONCE per exchange, before
// any candidate is chosen and before the modality is asked to admit it. That
// placement is the whole point of a separate shape: everything downstream —
// what the modality parses, what the audit row calls the request, what a token
// estimate counts — builds from one body, and a rewrite that landed later
// would leave those readings describing bytes that were never sent.
//
// It returns the body to carry forward. Returning the input unchanged — or
// nil, which the kernel reads the same way — is how a rewriter declines.
//
// An error means "this body is unusable" and ends the exchange before any
// upstream is contacted. That makes it the wrong tool for the common case: a
// rewriter that merely could not do its job — a body it cannot parse is a body
// the upstream may well accept — must return the ORIGINAL body and report what
// happened. Reserve the error for a body that must not be sent at all, and
// even then the rewriter does not choose the consequence: it says the body is
// unusable and the kernel's table decides what that costs.
//
// There is no protocol parameter, unlike the egress shape. The ingress
// protocol is fixed for the exchange, so a rewriter that needs it reads it off
// its own view rather than being handed a value that cannot vary.
//
// THE BODY MUST NOT BE MODIFIED IN PLACE. Produce a new slice, or return the
// input untouched; the bytes handed over are the same ones the audit row keeps
// as the caller's verbatim request, so a rewriter that edits them rewrites the
// record of what arrived. It is a contract rather than a defensive copy for a
// reason: a body may run to twenty megabytes, and copying one per rewriter on
// every request would cost more than it protects — worse, it would spend that
// copy BEFORE the rewriter's own size and validity gates, which exist to keep
// exactly those bodies cheap.
//
// Applies is asked first, and separately, so a capability the deployment has
// switched off costs nothing at all: no call, no allocation, not even a sink.
// It must read only the view, and it carries the whole applicability decision,
// so RewriteIngress is reached only when there is real work to do.
type IngressRewriterOf[V any] interface {
	Name() string
	Applies(view V) bool
	RewriteIngress(ctx context.Context, view V, body []byte, sink fact.Sink) ([]byte, error)
}

// IngressStage fixes the order of ingress rewriters, on the same reasoning as
// EgressStage: order is a property of the pipeline being assembled, not of the
// rewriter, so it is supplied at registration and never named by the capability
// itself — naming it would mean importing the kernel.
type IngressStage uint8

const (
	// StageCompress shrinks the body, so it must run before anything that
	// counts or prices tokens: an estimate taken ahead of it would charge for
	// bytes that never left.
	StageCompress IngressStage = 10
	// StageVisionFallback turns images into text (or placeholders) for
	// models declared unable to read them. It runs after compression: the
	// text it injects is derived content that compression has no business
	// shrinking, and the images compression skipped are exactly what this
	// stage consumes.
	StageVisionFallback IngressStage = 20
)

// ingressRewriter is the kernel-side, view-erased form.
type ingressRewriter interface {
	name() string
	stage() IngressStage
	applies(e *Exchange) bool
	rewrite(ctx context.Context, e *Exchange, body []byte, sink fact.Sink) ([]byte, error)
}

type ingressRewriterAdapter[V any] struct {
	inner   IngressRewriterOf[V]
	bind    func(*Exchange) V
	atStage IngressStage
	// Captured at registration; see the field note on
	// upstreamErrorObserverAdapter.
	registeredName string
}

func (a ingressRewriterAdapter[V]) name() string        { return a.registeredName }
func (a ingressRewriterAdapter[V]) stage() IngressStage { return a.atStage }

func (a ingressRewriterAdapter[V]) applies(e *Exchange) bool { return a.inner.Applies(a.bind(e)) }

func (a ingressRewriterAdapter[V]) rewrite(ctx context.Context, e *Exchange, body []byte, sink fact.Sink) ([]byte, error) {
	return a.inner.RewriteIngress(ctx, a.bind(e), body, sink)
}

// RegisterIngressRewriter wires a rewriter into the service, keeping the slice
// ordered by stage so the run order is settled at assembly rather than
// recomputed per request. Two rewriters claiming the same stage is a
// programming error and panics here, at startup, rather than resolving to
// whichever was registered first.
func RegisterIngressRewriter[V any](s *Service, r IngressRewriterOf[V], at IngressStage, bind func(*Exchange) V) {
	adapter := ingressRewriterAdapter[V]{inner: r, bind: bind, atStage: at, registeredName: r.Name()}
	for _, existing := range s.ingressRewriters {
		if existing.stage() == adapter.stage() {
			panic(fmt.Sprintf("gateway: ingress stage %d claimed by both %q and %q",
				at, existing.name(), adapter.name()))
		}
	}
	s.ingressRewriters = append(s.ingressRewriters, adapter)
	sort.Slice(s.ingressRewriters, func(i, j int) bool {
		return s.ingressRewriters[i].stage() < s.ingressRewriters[j].stage()
	})
}

// rewriteIngress runs the registered rewriters in stage order over the caller's
// body and reports the verdict the kernel should act on.
//
// The rewriters chain: each sees the previous one's output, so there is one
// current body at every point rather than competing versions of it. The first
// one is shown the very bytes the audit row keeps, which is why the shape
// forbids editing in place; the alternative, a copy per rewriter, would spend a
// full body — up to twenty megabytes here — ahead of the size and validity
// gates a rewriter uses to decline cheaply, on every request of a deployment
// that happens to have the capability switched on.
//
// A rewriter that errors has declared the body unusable, and this stops there
// with the body unchanged. Carrying on with it would send upstream exactly what
// a rewriter just refused to produce. Its own words stay in the log: this
// verdict ends the request, so the detail is what the caller is shown, and an
// error about a body a rewriter could not parse is not written for them.
func (s *Service) rewriteIngress(ctx context.Context, rc *Exchange, body []byte) (out []byte, changed bool, verdict decision.Resolved) {
	if len(s.ingressRewriters) == 0 {
		return body, false, decision.Resolved{}
	}
	if ctx == nil {
		// Reached only from a caller that never established a request context.
		// A rewriter that consults ctx should see an inert one rather than a
		// nil that panics on first use.
		ctx = context.Background()
	}
	original := body
	// Built on first use, so a request nobody applies to allocates nothing.
	var sink *exchangeSink
	for _, r := range s.ingressRewriters {
		if !r.applies(rc) {
			continue
		}
		if sink == nil {
			sink = newExchangeSink(rc)
		}
		sink.reporter = r.name()
		rewritten, err := r.rewrite(ctx, rc, body, sink)
		if err != nil {
			logger.Warn("gateway: ingress rewrite refused the body",
				zap.String("request_id", rc.requestID),
				zap.String("rewriter", r.name()),
				zap.Error(err))
			sink.Report(fact.Fact{
				Kind:   fact.KindIngressRewriteFailed,
				Detail: "the request could not be prepared for dispatch",
			})
			return original, false, sink.resolve()
		}
		if rewritten != nil {
			body = rewritten
		}
	}
	if sink == nil {
		return original, false, decision.Resolved{}
	}
	return body, !bytes.Equal(body, original), sink.resolve()
}

// EgressRewriterOf rewrites the body about to be sent upstream, once per
// CANDIDATE, after the modality has encoded it and before credentials are
// attached. Key rotation within a candidate reuses the rewritten body — the
// body depends on where it is going, not on which credential sends it — so a
// rewriter must not assume it runs again per key.
//
// It returns the body to send. Returning the input unchanged — or nil, which
// the kernel reads the same way — is how a rewriter declines; there is no
// separate "skip" signal, because a rewriter that has nothing to do and a
// rewriter that decided against acting are the same thing from the kernel's
// side.
//
// An error means "this body is unusable", and it stops the attempt: nothing is
// sent upstream. That makes it the wrong tool for the common case. A rewriter
// that merely could not do its job — a body it cannot parse is a body some
// upstream may still accept — must return the ORIGINAL body and report what
// happened. Reserve the error for a body that must not be sent.
//
// Even then the rewriter does not choose the consequence: it says the body is
// unusable, and the kernel's table decides what that costs the request.
//
// The egress protocol is a parameter rather than something read off the view
// because it belongs to the attempt, not the exchange: the same request can be
// encoded for a different protocol on a later candidate.
type EgressRewriterOf[V any] interface {
	Name() string
	RewriteEgress(ctx context.Context, view V, egress protocols.ProtocolID, body []byte, sink fact.Sink) ([]byte, error)
}

// EgressStage fixes the order of egress rewriters.
//
// Order is supplied at registration rather than declared by the rewriter, and
// that placement is the point: where a rewriter sits relative to the others is
// a property of the pipeline being assembled, not of the rewriter itself. A
// rewriter that renamed a field would have no way to know which other rewriter
// reads the renamed name — whoever composes them does.
//
// It also keeps the constraint from leaking: were the stage part of the
// interface, every rewriter would have to name this type, and naming it means
// importing the kernel — exactly the dependency the split exists to prevent.
//
// The values are spaced so a rewriter can be inserted between two existing ones
// without renumbering, and a collision is a startup failure rather than a tie
// broken silently by registration order.
type EgressStage uint8

const (
	// StageCustomPrompt appends to the system text, so it runs late: it must
	// see the body every other rewriter has finished shaping.
	StageCustomPrompt EgressStage = 50
	// StageMaxTokens holds the output ceiling down to the candidate's, and runs
	// after anything that could rename or introduce that field.
	StageMaxTokens EgressStage = 60
)

// egressRewriter is the kernel-side, view-erased form.
type egressRewriter interface {
	name() string
	stage() EgressStage
	rewrite(ctx context.Context, e *Exchange, egress protocols.ProtocolID, body []byte, sink fact.Sink) ([]byte, error)
}

type egressRewriterAdapter[V any] struct {
	inner   EgressRewriterOf[V]
	bind    func(*Exchange) V
	atStage EgressStage
	// Captured at registration; see the field note on
	// upstreamErrorObserverAdapter.
	registeredName string
}

func (a egressRewriterAdapter[V]) name() string       { return a.registeredName }
func (a egressRewriterAdapter[V]) stage() EgressStage { return a.atStage }

func (a egressRewriterAdapter[V]) rewrite(ctx context.Context, e *Exchange, egress protocols.ProtocolID, body []byte, sink fact.Sink) ([]byte, error) {
	return a.inner.RewriteEgress(ctx, a.bind(e), egress, body, sink)
}

// RegisterEgressRewriter wires a rewriter into the service, keeping the slice
// ordered by stage so the run order is settled at assembly rather than
// recomputed per request. Two rewriters claiming the same stage is a
// programming error and panics here, at startup, rather than resolving to
// whichever was registered first.
func RegisterEgressRewriter[V any](s *Service, r EgressRewriterOf[V], at EgressStage, bind func(*Exchange) V) {
	adapter := egressRewriterAdapter[V]{inner: r, bind: bind, atStage: at, registeredName: r.Name()}
	for _, existing := range s.egressRewriters {
		if existing.stage() == adapter.stage() {
			panic(fmt.Sprintf("gateway: egress rewriters %q and %q both claim stage %d",
				existing.name(), adapter.name(), adapter.stage()))
		}
	}
	s.egressRewriters = append(s.egressRewriters, adapter)
	sort.Slice(s.egressRewriters, func(i, j int) bool {
		return s.egressRewriters[i].stage() < s.egressRewriters[j].stage()
	})
}

// FailureRewriterOf is called after a non-2xx upstream response, with that
// response in hand, so a capability may offer a REPAIRED body. It never says
// "retry": it returns a body and reports a fact. Whether the repaired body is
// worth an attempt, and against which candidate, is the decision table's
// call — a body returned without a fact behind it is never dispatched.
//
// body is the egress-encoded body the failed dispatch actually sent; a
// repaired body must be in the same encoding, because it is re-sent to the
// same candidate as-is. The egress protocol is a parameter for the same
// reason it is on the egress rewriter: it belongs to the attempt, not the
// exchange, and a repairer that cannot tell which schema the bytes are in
// cannot repair them. Returning nil, or the input unchanged, abstains.
//
// An error means "no repair could be produced" and nothing more. The exchange
// is already on a failure path, so the kernel logs the error and carries on
// with the failure handling the response would have received anyway — unlike
// the ingress and egress shapes, there is no usable-body contract here to
// break.
type FailureRewriterOf[V any] interface {
	Name() string
	RewriteAfterFailure(ctx context.Context, view V, egress protocols.ProtocolID, body []byte, up fact.Upstream, sink fact.Sink) ([]byte, error)
}

// failureRewriter is the kernel-side, view-erased form.
type failureRewriter interface {
	name() string
	rewrite(ctx context.Context, e *Exchange, egress protocols.ProtocolID, body []byte, up fact.Upstream, sink fact.Sink) ([]byte, error)
}

type failureRewriterAdapter[V any] struct {
	inner FailureRewriterOf[V]
	bind  func(*Exchange) V
	// Captured at registration; see the field note on
	// upstreamErrorObserverAdapter.
	registeredName string
}

func (a failureRewriterAdapter[V]) name() string { return a.registeredName }

func (a failureRewriterAdapter[V]) rewrite(ctx context.Context, e *Exchange, egress protocols.ProtocolID, body []byte, up fact.Upstream, sink fact.Sink) ([]byte, error) {
	return a.inner.RewriteAfterFailure(ctx, a.bind(e), egress, body, up, sink)
}

// RegisterFailureRewriter wires a failure rewriter into the service. They run
// in registration order, each over the previous one's output; no stage
// parameter until a second production rewriter demonstrates an ordering
// constraint worth encoding.
func RegisterFailureRewriter[V any](s *Service, r FailureRewriterOf[V], bind func(*Exchange) V) {
	s.failureRewriters = append(s.failureRewriters, failureRewriterAdapter[V]{inner: r, bind: bind, registeredName: r.Name()})
}

// rewriteAfterFailure runs every registered failure rewriter over one failed
// dispatch and folds what they reported. It returns the final repaired body —
// nil when nobody repaired — and the resolved verdict of everything reported
// through it, which the caller folds with the observers' before routing.
//
// The rewriters chain: each sees the previous one's output, so there is one
// current body at every point rather than competing repairs. Every input is
// isolated per rewriter — the upstream snapshot for the same reason it is per
// observer, and the body because it aliases the audit capture of what was
// actually sent: a rewriter that edits its input in place and then abstains
// would otherwise corrupt the audit record and hand its dead edit to the next
// rewriter in the chain.
//
// Abstention is real abstention, and it is atomic PER INVOCATION: an
// invocation is accepted only when it changed the body AND reported the
// repair verdict for it, and anything less — an error, a nil or unchanged
// return, a changed body with no verdict, a verdict with no change —
// contributes NOTHING, neither body nor facts. The two halves of an answer
// vouch for each other: facts without a changed body would let a rewriter
// steer the chain from a shape whose whole licence is "offer a repair", and
// a changed body without ITS OWN repair verdict would dispatch bytes nobody
// answered for — including a later rewriter's factless edit riding on an
// earlier rewriter's verdict. Dropped reports stay on the timeline, which
// records what was said, not what was acted on.
//
// The final chained body is also checked against the ORIGINAL dispatch: a
// chain whose net effect restores the bytes that just failed has repaired
// nothing, however each step reported itself.
func (s *Service) rewriteAfterFailure(ctx context.Context, rc *Exchange, egress protocols.ProtocolID, body []byte, up fact.Upstream) ([]byte, decision.Resolved) {
	if len(s.failureRewriters) == 0 {
		return nil, decision.Resolved{}
	}
	base := newExchangeSink(rc)
	var resolved decision.Resolved
	current := body
	accepted := false
	for _, r := range s.failureRewriters {
		sink := base.forObserver(r.name())
		out, err := r.rewrite(ctx, rc, egress, bytes.Clone(current), isolate(up), sink)
		if err != nil {
			logger.Warn("gateway: failure rewriter could not produce a repair",
				zap.String("request_id", rc.requestID),
				zap.String("rewriter", r.name()),
				zap.Error(err))
			continue
		}
		if out == nil || bytes.Equal(out, current) {
			continue
		}
		verdict := sink.resolve()
		if verdict.Loop != decision.LoopRetrySameCandidate {
			continue
		}
		resolved = decision.Combine(resolved, verdict)
		current = out
		accepted = true
	}
	if !accepted || bytes.Equal(current, body) {
		return nil, resolved
	}
	return current, resolved
}

// rewriteEgress runs the registered rewriters in stage order over one attempt's
// body, and reports the verdict the kernel should act on.
//
// A rewriter that errors has declared the body unusable, and this stops there.
// Carrying on with the last good body would send upstream exactly what a
// rewriter just refused to produce — the rewriter would have been better off
// never running. What the refusal costs the request is still not the
// rewriter's call: it reports a fact and the table decides, which is why the
// failure comes back as a verdict rather than as an error the caller must
// interpret.
func (s *Service) rewriteEgress(ctx context.Context, rc *Exchange, egress protocols.ProtocolID, body []byte) ([]byte, decision.Resolved) {
	if len(s.egressRewriters) == 0 {
		return body, decision.Resolved{}
	}
	if ctx == nil {
		// Reached only from a caller that never established a request context.
		// A rewriter that consults ctx should see an inert one rather than a
		// nil that panics on first use.
		ctx = context.Background()
	}
	sink := newExchangeSink(rc)
	for _, r := range s.egressRewriters {
		sink.reporter = r.name()
		out, err := r.rewrite(ctx, rc, egress, body, sink)
		if err != nil {
			logger.Warn("gateway: egress rewrite refused the body",
				zap.String("request_id", rc.requestID),
				zap.String("rewriter", r.name()),
				zap.Error(err))
			sink.Report(fact.Fact{
				Kind:   fact.KindEgressRewriteFailed,
				Detail: r.name() + ": " + err.Error(),
			})
			return body, sink.resolve()
		}
		if out != nil {
			body = out
		}
	}
	return body, sink.resolve()
}

// kernelReporter is the provenance name the kernel stamps on facts it reports
// itself — its own reading of an upstream status line, filed through the same
// vocabulary as every capability's report so the timeline shows who judged
// what regardless of which side of the seam the judgement came from.
const kernelReporter = "kernel"

// exchangeSink collects what capabilities report during one exchange.
//
// It stamps provenance as reports arrive rather than asking reporters to supply
// it, so the attempt a report belongs to cannot be misattributed by a capability
// that held on to a stale value.
//
// Build one with newExchangeSink and never with a struct literal: every
// provenance field has a meaningful zero, so a literal that omits one produces
// entries that are wrong rather than obviously incomplete — an audit row
// claiming candidate 0 refused the payload reads exactly like a real one.
type exchangeSink struct {
	timeline  *fact.Timeline
	reporter  string
	attempt   int
	candidate uint
	provider  uint
	now       func() time.Time

	// batches records each Report call separately. Facts reported together are
	// resolved together, so the grouping has to survive until resolution.
	batches [][]fact.Fact
}

// forObserver returns a sink with the same provenance but its own reports.
//
// The provenance — which attempt, which candidate, which provider — is a
// property of where the exchange stands, so it is shared. The batches are not:
// resolving a sink folds everything reported through it, and one observer must
// not be answerable for what another reported, nor for what the kernel reported
// before either of them ran.
func (s *exchangeSink) forObserver(name string) *exchangeSink {
	clone := *s
	clone.reporter = name
	clone.batches = nil
	return &clone
}

// newExchangeSink builds a sink whose provenance describes where the exchange
// currently stands.
//
// The attempt number is taken as the count of records appended so far, which is
// the index the attempt about to be recorded will occupy: the sink is always
// built before that append, so reports land on the attempt that produced them
// rather than the one after it.
//
// candidate and provider are read through nil checks because the relay clears
// them: a candidate whose provider turned out to be unusable leaves provider
// nil on purpose, and attributing that report to whichever provider happened to
// be set last would be worse than attributing it to none.
func newExchangeSink(rc *Exchange) *exchangeSink {
	s := &exchangeSink{
		timeline: &rc.timeline,
		attempt:  len(rc.attempts),
		// Timeline entries are stamped in UTC like every other time this
		// service persists or compares, so an audit trail assembled from
		// several hosts stays in one frame of reference.
		now: func() time.Time { return time.Now().UTC() },
	}
	if rc.attempt.Candidate() != nil {
		s.candidate = rc.attempt.Candidate().ID
	}
	if rc.attempt.Provider() != nil {
		s.provider = rc.attempt.Provider().ID
	}
	return s
}

// newKernelSink builds a sink already stamped with the kernel's own
// provenance name. Kernel-side sites that file under the kernel's name
// build here rather than stamping after construction, so the stamp is not
// a separate step a site can forget — an omission that produces entries
// which are wrong rather than obviously incomplete.
func newKernelSink(rc *Exchange) *exchangeSink {
	s := newExchangeSink(rc)
	s.reporter = kernelReporter
	return s
}

// reportKernelFact files one judgement of the kernel's own: it builds the
// sink, stamps kernel provenance, reports the fact, and folds it into a
// verdict, all in a single call so no step can be reordered or skipped.
// Callers that only wanted the entry on the timeline discard the verdict.
func reportKernelFact(rc *Exchange, f fact.Fact) decision.Resolved {
	sink := newKernelSink(rc)
	// Overwritten unconditionally: Report only fills an EMPTY Reporter, so a
	// fact that arrived pre-attributed would file the kernel's own judgement
	// under someone else's name.
	f.Reporter = kernelReporter
	sink.Report(f)
	return sink.resolve()
}

// newSettlementSink builds the sink a settlement files through, with kernel
// provenance: what a settlement notes (the delivery observation) is the
// kernel's own record of how the request ended.
// againstRecordedAttempt renumbers it to describe the attempt ALREADY on
// the list rather than the one about to be added. The numbering rationale
// lives on settleOptions.againstRecordedAttempt, the option every caller
// arrives here with.
func newSettlementSink(rc *Exchange, againstRecordedAttempt bool) *exchangeSink {
	s := newKernelSink(rc)
	if againstRecordedAttempt && s.attempt > 0 {
		s.attempt--
	}
	return s
}

func (s *exchangeSink) Report(facts ...fact.Fact) {
	if len(facts) == 0 {
		return
	}
	batch := make([]fact.Fact, 0, len(facts))
	for _, f := range facts {
		if f.Reporter == "" {
			f.Reporter = s.reporter
		}
		batch = append(batch, f)
		s.timeline.Append(fact.Entry{
			Attempt:   s.attempt,
			Candidate: s.candidate,
			Provider:  s.provider,
			At:        s.now(),
			Reporter:  f.Reporter,
			Fact:      &f,
		})
	}
	s.batches = append(s.batches, batch)
}

func (s *exchangeSink) Note(records ...fact.Record) {
	for _, r := range records {
		s.timeline.Append(fact.Entry{
			Attempt:   s.attempt,
			Candidate: s.candidate,
			Provider:  s.provider,
			At:        s.now(),
			Reporter:  s.reporter,
			Record:    r,
		})
	}
}

// resolve folds every batch reported through this sink into one decision.
// Batches fold into each other by the same rule as facts within a batch, so a
// capability reporting twice is indistinguishable from two capabilities
// reporting once — which is the point: the outcome depends on what was said,
// not on who said it or when.
func (s *exchangeSink) resolve() decision.Resolved {
	var out decision.Resolved
	for _, b := range s.batches {
		out = decision.Combine(out, decision.ResolveBatch(b))
	}
	return out
}

// observeUpstreamError runs every registered observer over one upstream response and
// folds what they reported into a single verdict.
//
// Observers are run unconditionally rather than short-circuiting on the first
// report: each one sees the whole response and none of them knows what the
// others recognised, so stopping early would make the verdict depend on
// registration order — the one thing the fold is built to rule out.
//
// Each observer gets its own copy of the response for the same reason. Header
// and Body are a map and a slice, so handing every observer the same value
// would let one that normalises either in place change what the next one sees,
// reintroducing order dependence through a second door. The Body is also the
// bytes already captured for the audit row, so a stray write would rewrite the
// record of what the upstream actually said.
func (s *Service) observeUpstreamError(ctx context.Context, rc *Exchange, up fact.Upstream) decision.Resolved {
	if len(s.upstreamErrorObservers) == 0 {
		return decision.Resolved{}
	}
	sink := newExchangeSink(rc)
	for _, o := range s.upstreamErrorObservers {
		sink.reporter = o.name()
		o.observe(ctx, rc, isolate(up), sink)
	}
	return sink.resolve()
}

// isolate returns a copy an observer cannot use to reach anything outside
// itself.
//
// The cost is one header clone and one body copy per observer. Error bodies are
// already read under a 1 MiB bound and observers number in the single digits,
// so this buys a structural guarantee for a bounded price — and the alternative,
// trusting every present and future observer to treat its input as read-only,
// is the kind of guarantee that holds until exactly one of them does not.
func isolate(up fact.Upstream) fact.Upstream {
	out := up
	if up.Header != nil {
		out.Header = up.Header.Clone()
	}
	if up.Body != nil {
		out.Body = bytes.Clone(up.Body)
	}
	return out
}

// DeliveryObserverOf sees how one exchange ended — including, and especially,
// when it ended well.
//
// This is the success-side counterpart to UpstreamErrorObserverOf, and it is
// one shape rather than two because of where it runs. A successful streaming
// response has no complete body anybody could be handed: the bytes are
// forwarded as they arrive, and what a capability wants from them — the tokens
// billed, whether the answer finished, what an upstream charged extra for —
// arrives at the end. A successful non-streaming response has a body, but the
// same facts are the ones worth reporting. Both now arrive at the single point
// where a delivery settles, carrying what the attempt reported, so one
// observation point covers what would otherwise be a streaming shape and a
// buffered shape with two sets of call sites to keep in step.
//
// It observes and cannot steer. By the time it runs the caller has been served
// and the request is over, so NO effect a report can carry could be acted on —
// not a Loop, and not the five others either. Reporting any of them is a
// programming error and is refused loudly rather than ignored quietly.
type DeliveryObserverOf[V any] interface {
	Name() string
	ObserveDelivery(ctx context.Context, view V, d fact.Delivery, sink fact.Sink)
}

// deliveryObserver is the kernel-side, view-erased form.
//
// registeredName is a field rather than a method because it is read on the
// settlement path, where calling into the observer for it would be one more
// unguarded call into third-party code. Asked once at registration, it is also
// asked at a time when a panic is a startup failure rather than a request that
// loses its row.
type deliveryObserver struct {
	registeredName string
	observe        func(ctx context.Context, e *Exchange, d fact.Delivery, sink fact.Sink)
}

// RegisterDeliveryObserver wires a delivery observer into the service.
func RegisterDeliveryObserver[V any](s *Service, o DeliveryObserverOf[V], bind func(*Exchange) V) {
	s.deliveryObservers = append(s.deliveryObservers, deliveryObserver{
		registeredName: o.Name(),
		observe: func(ctx context.Context, e *Exchange, d fact.Delivery, sink fact.Sink) {
			o.ObserveDelivery(ctx, bind(e), d, sink)
		},
	})
}

// observeDelivery runs every registered observer over the delivery that ended
// the request.
//
// The sink is the settlement's own, so what an observer reports lands on the
// attempt the delivery describes rather than on whichever one the numbering
// would default to.
//
// Nothing an observer reports here can be acted on. The caller has been served,
// the status is on the wire, and the row is about to be written — a Loop effect
// would mean settling the same request twice, and a status effect would name a
// code nobody can still be shown. So the verdict is computed and refused rather
// than never computed: a report that cannot be honoured is a mistake somebody
// should hear about, and dropping it silently is how it stays a mistake. The
// test is whether a decision was DEFINED at all, not whether it steers, because
// every one of the six effects is equally unhonourable at this point.
//
// A panic in an observer must not take the settlement with it. By the time this
// runs the caller already has their answer; letting a third-party observation
// unwind into the request's own panic recovery would turn a request that was
// served correctly into a recorded 500 with no usage and no cost — losing the
// row for the one exchange that went fine. Same reasoning as the recorders,
// same guard.
func (s *Service) observeDelivery(rc *Exchange, d fact.Delivery, sink *exchangeSink) {
	if len(s.deliveryObservers) == 0 {
		return
	}
	ctx, done := observationContext(rc)
	defer done()
	for _, o := range s.deliveryObservers {
		// Each observer reports through its own sink. The other extension-point
		// loops share one and restamp its reporter, which is fine for them —
		// they resolve nothing, or resolve a sink the kernel has not reported
		// through. This one does both: it is handed the settlement's sink, and
		// it reads a verdict back out. Sharing it would fold whatever the kernel
		// had already reported into the check below, blaming an observer for an
		// effect no observer asked for, and would leave the last observer's name
		// stamped on it for whatever the kernel reported next.
		//
		// The name comes from the registration record, not from the observer.
		// Name() is capability code like any other: asking for it outside the
		// guard puts an unprotected call ahead of the protected one, and asking
		// again inside the recovery would re-enter the code that just failed.
		own := sink.forObserver(o.registeredName)
		func() {
			defer func() {
				if v := recover(); v != nil {
					logger.Error("gateway: delivery observer panicked",
						zap.String("observer", o.registeredName),
						zap.String("request_id", rc.requestID),
						zap.Any("panic", v))
				}
			}()
			o.observe(ctx, rc, isolateDelivery(d), own)
		}()
		if v := own.resolve(); v.Defined {
			// Attributed by observer name rather than by Kind. loopFrom is only
			// filled when a Loop effect wins the fold, so a report that steers
			// nothing — which is most of what can be reported here — would name
			// the zero Kind, which is documented as never reported at all.
			fields := []zap.Field{
				zap.String("request_id", rc.requestID),
				zap.String("observer", o.registeredName),
			}
			if v.Loop != decision.LoopNone {
				fields = append(fields, zap.String("reported_kind", v.LoopFrom().String()))
			}
			logger.Error("gateway: a delivery observer reported an effect on a settled request", fields...)
		}
	}
}

// observationContext is the context an observation of a finished exchange runs
// under. It is deliberately NOT the request's own.
//
// An observation runs after the caller has been served, and what it is for is
// accounting: recording what an upstream charged, on an exchange that is over.
// The request context is cancelled the moment the caller hangs up — and a
// caller hanging up mid-stream is exactly the case where an upstream has
// already done, and charged for, its work. Inheriting that cancellation would
// make every ctx-aware write inside an observer fail with context canceled
// precisely when there is most to record.
//
// The deadline replaces the cancellation it drops rather than adding a new
// constraint. Observers run inline, ahead of the audit row and the release of
// whatever admissions are still held, so one that never returns holds those
// open indefinitely. This bounds a ctx-aware observer; one that ignores its
// context entirely still blocks, and no deadline here can change that — the
// only thing that would is running observers off this goroutine, which would
// trade a bounded stall for reports arriving after the row they belong to.
func observationContext(rc *Exchange) (context.Context, context.CancelFunc) {
	parent := rc.requestCtx
	if parent == nil {
		// Reached only from a caller that never established a request context.
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), observationBudget)
}

// observationBudget is how long the observers of one exchange get, in total, to
// do their recording before settlement stops waiting on their context.
const observationBudget = 5 * time.Second

// isolateDelivery returns a copy whose usage an observer cannot write through.
//
// Delivery travels by value, but Usage is a pointer, and it is the pointer
// settlement is about to bill from. An observer that adjusted the counts it was
// shown — a surcharge added in the obvious place — would change what the caller
// is charged from a shape that is documented as unable to change anything, and
// it would change it for every observer after it too.
func isolateDelivery(d fact.Delivery) fact.Delivery {
	if d.Usage != nil {
		usage := *d.Usage
		d.Usage = &usage
	}
	return d
}

// isolateOutcome returns a copy whose usage a capability cannot write through.
//
// Outcome travels by value, but Usage is a pointer to the settled record — the
// same one the audit row persists. Releases and recorders run in a chain over
// the same outcome; one that adjusted the counts it was shown would change what
// every callback after it settles from, and would rewrite the books themselves,
// all through a shape documented as an immutable snapshot.
func isolateOutcome(out fact.Outcome) fact.Outcome {
	if out.Usage != nil {
		usage := *out.Usage
		out.Usage = &usage
	}
	return out
}

// ResponseCodecWrapperOf decorates the encoders that turn an upstream response
// back into the caller's own protocol. A capability is handed an encoder and
// returns an encoder; it never sees the connection, the writer, or a byte.
//
// That boundary is the whole shape. Deciding what a response says is a
// capability's business; deciding whether the caller received it is not, and
// the kernel keeps the parts that answer the second question — flushing, what
// has already been committed, and how a failed write is classified. A wrapper
// that could reach those could turn "the caller hung up" into "we served them".
//
// It applies only where a response is converted between protocols. A request
// forwarded to a provider speaking the caller's own protocol is relayed as
// bytes, with no encoder to wrap: there is nothing here for a capability to
// decorate, and the kernel rewrites those bytes itself.
//
// Applies is asked once per attempt rather than per frame. A stream can carry
// hundreds of events and a question answered identically for all of them
// belongs outside the loop.
type ResponseCodecWrapperOf[V any] interface {
	Name() string
	Applies(view V) bool
	WrapResponseEncoder(view V, enc protocols.ResponseEncoder, sink fact.Sink) protocols.ResponseEncoder
	WrapStreamEncoder(view V, enc protocols.StreamEncoder, sink fact.Sink) protocols.StreamEncoder
}

// CodecStage fixes the order wrappers are applied in, supplied at registration
// for the same reason the rewriter stages are: where something sits in a chain
// is a property of the chain, not of the thing sitting in it.
type CodecStage uint8

const (
	// StageModelName puts the caller's own model name back. It wraps closest
	// to the encoder so that anything added later sees the name the caller
	// will actually read.
	StageModelName CodecStage = 10
)

// responseCodecWrapper is the kernel-side, view-erased form.
type responseCodecWrapper interface {
	name() string
	stage() CodecStage
	applies(e *Exchange) bool
	wrapResponse(e *Exchange, enc protocols.ResponseEncoder, sink fact.Sink) protocols.ResponseEncoder
	wrapStream(e *Exchange, enc protocols.StreamEncoder, sink fact.Sink) protocols.StreamEncoder
}

type responseCodecWrapperAdapter[V any] struct {
	inner   ResponseCodecWrapperOf[V]
	bind    func(*Exchange) V
	atStage CodecStage
	// Captured at registration; see the field note on
	// upstreamErrorObserverAdapter.
	registeredName string
}

func (a responseCodecWrapperAdapter[V]) name() string      { return a.registeredName }
func (a responseCodecWrapperAdapter[V]) stage() CodecStage { return a.atStage }

func (a responseCodecWrapperAdapter[V]) applies(e *Exchange) bool { return a.inner.Applies(a.bind(e)) }

func (a responseCodecWrapperAdapter[V]) wrapResponse(e *Exchange, enc protocols.ResponseEncoder, sink fact.Sink) protocols.ResponseEncoder {
	return a.inner.WrapResponseEncoder(a.bind(e), enc, sink)
}

func (a responseCodecWrapperAdapter[V]) wrapStream(e *Exchange, enc protocols.StreamEncoder, sink fact.Sink) protocols.StreamEncoder {
	return a.inner.WrapStreamEncoder(a.bind(e), enc, sink)
}

// RegisterResponseCodecWrapper wires a wrapper into the service, keeping the
// slice ordered by stage. Two wrappers claiming one stage is a programming
// error and panics at startup rather than resolving to registration order.
func RegisterResponseCodecWrapper[V any](s *Service, w ResponseCodecWrapperOf[V], at CodecStage, bind func(*Exchange) V) {
	adapter := responseCodecWrapperAdapter[V]{inner: w, bind: bind, atStage: at, registeredName: w.Name()}
	for _, existing := range s.responseCodecWrappers {
		if existing.stage() == adapter.stage() {
			panic(fmt.Sprintf("gateway: codec stage %d claimed by both %q and %q",
				at, existing.name(), adapter.name()))
		}
	}
	s.responseCodecWrappers = append(s.responseCodecWrappers, adapter)
	sort.Slice(s.responseCodecWrappers, func(i, j int) bool {
		return s.responseCodecWrappers[i].stage() < s.responseCodecWrappers[j].stage()
	})
}

// ResponseCodecs is what a modality is handed to decorate the encoders it
// builds for a converted response.
//
// A value rather than an interface, so the zero value works: a toolbox built
// without one wraps nothing, which is what a service with no wrappers
// registered should do anyway. A nil interface here would instead be a panic on
// the delivery path.
type ResponseCodecs struct {
	wrappers []responseCodecWrapper
	exchange *Exchange
}

// WrapResponse returns the encoder to use for a non-streaming response.
func (c ResponseCodecs) WrapResponse(enc protocols.ResponseEncoder) protocols.ResponseEncoder {
	base := c.newSink()
	for _, w := range c.wrappers {
		if !w.applies(c.exchange) {
			continue
		}
		if wrapped := w.wrapResponse(c.exchange, enc, base.forObserver(w.name())); wrapped != nil {
			enc = wrapped
		}
	}
	return enc
}

// WrapStream returns the encoder to use for a streamed response.
func (c ResponseCodecs) WrapStream(enc protocols.StreamEncoder) protocols.StreamEncoder {
	base := c.newSink()
	for _, w := range c.wrappers {
		if !w.applies(c.exchange) {
			continue
		}
		if wrapped := w.wrapStream(c.exchange, enc, base.forObserver(w.name())); wrapped != nil {
			enc = wrapped
		}
	}
	return enc
}

// newSink builds the sink the wrappers report through, one stamped copy per
// wrapper the way every other shape's is.
//
// Provenance is the kernel's to write, not the capability's: a wrapper that
// named itself could name anything, and a timeline entry with no name at all —
// which is what handing over an unstamped sink produces — is a fact nobody can
// trace to the code that reported it.
//
// A copy per wrapper rather than one sink re-stamped as the loop advances. This
// shape hands back an ENCODER, and the natural thing for a wrapper to do with
// its sink is keep it and report while encoding — which happens long after this
// loop finished. A shared sink would by then be carrying the last wrapper's
// name, and every deferred report in the chain would be filed under whoever
// happened to be wrapped outermost.
//
// Built here rather than held on the struct so a delivery with no wrappers, or
// none that apply, allocates nothing.
func (c ResponseCodecs) newSink() *exchangeSink {
	if len(c.wrappers) == 0 {
		return nil
	}
	return newExchangeSink(c.exchange)
}

// AdmissionOf gates one exchange before any upstream work, and releases
// whatever it took once the exchange is over.
//
// Admit either takes what the request needs and returns a ticket, or reports
// why the request cannot proceed. It does not decide what a refusal costs: it
// says what it found and the table decides, same as every other shape.
//
// Release is called exactly once for every ticket Admit handed back, on every
// exit path including a panic. There is no separate settle-versus-compensate
// pair: the two differ only in what an implementation does with the outcome it
// is given, and the outcome is a parameter here, so an implementation that
// needs to distinguish them can, and the many that do not are not forced to
// split logic they share.
type AdmissionOf[V, T any] interface {
	Name() string
	Admit(ctx context.Context, view V, sink fact.Sink) (ticket T, held bool)
	Release(ctx context.Context, view V, ticket T, out fact.Outcome, sink fact.Sink)
}

// AdmissionPhase says WHEN an admission is asked, and it is supplied at
// registration for the same reason a rewriter's stage is: where something sits
// in the pipeline is a property of the pipeline being assembled, not of the
// thing itself.
//
// Two moments exist because the useful ones are genuinely different. Some
// admissions can answer the instant a request arrives — a rate limiter needs
// only the caller's identity, and asking early is the point, since the cheapest
// refusal is the one that happens before any work. Others cannot: an admission
// that reserves money has to know how much, and the amount depends on the body
// after every rewriter has had it and on the price of the candidate it is going
// to. Both of those are unknown on arrival, so an admission asked only then
// would have to guess, and a reservation made from a guess is either short —
// letting a request through the caller cannot pay for — or padded, refusing one
// they can.
//
// Both phases share one stack. Tickets are released newest-first across the
// whole of it, so a reservation taken in the second phase is given back before
// whatever the first phase took to make it possible.
type AdmissionPhase uint8

const (
	// AdmitOnArrival runs before the body has even been read: nothing about
	// this request is known yet beyond who is asking.
	AdmitOnArrival AdmissionPhase = 10
	// AdmitWhenPriced runs once the caller's body has been through every
	// ingress rewriter and a routable candidate exists, and still before
	// anything is sent upstream.
	//
	// Two things are NOT settled here, and an implementation that assumes
	// otherwise is wrong about money. The body is ingress-final, not outbound:
	// the modality re-encodes per candidate afterwards and the egress rewriters
	// append to that, the configured system prompt among them. And no candidate
	// has been committed to — the chain can skip or fail over, and settlement
	// prices whichever one actually served. So what can be computed here is a
	// floor against one candidate's rates, not the amount the request will cost.
	AdmitWhenPriced AdmissionPhase = 20
)

// admissionPhases is every phase the exchange actually asks, and registration
// is checked against it.
//
// Without the check an unrecognised value — a zero left by a struct literal, a
// constant added here but never wired into the exchange — registers happily and
// appears in the roster, and then nothing ever asks it. That is the worst
// possible failure for this shape: a gate that exists to refuse requests, and
// silently permits every one of them. A missing gate must be a startup failure,
// like a stage collision, rather than a permission nobody notices being granted.
var admissionPhases = []AdmissionPhase{AdmitOnArrival, AdmitWhenPriced}

func (p AdmissionPhase) supported() bool {
	return slices.Contains(admissionPhases, p)
}

// admission is the kernel-side, view-erased form. The ticket travels as an any:
// the kernel never inspects it, it only hands the same value back.
type admission interface {
	name() string
	phase() AdmissionPhase
	admit(ctx context.Context, e *Exchange, sink fact.Sink) (any, bool)
	release(ctx context.Context, e *Exchange, ticket any, out fact.Outcome, sink fact.Sink)
}

type admissionAdapter[V, T any] struct {
	inner   AdmissionOf[V, T]
	bind    func(*Exchange) V
	atPhase AdmissionPhase
	// Captured at registration; see the field note on
	// upstreamErrorObserverAdapter.
	registeredName string
}

func (a admissionAdapter[V, T]) name() string          { return a.registeredName }
func (a admissionAdapter[V, T]) phase() AdmissionPhase { return a.atPhase }

func (a admissionAdapter[V, T]) admit(ctx context.Context, e *Exchange, sink fact.Sink) (any, bool) {
	ticket, held := a.inner.Admit(ctx, a.bind(e), sink)
	return ticket, held
}

func (a admissionAdapter[V, T]) release(ctx context.Context, e *Exchange, ticket any, out fact.Outcome, sink fact.Sink) {
	typed, ok := ticket.(T)
	if !ok {
		// Unreachable: the kernel hands back the same value it received from
		// this same adapter. Guarded anyway because a wrong ticket would
		// otherwise release something another admission is holding.
		logger.Error("gateway: admission ticket type mismatch on release",
			zap.String("admission", a.registeredName))
		return
	}
	a.inner.Release(ctx, a.bind(e), typed, out, sink)
}

// RegisterAdmission wires an admission into the service at the given phase.
//
// Registration order is acquisition order WITHIN a phase, and release runs in
// reverse across every phase — plain stack discipline, which is what makes
// "release what was taken last, first" true by construction rather than by
// everyone agreeing on a set of ordinal constants nobody can get wrong only if
// they are all correct.
//
// The sort is stable, and that is what keeps the sentence above true: it makes
// the slice read in acquisition order regardless of the order the assembly
// happened to register phases in, without disturbing the relative order of the
// admissions sharing one.
func RegisterAdmission[V, T any](s *Service, a AdmissionOf[V, T], at AdmissionPhase, bind func(*Exchange) V) {
	if !at.supported() {
		panic(fmt.Sprintf("gateway: admission %q registered at phase %d, which nothing asks: it would never gate anything",
			a.Name(), at))
	}
	s.admissions = append(s.admissions, admissionAdapter[V, T]{inner: a, bind: bind, atPhase: at, registeredName: a.Name()})
	sort.SliceStable(s.admissions, func(i, j int) bool {
		return s.admissions[i].phase() < s.admissions[j].phase()
	})
}

// RegisteredAdmissions names the admissions in acquisition order, which is the
// reverse of the order they are compensated in.
//
// Stack discipline makes "release what was taken last, first" true by
// construction, but it leaves WHICH order that is as a property of the lines in
// the assembly function — and "the balance pre-deduct must be reversed after the
// sub-request charge" is a real constraint that no longer has anywhere to live
// once it is only an ordering of statements. This is what lets the assembly pin
// it: exported because the assembly, and therefore the test that pins it, is
// necessarily in another package.
func (s *Service) RegisteredAdmissions() []string {
	out := make([]string, len(s.admissions))
	for i, a := range s.admissions {
		out[i] = a.name()
	}
	return out
}

// heldTicket pairs a ticket with the admission that issued it.
type heldTicket struct {
	by     admission
	ticket any
}

// admit runs the registered admissions in order and stops at the first refusal.
//
// Stopping is not an optimisation: an admission that runs after a refusal would
// take a resource for a request that is already over, and for a rate limiter
// that means charging the caller for a request nobody serves.
//
// Tickets are appended to *held as they are acquired rather than returned at
// the end, so the caller can arm its release BEFORE calling this. If a later
// admission panics, everything taken so far is already recorded where the
// caller's deferred release can see it; had the tickets only appeared in a
// return value, that release would never have been installed and the resources
// would be held until the process restarts.
func (s *Service) admit(ctx context.Context, rc *Exchange, at AdmissionPhase, held *[]heldTicket) decision.Resolved {
	if len(s.admissions) == 0 {
		return decision.Resolved{}
	}
	sink := newExchangeSink(rc)
	for _, a := range s.admissions {
		if a.phase() != at {
			continue
		}
		sink.reporter = a.name()
		ticket, ok := a.admit(ctx, rc, sink)
		if ok {
			*held = append(*held, heldTicket{by: a, ticket: ticket})
		}
		if verdict := sink.resolve(); verdict.Loop >= decision.LoopNextCandidate {
			return verdict
		}
	}
	return sink.resolve()
}

// releaseAdmissions returns everything that was taken, most recent first.
//
// Each release runs under its own detached, bounded context — detached for the
// same reason the recorders and delivery observers are (a release is money
// coming back, and it runs from a defer on the way out, exactly when the
// caller hanging up or the request deadline lapsing has cancelled
// rc.requestCtx), and per ticket rather than shared because these run in a
// chain: one release exhausting a shared budget would hand every release after
// it a context that is already dead, and the reservations THEY hold would stay
// held. The bound also applies when the request context was still perfectly
// alive — a release used to run with no clock at all in that case, and a stuck
// backend could pin the goroutine indefinitely.
func (s *Service) releaseAdmissions(ctx context.Context, rc *Exchange, held []heldTicket, out fact.Outcome) {
	if len(held) == 0 {
		return
	}
	if ctx == nil {
		// Reached only from a caller that never established a request context.
		ctx = context.Background()
	}
	detached := context.WithoutCancel(ctx)
	sink := newExchangeSink(rc)
	for i := len(held) - 1; i >= 0; i-- {
		name := held[i].by.name()
		sink.reporter = name
		ticketCtx, cancel := context.WithTimeout(detached, releaseBudget)
		// Guarded like the other places capability code runs, and for a sharper
		// reason than most. This is a defer on the way out, after the caller has
		// been served: a panic here escapes into the HTTP framework's own
		// recovery, which knows nothing about the tickets still held. Every
		// reversal that had not run yet is skipped — the money one of them was
		// holding stays reserved, and the request that was served correctly
		// takes the row down with it.
		func() {
			defer func() {
				if v := recover(); v != nil {
					logger.Error("gateway: an admission panicked while releasing",
						zap.String("admission", name),
						zap.String("request_id", rc.requestID),
						zap.Any("panic", v))
				}
			}()
			held[i].by.release(ticketCtx, rc, held[i].ticket, isolateOutcome(out), sink)
		}()
		cancel()
	}
}

// recorderWriteBudget bounds how long the audit trail may take to persist. It
// runs after the caller has been served, so it delays nothing the caller sees;
// the bound exists so a stuck database cannot pin goroutines indefinitely.
const recorderWriteBudget = 5 * time.Second

// releaseBudget bounds how long EACH admission may take to give back what it
// holds — per ticket, not a shared total, so one stuck backend cannot starve
// the releases queued behind it. Same rationale as the recorder's budget: the
// work is too important to inherit the caller's cancellation, and too
// unbounded to run with no clock at all.
const releaseBudget = 5 * time.Second

// RecorderOf is the terminal fan-in: called exactly once per exchange, on every
// exit path, after the outcome is settled.
//
// It receives the whole timeline. That is the point of the shape: a recorder
// learns what happened from what was reported, not by reaching into the
// exchange for each capability's private field. A capability that starts
// reporting something new needs no change here to have it persisted, and one
// that stops reporting cannot leave a stale field behind that still looks
// current.
//
// The timeline arrives by value so that recorders are readers. Several of them
// run in sequence over the same history, and one that grew or edited it would
// decide what the others get to see.
type RecorderOf[V any] interface {
	Name() string
	Record(ctx context.Context, view V, out fact.Outcome, tl fact.Timeline)
}

// recorder is the kernel-side, view-erased form.
type recorder interface {
	name() string
	record(ctx context.Context, e *Exchange, out fact.Outcome, tl fact.Timeline)
}

type recorderAdapter[V any] struct {
	inner RecorderOf[V]
	bind  func(*Exchange) V
	// Captured at registration; see the field note on
	// upstreamErrorObserverAdapter.
	registeredName string
}

func (a recorderAdapter[V]) name() string { return a.registeredName }

func (a recorderAdapter[V]) record(ctx context.Context, e *Exchange, out fact.Outcome, tl fact.Timeline) {
	a.inner.Record(ctx, a.bind(e), out, tl)
}

// RegisterRecorder wires a recorder into the service.
func RegisterRecorder[V any](s *Service, r RecorderOf[V], bind func(*Exchange) V) {
	s.recorders = append(s.recorders, recorderAdapter[V]{inner: r, bind: bind, registeredName: r.Name()})
}

// runRecorders hands the settled exchange to every recorder.
//
// A recorder that panics must not take the others down with it, nor the request
// it is describing: by the time this runs the caller has already been served,
// and losing an audit row is bad where failing a served request would be worse.
func (s *Service) runRecorders(ctx context.Context, rc *Exchange, out fact.Outcome) {
	if len(s.recorders) == 0 {
		return
	}
	if ctx == nil {
		// Reached only from a caller that never established a request context.
		ctx = context.Background()
	}
	// The audit trail is written on a context detached from the request's own
	// cancellation, and bounded separately. A cancelled caller is exactly the
	// case where the row matters most — a request that ended in a disconnect is
	// one somebody will come looking for — and inheriting that cancellation
	// means the write fails precisely then. The bound keeps a stuck database
	// from holding the goroutine after the caller is long gone.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recorderWriteBudget)
	defer cancel()

	for _, r := range s.recorders {
		func() {
			defer func() {
				if v := recover(); v != nil {
					logger.Error("gateway: recorder panicked",
						zap.String("recorder", r.name()),
						zap.String("request_id", rc.requestID),
						zap.Any("panic", v))
				}
			}()
			r.record(ctx, rc, isolateOutcome(out), rc.timeline)
		}()
	}
}
