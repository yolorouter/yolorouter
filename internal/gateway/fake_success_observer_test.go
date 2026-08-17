package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/decision"
	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/claude"
)

// This file is one of the fake capabilities. They exist to put weight on the
// extension points from outside the kernel — the same thing a real capability
// does, with none of the business logic — and they never ship: everything here
// is compiled only under `go test`.
//
// The one below is the load-bearing case. The only shape that could REPORT
// something about a response ran on failures alone: the upstream-error observer
// sees non-2xx responses and nothing else, by its own name. A recorder does run
// on every exit path, but it is the terminal reader — it writes the row from
// what others reported and has no sink to report through itself, so a
// capability with something to say about a response that WORKED had nowhere to
// say it. No call site existed, and the first capability that needed one would
// have discovered that halfway through being ported. This test is what makes
// the absence of that call site a build failure here instead.

// surchargeObserved is what the fake below reports. A record type declared in a
// test is the point rather than a shortcut: this vocabulary is open, so a
// capability the kernel has never heard of must be able to report something the
// kernel has never heard of and still have it persisted under its name.
type surchargeObserved struct {
	fact.Base
	Reason   string
	Searches int
}

func (surchargeObserved) RecordName() string { return "fake_surcharge_observed" }

// billedView is this capability's own view of the exchange — nothing but the
// request id. Declared narrow on purpose: it is the compile-time evidence that
// an observer states what it needs rather than receiving the whole Exchange,
// and the bind function at registration is where that claim is checked.
type billedView struct {
	requestID string
}

// bindBilledView is the view every observer in this file is registered with.
//
// Named rather than written inline at each registration: the bind function is
// what decides how much of the Exchange a capability can reach, so eight copies
// of it are eight places for that answer to quietly diverge.
func bindBilledView(e *Exchange) billedView {
	return billedView{requestID: e.requestID}
}

// fakeSurcharge stands in for a capability that charges for something the
// provider only reveals in a response that WORKED — a web search performed
// mid-completion, an image the model decided to generate. Its defining
// property, and the reason it is the fake that matters, is that a failed
// exchange tells it nothing.
type fakeSurcharge struct {
	sawDeliveries int
	sawUsage      *fact.UsageReported
	// meddle, when set, is what this observer tries to do to the delivery it
	// was shown. Nothing here is supposed to be able to change what the request
	// bills; the field is how that is put to the test rather than assumed.
	meddle func(d fact.Delivery, sink fact.Sink)
}

func (*fakeSurcharge) Name() string { return "fake_surcharge" }

func (f *fakeSurcharge) ObserveDelivery(_ context.Context, _ billedView, d fact.Delivery, sink fact.Sink) {
	f.sawDeliveries++
	f.sawUsage = d.Usage
	if f.meddle != nil {
		f.meddle(d, sink)
		return
	}
	if d.Complete && d.Usage != nil && d.Usage.WebSearchCount > 0 {
		sink.Note(surchargeObserved{Reason: "web_search", Searches: d.Usage.WebSearchCount})
	}
}

// TestACapabilityCanBeToldAboutAResponseThatWorked is the gate this whole file
// exists for.
//
// Reporting on success is not a variation on reporting on failure. The facts
// worth reporting are different ones — what an upstream charged extra for, how
// many tokens it actually spent, whether the answer ran to the end — and they
// exist only when there is an answer. An observation shape that can only be
// reached by failing cannot carry any of them.
func TestACapabilityCanBeToldAboutAResponseThatWorked(t *testing.T) {
	svc := &Service{}
	rc := &Exchange{requestID: "req-success-observed"}

	spy := &fakeSurcharge{}
	RegisterDeliveryObserver(svc, spy, bindBilledView)

	// A count the provider charges for separately, reported once, in the usage
	// the response ends with. Nothing downstream can re-derive it: the frames it
	// was read from are gone by the time this observer runs, which is exactly
	// why it has to survive into the delivery's own usage.
	delivered := fact.Succeeded(200).WithUsage(&fact.UsageReported{
		Prompt: 11, Completion: 7, WebSearchCount: 3,
	})
	settleOneDelivery(t, svc, rc, delivered)

	if spy.sawDeliveries != 1 {
		t.Fatalf("the observer ran %d times on a successful exchange, want 1", spy.sawDeliveries)
	}
	if spy.sawUsage == nil || spy.sawUsage.Completion != 7 {
		t.Errorf("observer saw usage %+v, want the counts the delivery carried — "+
			"an observation point that cannot see them is one no billing capability can use", spy.sawUsage)
	}

	var reported *fact.Entry
	for i, e := range rc.timeline.All() {
		if _, ok := e.Record.(surchargeObserved); ok {
			reported = &rc.timeline.All()[i]
		}
	}
	if reported == nil {
		t.Fatal("what the observer reported never reached the timeline, which is the only route it has to the audit row")
	}
	if got := reported.Record.(surchargeObserved); got.Searches != 3 {
		t.Errorf("recorded %d searches, want 3: a surcharge counted from anything but the "+
			"provider's own number is a guess", got.Searches)
	}
	if reported.Reporter != "fake_surcharge" {
		t.Errorf("reporter = %q, want the observer's own name: a record nobody is attributed for "+
			"cannot be traced back to the capability that made the claim", reported.Reporter)
	}
}

// TestAFailedExchangeTellsTheSuccessObserverNothingWorthBilling pins the other
// half of the same seam: the observer runs on every settlement, and deciding
// what a failure means is ITS business, not a filter the kernel applies.
//
// The kernel deliberately does not gate this shape on "success". It has no
// definition of the word that would suit every capability — a truncated stream
// is a failure to one and a billable partial answer to another — and a delivery
// says enough about itself for the capability to decide.
func TestAFailedExchangeTellsTheSuccessObserverNothingWorthBilling(t *testing.T) {
	svc := &Service{}
	rc := &Exchange{requestID: "req-failed-observed"}

	spy := &fakeSurcharge{}
	RegisterDeliveryObserver(svc, spy, bindBilledView)

	settleOneDelivery(t, svc, rc, fact.Rejected(502, fact.FaultUpstream, "all_candidates_failed", nil))

	if spy.sawDeliveries != 1 {
		t.Fatalf("the observer ran %d times, want 1: it is told how every exchange ended", spy.sawDeliveries)
	}
	for _, e := range rc.timeline.All() {
		if _, ok := e.Record.(surchargeObserved); ok {
			t.Fatal("a surcharge was recorded for an exchange that delivered nothing")
		}
	}
}

// TestAnObserverCannotChargeTheCallerFromInsideAnObservation pins the isolation
// this shape needs and the upstream-error shape needed before it.
//
// Delivery travels by value, so a careless observer cannot reach its fields.
// Usage is a pointer, and it is the one settlement is about to bill from —
// adding a surcharge to the counts in front of you is the obvious mistake, it
// looks like it works, and it would change what every observer after you sees
// as well.
func TestAnObserverCannotChargeTheCallerFromInsideAnObservation(t *testing.T) {
	svc := &Service{}
	rc := &Exchange{requestID: "req-meddling-observer"}

	meddler := &fakeSurcharge{meddle: func(d fact.Delivery, _ fact.Sink) {
		if d.Usage != nil {
			d.Usage.Completion += 1000
		}
	}}
	RegisterDeliveryObserver(svc, meddler, bindBilledView)
	downstream := &fakeSurcharge{}
	RegisterDeliveryObserver(svc, downstream, bindBilledView)

	usage := &fact.UsageReported{Prompt: 11, Completion: 7}
	settleOneDelivery(t, svc, rc, fact.Succeeded(200).WithUsage(usage))

	if usage.Completion != 7 {
		t.Errorf("the delivery's own usage now reads %d completion tokens, want 7: "+
			"an observer rewrote what the caller is charged for", usage.Completion)
	}
	if downstream.sawUsage == nil || downstream.sawUsage.Completion != 7 {
		t.Errorf("the next observer saw %+v, want the untouched counts: one observer changed "+
			"what another one is told, which makes the outcome depend on registration order",
			downstream.sawUsage)
	}
	var billed *fact.UsageReported
	for _, e := range rc.timeline.All() {
		if v, ok := e.Record.(fact.UsageReported); ok {
			billed = &v
		}
	}
	if billed == nil {
		t.Fatal("nothing was billed at all; this test can no longer tell whether the meddling landed")
	}
	if billed.Completion != 7 {
		t.Errorf("billed %d completion tokens, want 7", billed.Completion)
	}
}

// TestAnObserverCannotSteerASettledRequest pins the one thing this shape must
// refuse.
//
// A fact carrying a Loop effect asks the kernel to move the chain. By the time
// this runs the caller has been served and the row is being written, so
// honouring it would mean settling the same request a second time. The verdict
// is read and refused rather than never computed, because a report that cannot
// be acted on is a mistake somebody should hear about.
func TestAnObserverCannotSteerASettledRequest(t *testing.T) {
	svc := &Service{}
	rc := &Exchange{requestID: "req-steering-observer"}

	steerer := &fakeSurcharge{meddle: func(_ fact.Delivery, sink fact.Sink) {
		sink.Report(fact.Fact{Kind: fact.KindPayloadRefused})
	}}
	RegisterDeliveryObserver(svc, steerer, bindBilledView)

	if !decision.For(fact.KindPayloadRefused).Defined {
		t.Fatal("the fixture no longer reports a fact with any effect; pick a Kind the table defines")
	}

	var got attemptResult
	out := captureWarnings(t, func() {
		got = settleOneDelivery(t, svc, rc, fact.Succeeded(200))
	})

	if got != attemptSuccess {
		t.Errorf("result = %v, want the request to stay settled: an observation must not reopen a served request", got)
	}
	if len(rc.attempts) != 1 {
		t.Errorf("attempts = %d, want 1: the chain moved on a report made after the caller was served", len(rc.attempts))
	}
	// Refused, not ignored. An effect nobody can act on is a bug in the
	// capability, and the only way its author finds out is if somebody says so.
	if !strings.Contains(out, "req-steering-observer") {
		t.Errorf("nothing was logged about an effect reported too late to honour; log said: %s", out)
	}
	// Named by the capability that made the claim. Without it the operator is
	// told an effect was reported and given no way to find who reported it.
	if !strings.Contains(out, "fake_surcharge") {
		t.Errorf("the refusal did not name the observer that reported it; log said: %s", out)
	}
}

// TestCountsJudgedImpossibleAreNotPricedAfterTheDeliveryHop is the round trip
// that decides whether an upstream can be believed.
//
// A stream folds every frame into one accumulated record. When one frame is
// impossible — a negative token count — the record is marked, and then the
// offending number is overwritten by the next frame that carries a plausible
// one. The mark is the only thing left saying the counts cannot be trusted.
//
// Settlement does not read the exchange's own usage. It reads the copy that
// travelled on the delivery, converted out and back. If the mark does not
// survive those two hops, what arrives is a set of perfectly ordinary-looking
// numbers, and they are priced — the request is charged on figures the gateway
// had already judged impossible, and nothing downstream can tell, because the
// frame that proved it is gone.
func TestCountsJudgedImpossibleAreNotPricedAfterTheDeliveryHop(t *testing.T) {
	condemned := &protocols.IRUsage{PromptTokens: 1000, CompletionTokens: 2000, Invalid: true}

	// Out to the delivery's vocabulary and back, which is the trip settlement
	// makes before it prices anything.
	roundTripped := usageFromReport(usageReportOf(condemned))
	if roundTripped == nil {
		t.Fatal("the usage did not survive the delivery hop at all")
	}
	if !roundTripped.Invalid {
		t.Fatal("the impossibility verdict was dropped in transit; what settlement receives is " +
			"a plausible-looking record it will price")
	}
	if usageIsCoherent(roundTripped) {
		t.Error("the round-tripped record reads as coherent, so the billing gate lets it through")
	}
	// The counts still travel — the audit row is where a contradiction has to
	// stay visible, and dropping them would hide it rather than record it.
	if roundTripped.PromptTokens != 1000 || roundTripped.CompletionTokens != 2000 {
		t.Errorf("counts = %d/%d, want them kept alongside the verdict",
			roundTripped.PromptTokens, roundTripped.CompletionTokens)
	}
}

// TestAnEffectThatSteersNothingIsStillRefused is the half of the refusal that
// a Loop-shaped test cannot reach.
//
// Loop is one of six effects, and by the time an observation runs none of them
// can be honoured. Testing only with a fact that carries a Loop leaves the
// other five unguarded — and the check would pass just as well if it asked
// "did this steer?" instead of "did this decide anything?". A fact with no Kind
// set is the plainest case: the table defines it, it steers nothing, and
// reporting it here is still a capability asking for something impossible.
func TestAnEffectThatSteersNothingIsStillRefused(t *testing.T) {
	if d := decision.For(fact.KindNone); !d.Defined || d.Loop != decision.LoopNone {
		t.Fatalf("the fixture no longer describes a defined, non-steering decision: %+v", d)
	}

	svc := &Service{}
	rc := &Exchange{requestID: "req-nonsteering-observer"}
	quiet := &fakeSurcharge{meddle: func(_ fact.Delivery, sink fact.Sink) {
		sink.Report(fact.Fact{Detail: "an effect that steers nothing"})
	}}
	RegisterDeliveryObserver(svc, quiet, bindBilledView)

	out := captureWarnings(t, func() {
		settleOneDelivery(t, svc, rc, fact.Succeeded(200))
	})

	if !strings.Contains(out, "reported an effect on a settled request") {
		t.Errorf("a decision that was defined but steers nothing went unreported; log said: %s", out)
	}
	if !strings.Contains(out, "fake_surcharge") {
		t.Errorf("the refusal did not name the observer; log said: %s", out)
	}
}

// TestAQuietObserverIsNotBlamedForWhatTheKernelReported is the other side of
// that log line, and the reason each observer reports through its own sink.
//
// The kernel reports through a sink of its own before any observer runs. Handed
// that same sink, an observer that reports nothing at all still resolves to
// whatever was already in it — so the refusal above fires for an exchange where
// no capability did anything wrong, and names one anyway.
func TestAQuietObserverIsNotBlamedForWhatTheKernelReported(t *testing.T) {
	svc := &Service{}
	rc := &Exchange{requestID: "req-quiet-observer"}

	quiet := &fakeSurcharge{meddle: func(fact.Delivery, fact.Sink) {}}
	RegisterDeliveryObserver(svc, quiet, bindBilledView)

	out := captureWarnings(t, func() {
		sink := newExchangeSink(rc)
		// Whatever the kernel had to say about this exchange, said before the
		// observers run — the ordinary case, not a contrived one.
		sink.Report(fact.Fact{Kind: fact.KindPayloadRefused})
		svc.observeDelivery(rc, fact.Succeeded(200), sink)
	})

	if quiet.sawDeliveries != 1 {
		t.Fatalf("the observer ran %d times, want 1", quiet.sawDeliveries)
	}
	if strings.Contains(out, "reported an effect on a settled request") {
		t.Errorf("an observer that reported nothing was blamed for the kernel's own report; log said: %s", out)
	}
}

// TestAPanickingObserverDoesNotCostTheRequestItsRow is the one failure mode
// that only exists because this shape runs AFTER the caller has been served.
//
// Everywhere earlier in the request, a capability that panics takes the request
// down with it and that is the honest outcome — nobody was served, and a 500 is
// what happened. Here the caller already has a correct answer. An unguarded
// panic would unwind into the request's own recovery, which settles what it
// finds as a gateway 500: the served request would be recorded as a failure,
// with no usage and no cost, because a third party's observation threw.
func TestAPanickingObserverDoesNotCostTheRequestItsRow(t *testing.T) {
	svc := &Service{}
	rc := &Exchange{requestID: "req-panicking-observer"}

	boom := &fakeSurcharge{meddle: func(fact.Delivery, fact.Sink) {
		panic("a capability with a nil map")
	}}
	RegisterDeliveryObserver(svc, boom, bindBilledView)
	after := &fakeSurcharge{}
	RegisterDeliveryObserver(svc, after, bindBilledView)

	got := settleOneDelivery(t, svc, rc,
		fact.Succeeded(200).WithUsage(&fact.UsageReported{Prompt: 11, Completion: 7}))

	if got != attemptSuccess {
		t.Fatalf("result = %v, want the request still settled", got)
	}
	if rc.statusCode != 200 {
		t.Errorf("settled status = %d, want 200: the caller was served correctly and an "+
			"observation that threw rewrote their request into a failure", rc.statusCode)
	}
	if after.sawDeliveries != 1 {
		t.Errorf("the observer after the panicking one ran %d times, want 1", after.sawDeliveries)
	}
	var billed *fact.UsageReported
	for _, e := range rc.timeline.All() {
		if v, ok := e.Record.(fact.UsageReported); ok {
			billed = &v
		}
	}
	if billed == nil || billed.Completion != 7 {
		t.Errorf("billed usage = %+v, want the 7 completion tokens the delivery reported", billed)
	}
}

// namelessObserver panics when asked what it is called.
type namelessObserver struct{ fakeSurcharge }

func (*namelessObserver) Name() string { panic("a capability with a nil config") }

// TestAnObserverIsAskedItsNameAtAssemblyNotAtSettlement pins where the second
// unguarded call into capability code used to be.
//
// The guard around the observation is only as good as what sits outside it, and
// Name() is capability code too. Asked on the settlement path it would run
// BEFORE the recovery was armed, and asking again inside the recovery — to say
// which observer failed — would re-enter the code that just failed. Asked once
// at registration, a broken Name is a process that does not start, which is
// where that class of mistake belongs.
func TestAnObserverIsAskedItsNameAtAssemblyNotAtSettlement(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("registering an observer whose Name panics succeeded; the name is then " +
				"being read somewhere later, and the only later there is is the settlement path")
		}
	}()
	RegisterDeliveryObserver(&Service{}, &namelessObserver{}, bindBilledView)
}

// TestNothingIsAttributedToAnObserverWhenNoneAreRegistered keeps the shape free
// for a deployment that wires none of them, which is every deployment today.
//
// It does NOT pin the early return in observeDelivery. That return is an
// allocation fast path — with no observers the loop body never runs either, so
// removing it changes nothing this or any other test can see. What is worth
// holding is the property below: a settlement with no observers records nothing
// that claims to have come from one.
func TestNothingIsAttributedToAnObserverWhenNoneAreRegistered(t *testing.T) {
	svc := &Service{}
	rc := &Exchange{requestID: "req-no-observers", ingress: protocols.ProtocolOpenAI}

	if got := settleOneDelivery(t, svc, rc, fact.Succeeded(200)); got != attemptSuccess {
		t.Errorf("result = %v, want the settlement to go through untouched", got)
	}
	for _, e := range rc.timeline.All() {
		if _, ok := e.Record.(surchargeObserved); ok {
			t.Fatalf("a surcharge was recorded on a request with no observer to report it: %+v", e)
		}
	}
}

// TestAnObservationOutlivesTheCallerHangingUp is the reason observations do not
// run under the request's own context.
//
// A caller who leaves mid-stream cancels that context — and that is precisely
// the exchange with something to record, because the upstream has already done
// and charged for the work. An observer handed the cancelled context cannot
// write any of it down.
func TestAnObservationOutlivesTheCallerHangingUp(t *testing.T) {
	svc := &Service{}
	var seen error
	probe := &contextProbe{err: &seen}
	RegisterDeliveryObserver(svc, probe, bindBilledView)

	rc := &Exchange{
		requestID:  "req-caller-left",
		ingress:    protocols.ProtocolOpenAI,
		requestCtx: cancelledContext(),
	}
	settleOneDelivery(t, svc, rc, fact.Truncated(200, 499, fact.FaultClient, "client_disconnected", nil))

	// Asserted before the context itself: "the observer saw a live context" and
	// "the observer never ran" are the same nil, and only one of them is the
	// property this test is named for.
	if !probe.ran {
		t.Fatal("the observer never ran, so this proves nothing about the context it would have been given")
	}
	if seen != nil {
		t.Errorf("the observer was handed a dead context (%v); the caller leaving is not a "+
			"reason to stop recording what an upstream already charged for", seen)
	}
}

// contextProbe reports whether it ran and whether the context it was given was
// already finished.
type contextProbe struct {
	err *error
	ran bool
}

func (*contextProbe) Name() string { return "context_probe" }

func (p *contextProbe) ObserveDelivery(ctx context.Context, _ billedView, _ fact.Delivery, _ fact.Sink) {
	p.ran = true
	*p.err = ctx.Err()
}

// TestTheSearchCountSurvivesEveryHopToTheObserver is what the fake above
// depends on and could not prove by itself.
//
// The fake is handed a delivery with the count already on it, so it shows that
// the SHAPE can carry one. What decides whether a real capability ever sees it
// is the chain in front of that: the provider states the count in the response
// body, which is decoded into an IR usage, which becomes a gateway usage, which
// becomes the record a delivery carries and the record the audit row is written
// from. Any hop that does not name the field drops it silently — the struct
// still converts, the tests still pass, and the number is gone by the time
// anyone could act on it. The count cannot be re-derived afterwards; the body it
// was read from does not survive the delivery.
//
// It starts from a response body rather than a hand-built IR usage on purpose.
// Built from the middle, "every hop" meant only the struct copies inside this
// package, and the decoder in front of them went unread for as long as the test
// existed — which is exactly where the field was being dropped.
func TestTheSearchCountSurvivesEveryHopToTheObserver(t *testing.T) {
	body := []byte(`{
		"id": "msg_1",
		"model": "claude-sonnet-4-5",
		"stop_reason": "end_turn",
		"content": [{"type": "text", "text": "ok"}],
		"usage": {"input_tokens": 11, "output_tokens": 7,
			"server_tool_use": {"web_search_requests": 3}}
	}`)
	irResp, err := claude.ResponseDecoder{}.DecodeResponse(body)
	if err != nil {
		t.Fatalf("decoding the upstream response: %v", err)
	}
	if irResp.Usage.WebSearchCount != 3 {
		t.Fatalf("searches = %d straight out of the decoder, want 3: the provider stated "+
			"three and this is the only place that number is ever read",
			irResp.Usage.WebSearchCount)
	}
	decoded := &irResp.Usage

	onExchange := reportedUsage(decoded)
	if onExchange == nil {
		t.Fatal("the usage did not survive the reported-usage gate at all")
	}
	if onExchange.WebSearchCount != 3 {
		t.Fatalf("searches = %d after the reported-usage gate, want 3", onExchange.WebSearchCount)
	}

	reported := usageReportOf(onExchange)
	if reported == nil {
		t.Fatal("no usage was reported for a decoded response")
	}
	if reported.WebSearchCount != 3 {
		t.Fatalf("searches = %d in the reported usage, want 3: this is the record a delivery "+
			"carries, and an observer has nothing else to read", reported.WebSearchCount)
	}

	// And back, because settlement converts the other way and a one-directional
	// field is one the audit row disagrees with the delivery about.
	backOnExchange := usageFromReport(reported)
	if backOnExchange == nil || backOnExchange.WebSearchCount != 3 {
		t.Errorf("searches = %+v on the way back to settlement, want 3", backOnExchange)
	}
	// The audit row is written from its own conversion, not from the one above.
	// A capability that reports a surcharge sees the count; without this hop the
	// row the count is reconciled against says none were run, so the two
	// disagree and only one of them is persisted.
	rc := &Exchange{requestID: "req-searches", ingress: protocols.ProtocolOpenAI}
	sink := newExchangeSink(rc)
	(&Service{}).reportUsage(rc, onExchange, sink)
	if got := requireBilled(t, rc).WebSearchCount; got != 3 {
		t.Errorf("searches = %d in the audit row, want 3: this is the record that outlives "+
			"the request, and the body the count came from does not", got)
	}
}
