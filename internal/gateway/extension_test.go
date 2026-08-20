package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/decision"
	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// stubObserver reports one fact so a test can inspect what the sink stamped on
// it.
type stubObserver struct{}

func (stubObserver) Name() string { return "stub" }

func (stubObserver) ObserveUpstreamError(_ context.Context, _ struct{}, up fact.Upstream, sink fact.Sink) {
	sink.Report(fact.Fact{Kind: fact.KindPayloadRefused, Status: up.StatusCode})
}

// TestObserveUpstreamStampsProvenance pins the provenance the sink writes onto
// every entry.
//
// Every provenance field has a meaningful zero, so getting one wrong produces
// audit rows that are wrong rather than obviously empty: an entry claiming
// candidate 0 refused the payload reads exactly like a real one. Nothing reads
// the timeline yet, which is precisely why this needs a test — a regression here
// would stay invisible until the audit trail is wired up, and by then the cause
// would be several changes back.
func TestObserveUpstreamStampsProvenance(t *testing.T) {
	svc := &Service{}
	RegisterUpstreamErrorObserver(svc, stubObserver{}, attemptView)

	rc := &Exchange{
		attempts: make([]AttemptRecord, 2), // two attempts already recorded
	}
	rc.attempt.BeginCandidate(&model.ModelCandidate{ID: 77})
	rc.attempt.BindProvider(&model.Provider{ID: 42})

	got := svc.observeUpstreamError(context.Background(), rc, fact.Upstream{StatusCode: 400})
	if got.Loop != decision.LoopNextCandidate {
		t.Fatalf("want the refusal to resolve to a failover, got loop %v", got.Loop)
	}

	entries := rc.timeline.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 timeline entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Candidate != 77 {
		t.Errorf("candidate = %d, want 77", e.Candidate)
	}
	if e.Provider != 42 {
		t.Errorf("provider = %d, want 42", e.Provider)
	}
	// The sink is built before the attempt record is appended, so the report
	// belongs to the attempt about to be recorded — index 2 here, not 1.
	if e.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", e.Attempt)
	}
	if e.Reporter != "stub" {
		t.Errorf("reporter = %q, want %q", e.Reporter, "stub")
	}
}

// TestObserveUpstreamToleratesClearedTarget covers the relay's own habit of
// clearing the provider when a candidate turns out to be unusable. Attributing
// such a report to whichever provider happened to be set last would be worse
// than attributing it to none, so an absent target must stay absent rather than
// panic or borrow.
func TestObserveUpstreamToleratesClearedTarget(t *testing.T) {
	svc := &Service{}
	RegisterUpstreamErrorObserver(svc, stubObserver{}, attemptView)

	rc := &Exchange{} // no candidate, no provider

	svc.observeUpstreamError(context.Background(), rc, fact.Upstream{StatusCode: 400})

	entries := rc.timeline.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 timeline entry, got %d", len(entries))
	}
	if entries[0].Candidate != 0 || entries[0].Provider != 0 {
		t.Errorf("want an unattributed entry, got candidate %d provider %d",
			entries[0].Candidate, entries[0].Provider)
	}
}

// TestObserveUpstreamWithNoObserversIsInert confirms a kernel with nothing
// registered reports nothing and steers nothing — the bare Service must not
// invent a verdict of its own.
func TestObserveUpstreamWithNoObserversIsInert(t *testing.T) {
	svc := &Service{}
	rc := &Exchange{}

	got := svc.observeUpstreamError(context.Background(), rc, fact.Upstream{StatusCode: 400})
	if got.Loop != decision.LoopNone {
		t.Errorf("loop = %v, want LoopNone", got.Loop)
	}
	if len(rc.timeline.All()) != 0 {
		t.Errorf("want an empty timeline, got %d entries", len(rc.timeline.All()))
	}
}

// refusingRewriter declares the body unusable. Nothing in the tree does this
// yet, which is exactly why it needs a test: the branch that handles a refusal
// is otherwise unreachable, and unreachable code that guards a contract is code
// that has never been shown to honour it.
type refusingRewriter struct{}

func (refusingRewriter) Name() string { return "refuser" }

func (refusingRewriter) RewriteEgress(_ context.Context, _ struct{}, _ protocols.ProtocolID, _ []byte, _ fact.Sink) ([]byte, error) {
	return nil, errors.New("cannot produce a usable body")
}

// mutatingRewriter rewrites successfully, so a test can prove a refusal stops
// the chain before later rewriters run.
type mutatingRewriter struct{ ran *bool }

func (mutatingRewriter) Name() string { return "mutator" }

func (m mutatingRewriter) RewriteEgress(_ context.Context, _ struct{}, _ protocols.ProtocolID, body []byte, _ fact.Sink) ([]byte, error) {
	*m.ran = true
	return append(body, '!'), nil
}

// TestRewriteEgressRefusalStopsAndReportsAVerdict pins the contract the
// interface states: an error means the body must not be sent. Returning the
// last good body instead would put upstream exactly what a rewriter refused to
// produce.
func TestRewriteEgressRefusalStopsAndReportsAVerdict(t *testing.T) {
	svc := &Service{}
	laterRan := false
	RegisterEgressRewriter(svc, refusingRewriter{}, 10, attemptView)
	RegisterEgressRewriter(svc, mutatingRewriter{ran: &laterRan}, 20, attemptView)

	rc := &Exchange{}
	body := []byte(`{"a":1}`)
	out, verdict := svc.rewriteEgress(context.Background(), rc, protocols.ProtocolOpenAI, body)

	if verdict.Loop != decision.LoopNextCandidate {
		t.Fatalf("loop = %v, want decision.LoopNextCandidate so the candidate is skipped", verdict.Loop)
	}
	if verdict.LoopFrom() != fact.KindEgressRewriteFailed {
		t.Errorf("loopFrom = %s, want egress_rewrite_failed", verdict.LoopFrom())
	}
	if string(out) != string(body) {
		t.Errorf("body = %q, want it left as it was; a refused body must not be handed on", out)
	}
	if laterRan {
		t.Error("a later rewriter ran after the body was refused")
	}
	if len(rc.timeline.All()) != 1 {
		t.Errorf("want the refusal on the timeline, got %d entries", len(rc.timeline.All()))
	}
}

// TestRewriteEgressRunsInStageOrder confirms order comes from the stage given
// at registration, not from the order the calls happened to be made in.
func TestRewriteEgressRunsInStageOrder(t *testing.T) {
	var order []string
	rec := func(name string) EgressRewriterOf[struct{}] { return orderingRewriter{name: name, log: &order} }

	svc := &Service{}
	// Registered late-stage first, on purpose.
	RegisterEgressRewriter(svc, rec("second"), 90, attemptView)
	RegisterEgressRewriter(svc, rec("first"), 10, attemptView)

	svc.rewriteEgress(context.Background(), &Exchange{}, protocols.ProtocolOpenAI, []byte(`{}`))

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("ran in %v, want stage order regardless of registration order", order)
	}
}

type orderingRewriter struct {
	name string
	log  *[]string
}

func (o orderingRewriter) Name() string { return o.name }

func (o orderingRewriter) RewriteEgress(_ context.Context, _ struct{}, _ protocols.ProtocolID, body []byte, _ fact.Sink) ([]byte, error) {
	*o.log = append(*o.log, o.name)
	return body, nil
}

// TestDuplicateStagePanicsAtAssembly confirms a stage collision is caught where
// it can still be fixed, rather than resolving silently to whichever rewriter
// was registered first.
func TestDuplicateStagePanicsAtAssembly(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering two rewriters at the same stage must panic")
		}
	}()
	svc := &Service{}
	var log []string
	RegisterEgressRewriter(svc, orderingRewriter{name: "a", log: &log}, 10, attemptView)
	RegisterEgressRewriter(svc, orderingRewriter{name: "b", log: &log}, 10, attemptView)
}

// vandalObserver mutates everything it is handed. A well-behaved observer never
// does this; the point is that the guarantee must not depend on that.
type vandalObserver struct{}

func (vandalObserver) Name() string { return "vandal" }

func (vandalObserver) ObserveUpstreamError(_ context.Context, _ struct{}, up fact.Upstream, _ fact.Sink) {
	if len(up.Body) > 0 {
		up.Body[0] = 'X'
	}
	up.Header.Set("X-Tampered", "yes")
}

// witnessObserver records what it was actually handed.
type witnessObserver struct {
	body   []byte
	header string
}

func (w *witnessObserver) Name() string { return "witness" }

func (w *witnessObserver) ObserveUpstreamError(_ context.Context, _ struct{}, up fact.Upstream, _ fact.Sink) {
	w.body = bytes.Clone(up.Body)
	w.header = up.Header.Get("X-Tampered")
}

// TestObserversCannotReachEachOtherOrTheAuditBody is the isolation guarantee
// the interface states. Without per-observer copies the verdict would depend on
// registration order through a second door — one observer editing the input the
// next one reads — and a stray write would also rewrite the bytes already
// captured as the record of what the upstream said.
func TestObserversCannotReachEachOtherOrTheAuditBody(t *testing.T) {
	svc := &Service{}
	witness := &witnessObserver{}
	RegisterUpstreamErrorObserver(svc, vandalObserver{}, attemptView)
	RegisterUpstreamErrorObserver(svc, witness, attemptView)

	captured := []byte(`{"error":"real"}`)
	header := http.Header{}
	header.Set("Content-Type", "application/json")

	svc.observeUpstreamError(context.Background(), &Exchange{}, fact.Upstream{
		StatusCode: 400,
		Header:     header,
		Body:       captured,
	})

	if witness.body[0] == 'X' {
		t.Error("the second observer saw the first one's edit to the body")
	}
	if witness.header != "" {
		t.Error("the second observer saw the first one's edit to the headers")
	}
	if string(captured) != `{"error":"real"}` {
		t.Errorf("the captured upstream body was rewritten to %q", captured)
	}
	if header.Get("X-Tampered") != "" {
		t.Error("the caller's header map was mutated")
	}
}

// candidateRefusingRewriter refuses the body only when it is bound for the
// first candidate, so a test can watch the chain walk past the refusal.
type candidateRefusingRewriter struct{}

func (candidateRefusingRewriter) Name() string { return "candidate_refuser" }

func (candidateRefusingRewriter) RewriteEgress(_ context.Context, _ struct{}, _ protocols.ProtocolID, body []byte, _ fact.Sink) ([]byte, error) {
	if bytes.Contains(body, []byte(`"model":"c1-model"`)) {
		return nil, errors.New("refusing the body bound for c1")
	}
	return body, nil
}

// TestEgressRefusalFailsOverToNextCandidate is the end-to-end proof for the
// refusal path: the verdict must cost exactly one candidate. The refused
// candidate's upstream must never see the request — a refused body reaching the
// wire is the failure this path exists to prevent — and the chain must then
// serve from the next candidate rather than giving up.
func TestEgressRefusalFailsOverToNextCandidate(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		seenMu     sync.Mutex
		seenModels []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenMu.Lock()
		seenModels = append(seenModels, extractModelFromJSON(t, body))
		seenMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"c2-model","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	RegisterEgressRewriter(svc, candidateRefusingRewriter{}, 10,
		attemptView)
	apiKey := seedTwoCandidateModel(t, svc, db, upstream.URL)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 served by the second candidate; body = %s", w.Code, w.Body.String())
	}
	seenMu.Lock()
	got := append([]string(nil), seenModels...)
	seenMu.Unlock()
	if len(got) != 1 || got[0] != "c2-model" {
		t.Fatalf("upstream saw %v, want only [c2-model]: the refused body must never reach the wire", got)
	}
}

// trackingAdmission records what it was asked to do, so a test can assert the
// kernel's acquire/release discipline rather than the capability's own logic.
type trackingAdmission struct {
	name     string
	log      *[]string
	refuse   bool
	takeHold bool
}

func (a trackingAdmission) Name() string { return a.name }

func (a trackingAdmission) Admit(_ context.Context, _ struct{}, sink fact.Sink) (string, bool) {
	*a.log = append(*a.log, "admit:"+a.name)
	if a.refuse {
		sink.Report(fact.Fact{Kind: fact.KindCallerRateLimited, Detail: a.name + " refused"})
		return a.name, false
	}
	return a.name, a.takeHold
}

func (a trackingAdmission) Release(_ context.Context, _ struct{}, ticket string, _ fact.Outcome, _ fact.Sink) {
	*a.log = append(*a.log, "release:"+ticket)
}

// TestAdmissionsReleaseInReverseOrder pins the stack discipline. Whatever was
// taken last is given back first, which is what makes a later admission safe to
// depend on an earlier one still holding.
func TestAdmissionsReleaseInReverseOrder(t *testing.T) {
	var log []string
	svc := &Service{}
	RegisterAdmission(svc, trackingAdmission{name: "first", log: &log, takeHold: true}, AdmitOnArrival,
		attemptView)
	RegisterAdmission(svc, trackingAdmission{name: "second", log: &log, takeHold: true}, AdmitOnArrival,
		attemptView)

	rc := &Exchange{}
	var held []heldTicket
	verdict := svc.admit(context.Background(), rc, AdmitOnArrival, &held)
	if verdict.Loop != decision.LoopNone {
		t.Fatalf("loop = %v, want LoopNone when nothing refused", verdict.Loop)
	}
	svc.releaseAdmissions(context.Background(), rc, held, fact.Outcome{})

	want := []string{"admit:first", "admit:second", "release:second", "release:first"}
	if len(log) != len(want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("log = %v, want %v", log, want)
		}
	}
}

// TestAdmissionRefusalStopsLaterAdmissions is why the refusal short-circuits:
// an admission running after a refusal would take a resource for a request that
// is already over. For a rate limiter that means charging the caller for a
// request nobody serves.
func TestAdmissionRefusalStopsLaterAdmissions(t *testing.T) {
	var log []string
	svc := &Service{}
	RegisterAdmission(svc, trackingAdmission{name: "refuser", log: &log, refuse: true}, AdmitOnArrival,
		attemptView)
	RegisterAdmission(svc, trackingAdmission{name: "later", log: &log, takeHold: true}, AdmitOnArrival,
		attemptView)

	rc := &Exchange{}
	var held []heldTicket
	verdict := svc.admit(context.Background(), rc, AdmitOnArrival, &held)

	if verdict.Loop != decision.LoopTerminate {
		t.Fatalf("loop = %v, want decision.LoopTerminate", verdict.Loop)
	}
	if verdict.RejectDetail() != "refuser refused" {
		t.Errorf("rejectDetail = %q, want the reporter's own words", verdict.RejectDetail())
	}
	if len(held) != 0 {
		t.Errorf("held = %v, want nothing held when the first admission refused", held)
	}
	for _, e := range log {
		if e == "admit:later" {
			t.Fatal("an admission ran after the request had already been refused")
		}
	}
}

// TestAdmissionReleasesOnlyWhatWasHeld covers the unlimited-key shape: an
// admission that took nothing must not be released, or it gives back a resource
// it never had.
func TestAdmissionReleasesOnlyWhatWasHeld(t *testing.T) {
	var log []string
	svc := &Service{}
	RegisterAdmission(svc, trackingAdmission{name: "nothing", log: &log, takeHold: false}, AdmitOnArrival,
		attemptView)

	rc := &Exchange{}
	var held []heldTicket
	svc.admit(context.Background(), rc, AdmitOnArrival, &held)
	svc.releaseAdmissions(context.Background(), rc, held, fact.Outcome{})

	for _, e := range log {
		if e == "release:nothing" {
			t.Fatal("released an admission that reported holding nothing")
		}
	}
}

// panickingAdmission fails in the one way the release contract has to survive.
type panickingAdmission struct{ log *[]string }

func (panickingAdmission) Name() string { return "panicker" }

func (a panickingAdmission) Admit(context.Context, struct{}, fact.Sink) (string, bool) {
	*a.log = append(*a.log, "admit:panicker")
	panic("admission blew up")
}

func (a panickingAdmission) Release(context.Context, struct{}, string, fact.Outcome, fact.Sink) {
	*a.log = append(*a.log, "release:panicker")
}

// TestAdmissionPanicStillReleasesEarlierTickets is why the release is armed
// before anything is acquired.
//
// A panic in a later admission unwinds past any cleanup that would have been
// installed after the call returned, so an earlier admission's resource — a
// concurrency slot, say — would be held for the life of the process. The
// recovering middleware would report a tidy 500 while the caller's allowance
// quietly shrank by one, which is the kind of leak that only shows up as
// "throughput degrades over days".
func TestAdmissionPanicStillReleasesEarlierTickets(t *testing.T) {
	var log []string
	svc := &Service{}
	RegisterAdmission(svc, trackingAdmission{name: "first", log: &log, takeHold: true}, AdmitOnArrival,
		attemptView)
	RegisterAdmission(svc, panickingAdmission{log: &log}, AdmitOnArrival,
		attemptView)

	rc := &Exchange{}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic should still propagate to the caller")
			}
		}()
		// Exactly the shape Handle uses: arm the release, then acquire.
		//
		// The closure is load-bearing. Deferring the call directly would
		// evaluate held at defer time — nil, before anything was acquired — and
		// release nothing, which is the same leak this test exists to catch.
		var held []heldTicket
		defer func() {
			svc.releaseAdmissions(context.Background(), rc, held, fact.Outcome{})
		}()
		svc.admit(context.Background(), rc, AdmitOnArrival, &held)
	}()

	var released bool
	for _, e := range log {
		if e == "release:first" {
			released = true
		}
	}
	if !released {
		t.Fatal("the first admission's ticket was never released after a later panic")
	}
}

// TestFailReasonPrefersTheReporterCode pins the boundary between an internal
// name and a persisted one.
//
// Kind names are internal and get renamed as the vocabulary is refined. The
// failure reason is read by dashboards and log viewers written against specific
// strings, so deriving one from the other means a rename silently breaks a
// display nobody is watching.
func TestFailReasonPrefersTheReporterCode(t *testing.T) {
	withCode := decision.ResolveBatch([]fact.Fact{
		{Kind: fact.KindCallerRateLimited, Reason: "concurrency_limit"},
	})
	if got := withCode.FailReason(); got != "concurrency_limit" {
		t.Errorf("failReason = %q, want the reporter's stable code", got)
	}

	withoutCode := decision.ResolveBatch([]fact.Fact{{Kind: fact.KindCallerRateLimited}})
	if got := withoutCode.FailReason(); got != "caller_rate_limited" {
		t.Errorf("failReason = %q, want the kind name as fallback", got)
	}
}

// mutatingRecorder is the rule-breaker: it does everything a recorder must not
// do to the history it is handed.
type mutatingRecorder struct{}

func (mutatingRecorder) Name() string { return "mutating" }

func (mutatingRecorder) Record(_ context.Context, _ struct{}, _ fact.Outcome, tl fact.Timeline) {
	// Grow it, edit an entry in place, and edit the Fact that entry points at.
	tl.Append(fact.Entry{Reporter: "forged", Fact: &fact.Fact{Kind: fact.KindClientGone}})
	got := tl.All()
	for i := range got {
		got[i].Reporter = "overwritten"
		if got[i].Fact != nil {
			got[i].Fact.Reason = "overwritten"
		}
	}
}

// readingRecorder runs after the rule-breaker and keeps what it was given, so
// the assertions can ask what actually survived.
type readingRecorder struct {
	saw []fact.Entry
}

func (r *readingRecorder) Name() string { return "reading" }

func (r *readingRecorder) Record(_ context.Context, _ struct{}, _ fact.Outcome, tl fact.Timeline) {
	r.saw = tl.All()
}

// TestRecordersCannotEditTheHistoryTheyRead checks that the timeline a recorder
// receives is its own to read and nobody else's to lose.
//
// Recorders run in sequence over one history. If the first could grow or edit
// it, the audit trail would depend on the order recorders happen to be
// registered in — the same order dependence the fold exists to rule out,
// arriving through a later door. Handing the timeline over by value is what
// prevents it, so this test fails if that is ever relaxed back to a pointer.
func TestRecordersCannotEditTheHistoryTheyRead(t *testing.T) {
	rc := &Exchange{requestID: "req-timeline-isolation"}
	rc.timeline.Append(fact.Entry{
		Attempt:  0,
		Reporter: "original",
		Fact:     &fact.Fact{Kind: fact.KindPayloadRefused, Reason: "original"},
	})

	svc := &Service{}
	reader := &readingRecorder{}
	bind := func(*Exchange) struct{} { return struct{}{} }
	RegisterRecorder(svc, mutatingRecorder{}, bind)
	RegisterRecorder(svc, reader, bind)

	svc.runRecorders(context.Background(), rc, fact.Outcome{})

	if len(reader.saw) != 1 {
		t.Fatalf("the second recorder saw %d entries, want 1: the first one's append reached it",
			len(reader.saw))
	}
	if reader.saw[0].Reporter != "original" {
		t.Errorf("the second recorder read Reporter %q, want %q: the first one's edit reached it",
			reader.saw[0].Reporter, "original")
	}
	if reader.saw[0].Fact.Reason != "original" {
		t.Errorf("the second recorder read Reason %q, want %q: entries are copied but the Fact they point at is not",
			reader.saw[0].Fact.Reason, "original")
	}

	// The kernel's own copy is the one anything after this reads.
	kept := rc.timeline.All()
	if len(kept) != 1 || kept[0].Reporter != "original" || kept[0].Fact.Reason != "original" {
		t.Errorf("the kernel's timeline was modified by a recorder: %+v", kept)
	}
}

// TestImpossibleDeliveryIsRefusedAndSettled ties the delivery rules to a
// consequence.
//
// fact's own tests prove Validate REPORTS an impossible delivery. This one
// proves the kernel ACTS on it: without the substitution the rules would be a
// description with no consequence attached.
//
// The delivery here claims the caller both received nothing and saw a status,
// and asks to continue the chain. Continuing would be the worst outcome: the
// request would run on against values that describe nothing that happened.
func TestImpossibleDeliveryIsRefusedAndSettled(t *testing.T) {
	svc := &Service{}
	rc := &Exchange{requestID: "req-impossible"}

	// Uncommitted, yet claiming the caller saw a status. BillingStatus is set
	// so this is rejected by the rule it means to exercise rather than by the
	// earlier "billing status is unset" one.
	impossible := fact.Delivery{
		Verdict:       fact.VerdictNextCandidate,
		BillingStatus: 502,
		ClientStatus:  200,
	}
	if impossible.Validate() == nil {
		t.Fatal("the fixture is no longer impossible; pick another shape")
	}

	got := settleOneDelivery(t, svc, rc, impossible)

	if got != attemptSuccess {
		t.Errorf("result = %v, want the request settled: an impossible delivery must not send the chain on", got)
	}
	if !rc.logWritten.Load() {
		t.Error("no row was written: the request was neither settled nor continued")
	}
	if rc.statusCode != http.StatusInternalServerError {
		t.Errorf("settled status = %d, want 500: we cannot blame a provider for a delivery we could not read", rc.statusCode)
	}

	var noted *fact.DeliveryObserved
	for _, e := range rc.timeline.All() {
		if v, ok := e.Record.(fact.DeliveryObserved); ok {
			noted = &v
		}
	}
	if noted == nil {
		t.Fatal("the refused delivery was never recorded")
	}
	if noted.BillingStatus != http.StatusInternalServerError || noted.Fault != "gateway" {
		t.Errorf("recorded %+v, want the substitution recorded, not the impossible original", *noted)
	}

	// The attempt row is built from whatever the labelling step is handed, and
	// only the substitute carries a reason. Check the delivery after labelling
	// instead of before and this comes out empty: the row an operator opens to
	// find out what happened would say nothing at all.
	if len(rc.attempts) != 1 {
		t.Fatalf("attempts = %d, want exactly one", len(rc.attempts))
	}
	if !strings.HasPrefix(rc.attempts[0].FailReason, "invalid_delivery") {
		t.Errorf("attempt fail reason = %q, want it to name the refused delivery — "+
			"an empty reason here means the row was labelled before the delivery was checked",
			rc.attempts[0].FailReason)
	}
}

// settleOneDelivery puts a delivery through the kernel's own recording step,
// which is where a delivery is checked, labelled, and settled in that order.
func settleOneDelivery(t *testing.T, svc *Service, rc *Exchange, d fact.Delivery) attemptResult {
	t.Helper()
	adm := admitFor(t, protocols.ProtocolOpenAI, "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
		Candidate{ProviderModelName: "m", EgressProtocol: protocols.ProtocolOpenAI, Passthrough: true})
	cand := model.ModelCandidate{}
	rc.attempt.BeginCandidate(&cand)
	rc.attempt.BindProvider(&model.Provider{})
	rc.attempt.BindKey(&model.ProviderKey{})
	return svc.recordAndSettle(nil, rc, adm, d, 200, time.Now())
}

// TestDeliveryIsRecordedAgainstTheAttemptItDescribes pins provenance for the
// delivery record.
//
// The sink numbers an entry by how many attempts have been recorded so far,
// which is correct for anything reporting DURING an attempt — the record for
// that attempt has not been appended yet. A delivery is checked in exactly that
// window, before its own attempt is labelled, and the append that follows is
// what makes the number it was given the right one. Move the check after the
// append and every delivery is filed under the candidate that comes NEXT, with
// the last one under a candidate that never ran.
func TestDeliveryIsRecordedAgainstTheAttemptItDescribes(t *testing.T) {
	svc := &Service{}
	rc := &Exchange{requestID: "req-provenance"}
	// Two attempts already ran; the one being settled here is the third.
	rc.attempts = []AttemptRecord{{ProviderName: "first"}, {ProviderName: "second"}}

	settleOneDelivery(t, svc, rc, fact.Succeeded(200))

	if len(rc.attempts) != 3 {
		t.Fatalf("attempts = %d, want the delivery to have appended its own", len(rc.attempts))
	}

	var noted *fact.Entry
	for i, e := range rc.timeline.All() {
		if _, ok := e.Record.(fact.DeliveryObserved); ok {
			noted = &rc.timeline.All()[i]
		}
	}
	if noted == nil {
		t.Fatal("the delivery was never recorded")
	}
	if noted.Attempt != 2 {
		t.Errorf("delivery recorded against attempt %d, want 2: it describes the attempt it just settled, not the next one",
			noted.Attempt)
	}
}

// TestASettlementAfterItsOwnAttemptIsRecordedAgainstThatAttempt is the same
// provenance rule for the endings that no dispatch produces: an upstream 4xx
// the caller has to own, and the disconnects noticed inside a candidate. Each
// records the attempt it died on and then settles, so each meets the same
// off-by-one — and the default numbering is what a settling helper reaches for
// unless its call site says otherwise.
//
// Driven through a terminal 400 because that ending is reachable without racing
// a live connection shutdown; the disconnect paths settle through the same two
// helpers.
func TestASettlementAfterItsOwnAttemptIsRecordedAgainstThatAttempt(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	// 400 is terminal: the caller's own request is what the upstream objected
	// to, so no other candidate would do better and the chain ends here.
	refuses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`))
	}))
	defer refuses.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "refuses", refuses.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)

	now := time.Now().UTC()
	m := &model.Model{Name: "gpt-4o", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if err := db.Create(&model.ModelCandidate{
		ModelID: m.ID, ProviderID: p.ID, ProviderModelName: "claude-3-5-sonnet-20241022",
		InputPrice: 1.0, OutputPrice: 2.0, MaxOutput: 4096,
		SupportsStreaming: boolPtr(true), SupportsFunctionCalling: boolPtr(true),
		ManagementStatus:   model.ModelCandidateStatusEnabled,
		SortOrder:          1,
		VerificationStatus: model.ModelVerificationStatusPassed,
		CreatedAt:          now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	c, _ := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	var noted []fact.Entry
	for _, e := range captured.timeline.All() {
		if _, ok := e.Record.(fact.DeliveryObserved); ok {
			noted = append(noted, e)
		}
	}
	if len(noted) != 1 {
		t.Fatalf("the settlement was recorded %d times, want once", len(noted))
	}
	if len(captured.attempts) != 1 {
		t.Fatalf("the request made %d attempts, want 1: the test no longer sets up what it claims to",
			len(captured.attempts))
	}
	if want := len(captured.attempts) - 1; noted[0].Attempt != want {
		t.Errorf("settlement recorded against attempt %d, want %d: it describes the attempt that was just appended, not one that never runs",
			noted[0].Attempt, want)
	}
}

// clockProbeAdmission samples, at the moment its release runs, the liveness
// and identity of the context it was handed. Sampling inside the release
// matters: the kernel cancels each ticket's context right after its release
// returns, and a cancelled context's Done() collapses to a shared sentinel —
// judged after the fact, three separate clocks are indistinguishable from one.
type releaseClockSample struct {
	err      error
	deadline bool
	done     <-chan struct{}
}

type clockProbeAdmission struct {
	name string
	got  *[]releaseClockSample
}

func (a clockProbeAdmission) Name() string { return a.name }

func (a clockProbeAdmission) Admit(_ context.Context, _ struct{}, _ fact.Sink) (string, bool) {
	return a.name, true
}

func (a clockProbeAdmission) Release(ctx context.Context, _ struct{}, _ string, _ fact.Outcome, _ fact.Sink) {
	_, hasDeadline := ctx.Deadline()
	*a.got = append(*a.got, releaseClockSample{err: ctx.Err(), deadline: hasDeadline, done: ctx.Done()})
}

// TestEachReleaseGetsItsOwnClock pins the per-ticket budget: releases run in a
// chain, and under a SHARED deadline the first release exhausting it would
// hand every release after it a context that is already dead — a database
// refund timing out once would strand the reservations every later admission
// still holds. Each release must therefore arrive with its own live, bounded
// context, not a sibling's leftovers.
func TestEachReleaseGetsItsOwnClock(t *testing.T) {
	var got []releaseClockSample
	svc := &Service{}
	for _, name := range []string{"first", "second", "third"} {
		RegisterAdmission(svc, clockProbeAdmission{name: name, got: &got}, AdmitOnArrival, attemptView)
	}

	rc := &Exchange{}
	var held []heldTicket
	if v := svc.admit(context.Background(), rc, AdmitOnArrival, &held); v.Loop != decision.LoopNone {
		t.Fatalf("loop = %v, want LoopNone", v.Loop)
	}
	svc.releaseAdmissions(context.Background(), rc, held, fact.Outcome{})

	if len(got) != 3 {
		t.Fatalf("releases observed = %d, want 3", len(got))
	}
	for i, sample := range got {
		if sample.err != nil {
			t.Errorf("release %d arrived with a context already done (%v): an earlier "+
				"release's exhaustion leaked into this one", i, sample.err)
		}
		if !sample.deadline {
			t.Errorf("release %d has no deadline: a stuck backend can pin the goroutine", i)
		}
		for j := range got[:i] {
			if got[i].done == got[j].done {
				t.Errorf("releases %d and %d share one context: whichever exhausts the "+
					"budget first kills the other's chance to give its resource back", j, i)
			}
		}
	}
}
