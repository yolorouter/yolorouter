package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// captureAdmission holds nothing and records the Outcome its Release was
// handed, so a test can assert what the settlement seam actually carries.
type captureAdmission struct{ got *fact.Outcome }

func (captureAdmission) Name() string { return "outcome_capture" }

func (captureAdmission) Admit(context.Context, struct{}, fact.Sink) (struct{}, bool) {
	return struct{}{}, true
}

func (a captureAdmission) Release(_ context.Context, _ struct{}, _ struct{}, o fact.Outcome, _ fact.Sink) {
	*a.got = o
}

// usageMutatingAdmission writes through the usage pointer it is shown, the way
// a buggy or hostile settlement backend could.
type usageMutatingAdmission struct{}

func (usageMutatingAdmission) Name() string { return "usage_mutating" }

func (usageMutatingAdmission) Admit(context.Context, struct{}, fact.Sink) (struct{}, bool) {
	return struct{}{}, true
}

func (usageMutatingAdmission) Release(_ context.Context, _ struct{}, _ struct{}, o fact.Outcome, _ fact.Sink) {
	if o.Usage != nil {
		*o.Usage = fact.UsageReported{Total: 999}
	}
}

// outcomeReadingRecorder keeps the outcome it was handed so a test can check
// what a later recorder actually sees.
type outcomeReadingRecorder struct{ saw fact.Outcome }

func (r *outcomeReadingRecorder) Name() string { return "outcome_reading" }

func (r *outcomeReadingRecorder) Record(_ context.Context, _ struct{}, o fact.Outcome, _ fact.Timeline) {
	r.saw = o
}

// usageMutatingRecorder is the recorder-side twin of usageMutatingAdmission.
type usageMutatingRecorder struct{}

func (usageMutatingRecorder) Name() string { return "usage_mutating" }

func (usageMutatingRecorder) Record(_ context.Context, _ struct{}, o fact.Outcome, _ fact.Timeline) {
	if o.Usage != nil {
		*o.Usage = fact.UsageReported{Total: 999}
	}
}

// Releases run in a chain over one outcome. If the first could write through
// the usage pointer, every release after it would settle from forged counts,
// and the kernel's own record — the one the audit row persists — would be
// rewritten too. Each release must get its own copy.
func TestReleasesCannotEditTheUsageAnotherSettlesFrom(t *testing.T) {
	rc := &Exchange{requestID: "req-outcome-isolation"}
	usage := &fact.UsageReported{Prompt: 3, Completion: 2, Total: 5}
	out := fact.Outcome{Usage: usage, CostMicros: 7, CostKnown: true}

	var got fact.Outcome
	bind := func(*Exchange) struct{} { return struct{}{} }
	// Releases run in reverse acquisition order, so the mutator — last in the
	// slice — runs before the capturer.
	held := []heldTicket{
		{by: admissionAdapter[struct{}, struct{}]{inner: captureAdmission{got: &got}, bind: bind, registeredName: "outcome_capture"}, ticket: struct{}{}},
		{by: admissionAdapter[struct{}, struct{}]{inner: usageMutatingAdmission{}, bind: bind, registeredName: "usage_mutating"}, ticket: struct{}{}},
	}

	(&Service{}).releaseAdmissions(context.Background(), rc, held, out)

	if got.Usage == nil {
		t.Fatal("the second release saw no usage at all")
	}
	if got.Usage.Total != 5 || got.Usage.Prompt != 3 || got.Usage.Completion != 2 {
		t.Errorf("the second release saw usage %+v, want 3/2/5: the first one's edit reached it", got.Usage)
	}
	if usage.Total != 5 || usage.Prompt != 3 || usage.Completion != 2 {
		t.Errorf("the kernel's settled record was modified by a release: %+v", usage)
	}
}

// Same guarantee on the recorder side: an audit row must describe what was
// settled, not what the recorder before it chose to write.
func TestRecordersCannotEditTheUsageTheRowCarries(t *testing.T) {
	rc := &Exchange{requestID: "req-outcome-isolation"}
	usage := &fact.UsageReported{Prompt: 3, Completion: 2, Total: 5}
	out := fact.Outcome{Usage: usage, CostMicros: 7, CostKnown: true}

	svc := &Service{}
	reader := &outcomeReadingRecorder{}
	bind := func(*Exchange) struct{} { return struct{}{} }
	RegisterRecorder(svc, usageMutatingRecorder{}, bind)
	RegisterRecorder(svc, reader, bind)

	svc.runRecorders(context.Background(), rc, out)

	if reader.saw.Usage == nil {
		t.Fatal("the second recorder saw no usage at all")
	}
	if reader.saw.Usage.Total != 5 || reader.saw.Usage.Prompt != 3 || reader.saw.Usage.Completion != 2 {
		t.Errorf("the second recorder saw usage %+v, want 3/2/5: the first one's edit reached it", reader.saw.Usage)
	}
	if usage.Total != 5 || usage.Prompt != 3 || usage.Completion != 2 {
		t.Errorf("the kernel's settled record was modified by a recorder: %+v", usage)
	}
}

// A served exchange releases with the settled usage and price on the outcome:
// this is the half a Release settles from, and it must match the audit row —
// the same counts, priced, with "priceable" stated explicitly.
func TestReleaseSeesTheSettledUsageAndCost(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	var got fact.Outcome
	RegisterAdmission(svc, captureAdmission{got: &got}, AdmitOnArrival, func(*Exchange) struct{} { return struct{}{} })
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got.Usage == nil {
		t.Fatal("release saw no usage on a served exchange — nothing to settle from")
	}
	if got.Usage.Total != 5 || got.Usage.Prompt != 3 || got.Usage.Completion != 2 {
		t.Fatalf("release usage = %+v, want the upstream's 3/2/5", got.Usage)
	}
	if !got.CostKnown {
		t.Fatal("a priced exchange must state CostKnown; unknown means unpriceable")
	}
	// The fixture prices the candidate at 1.0/2.0 per million tokens:
	// 3 prompt + 2*2 completion = 7 micros.
	if got.CostMicros != 7 {
		t.Fatalf("CostMicros = %d, want 7 at the fixture's 1.0/2.0 prices", got.CostMicros)
	}
}

// A rejected exchange releases with no usage: absent usage is exactly the
// signal a Release reverses its reservation on.
func TestReleaseSeesNoUsageOnARejectedExchange(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	var got fact.Outcome
	RegisterAdmission(svc, captureAdmission{got: &got}, AdmitOnArrival, func(*Exchange) struct{} { return struct{}{} })
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if got.Usage != nil {
		t.Fatalf("release saw usage %+v on a failed exchange — a reversal would be booked as a charge", got.Usage)
	}
	if got.CostKnown {
		t.Fatal("an unserved exchange cannot have a known cost")
	}
}
