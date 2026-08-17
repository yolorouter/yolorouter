package gateway

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/fact"
)

// These tests pin the end-of-exchange choreography DIRECTLY: concludeExchange
// is called with probe capabilities instead of driving a whole HTTP request,
// so each ordering rule fails on its own when the step order is disturbed,
// instead of surfacing as a distant symptom in a full-pipeline test.

// concludeProbe collects what the probe capabilities observe while an
// exchange concludes: the order the steps ran in, and what the release saw.
type concludeProbe struct {
	events []string
	// releaseSawSettled is the exchange's outcomeSettled flag AT RELEASE TIME
	// (captured via the admission's bind, which the adapter evaluates when
	// the release runs) — true proves settlement happened first.
	releaseSawSettled bool
	// releaseSawStatus is the settled status the release was handed.
	releaseSawStatus int
}

type probeAdmission struct{ p *concludeProbe }

func (a probeAdmission) Name() string { return "conclude_probe" }

func (a probeAdmission) Admit(_ context.Context, _ bool, _ fact.Sink) (struct{}, bool) {
	return struct{}{}, true
}

func (a probeAdmission) Release(_ context.Context, settled bool, _ struct{}, out fact.Outcome, _ fact.Sink) {
	a.p.events = append(a.p.events, "release")
	a.p.releaseSawSettled = settled
	a.p.releaseSawStatus = out.StatusCode
}

type probeRecorder struct{ p *concludeProbe }

func (r probeRecorder) Name() string { return "conclude_probe_recorder" }

func (r probeRecorder) Record(_ context.Context, _ struct{}, _ fact.Outcome, _ fact.Timeline) {
	r.p.events = append(r.p.events, "record")
}

// concludeFixture wires a bare service with one probe admission ticket and
// one probe recorder around a fresh exchange.
func concludeFixture(t *testing.T) (*Service, *Exchange, []heldTicket, *concludeProbe) {
	t.Helper()
	p := &concludeProbe{}
	svc := &Service{}
	RegisterRecorder(svc, probeRecorder{p}, func(*Exchange) struct{} { return struct{}{} })
	rc := &Exchange{requestID: "req-conclude", requestCtx: context.Background()}
	held := []heldTicket{{
		by: admissionAdapter[bool, struct{}]{
			inner:          probeAdmission{p},
			bind:           func(rc *Exchange) bool { return rc.outcomeSettled },
			registeredName: "conclude_probe",
		},
		ticket: struct{}{},
	}}
	testHookHandleDone = func(*Exchange) { p.events = append(p.events, "hook") }
	t.Cleanup(func() { testHookHandleDone = nil })
	return svc, rc, held, p
}

// TestConcludeOrdersSettleReleaseRecordHook is the choreography contract in
// one assertion: an unsettled exchange is settled FIRST (the release reads a
// real status, not a zero), admissions release next, the recorder pass runs
// after them (so it sees the reversal facts they append), and the done hook
// fires only when everything else has.
func TestConcludeOrdersSettleReleaseRecordHook(t *testing.T) {
	svc, rc, held, p := concludeFixture(t)

	svc.concludeExchange(nil, rc, held, time.Now())

	if want := []string{"release", "record", "hook"}; !slices.Equal(p.events, want) {
		t.Fatalf("conclude step order: want %v, got %v", want, p.events)
	}
	if !p.releaseSawSettled {
		t.Error("the release ran against an unsettled exchange: settlement must come first")
	}
	if p.releaseSawStatus != 500 {
		t.Errorf("the release saw status %d, want the settled 500", p.releaseSawStatus)
	}
	// Nothing had been written to a caller, so the safety-net settlement must
	// record an uncommitted delivery.
	obs := observedDelivery(t, rc)
	if obs.Committed {
		t.Error("safety-net settlement claimed bytes were committed on a request that wrote nothing")
	}
	if obs.FailReason != "panic_recovered" {
		t.Errorf("safety-net settlement reason: want panic_recovered, got %q", obs.FailReason)
	}
}

// TestConcludeDoesNotResettleANormalExit: an exchange the handler already
// settled concludes without a second settlement — the release and recorder
// see the handler's own ending, not a synthetic 500 layered over it.
func TestConcludeDoesNotResettleANormalExit(t *testing.T) {
	svc, rc, held, p := concludeFixture(t)
	svc.settle(rc, fact.Rejected(400, fact.FaultClient, "bad_request", nil), time.Now(), settleOptions{})

	svc.concludeExchange(nil, rc, held, time.Now())

	if p.releaseSawStatus != 400 {
		t.Errorf("the release saw status %d, want the handler's own 400", p.releaseSawStatus)
	}
	if rc.outcome.FailReason != "bad_request" {
		t.Errorf("conclude replaced the settled reason with %q", rc.outcome.FailReason)
	}
	if want := []string{"release", "record", "hook"}; !slices.Equal(p.events, want) {
		t.Fatalf("conclude step order: want %v, got %v", want, p.events)
	}
}

// TestConcludeRecordsTruncationWhenBytesWentOut: when the caller already saw
// a status before the exchange died unsettled, the safety net must not claim
// nothing was committed — the row says truncated-after-200, the only place
// that ending is visible at all.
func TestConcludeRecordsTruncationWhenBytesWentOut(t *testing.T) {
	svc, rc, held, _ := concludeFixture(t)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.String(200, "partial body")

	svc.concludeExchange(c, rc, held, time.Now())

	obs := observedDelivery(t, rc)
	if !obs.Committed {
		t.Error("bytes went out but the settlement claims nothing was committed")
	}
	if obs.ClientStatus != 200 || obs.BillingStatus != 500 {
		t.Errorf("truncated settlement statuses: client %d billing %d, want 200/500",
			obs.ClientStatus, obs.BillingStatus)
	}
}

// observedDelivery digs the settlement's delivery record out of the timeline.
func observedDelivery(t *testing.T, rc *Exchange) fact.DeliveryObserved {
	t.Helper()
	for _, e := range rc.timeline.All() {
		if obs, ok := e.Record.(fact.DeliveryObserved); ok {
			return obs
		}
	}
	t.Fatal("no DeliveryObserved record on the timeline: nothing settled the exchange")
	return fact.DeliveryObserved{}
}
