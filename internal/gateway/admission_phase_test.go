package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// pricedAdmission records what the exchange looked like at the moment it was
// asked, which is the only way to tell the two phases apart from the inside.
type pricedAdmission struct {
	name string
	// sawBody appends the body the exchange was carrying on EVERY call, not just
	// the last: an admission asked twice — once before the body was final —
	// would otherwise be hidden by the later observation, and asked twice is
	// reserved twice.
	sawBody *[][]byte
	// log, when set, records admit/release in call order, shared across
	// admissions so one slice shows the whole sequence.
	log    *[]string
	refuse bool
}

func (a pricedAdmission) Name() string { return a.name }

func (a pricedAdmission) Admit(_ context.Context, view *Exchange, sink fact.Sink) (string, bool) {
	if a.sawBody != nil {
		// Read the way a capability has to today: there is no one accessor for
		// "the body after ingress", because nothing in production needs one yet.
		body := view.CompressedRequestBody()
		if body == nil {
			body = view.RequestBody()
		}
		*a.sawBody = append(*a.sawBody, body)
	}
	if a.log != nil {
		*a.log = append(*a.log, "admit:"+a.name)
	}
	if a.refuse {
		sink.Report(fact.Fact{Kind: fact.KindCallerRateLimited, Detail: a.name + " refused"})
		return a.name, false
	}
	return a.name, true
}

func (a pricedAdmission) Release(_ context.Context, _ *Exchange, ticket string, _ fact.Outcome, _ fact.Sink) {
	if a.log != nil {
		*a.log = append(*a.log, "release:"+ticket)
	}
}

func exchangeView(e *Exchange) *Exchange { return e }

// The second phase must see the FINAL body — the one that will actually be
// sent, after every rewriter has had it. An amount computed from the caller's
// original bytes over-reserves on every request a rewriter shrank.
//
// A rewriter that visibly changes the body is what makes this test bite: with
// no rewriter registered the two bodies are identical, and the assertion would
// hold just as well if the phase ran the instant the body was read.
func TestTheSecondPhaseSeesTheRewrittenBodyNotTheOriginal(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var sent []byte
	upstream := upstreamEchoingBody(t, &sent)
	svc := newSvc(t, db)
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)

	RegisterIngressRewriter(svc, scriptedIngress{name: "shouter", on: func(body []byte) ([]byte, error) {
		return bytes.Replace(body, []byte(`"hi"`), []byte(`"REWRITTEN"`), 1), nil
	}}, 90, noView)

	var seen [][]byte
	RegisterAdmission(svc, pricedAdmission{name: "late", sawBody: &seen}, AdmitWhenPriced, exchangeView)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(sent, []byte("REWRITTEN")) {
		t.Fatalf("the rewrite never reached the wire, so this proves nothing: %s", sent)
	}
	if len(seen) != 1 {
		t.Fatalf("the priced phase was asked %d times, want exactly once: an extra call before the body was final is a reservation made from the wrong bytes", len(seen))
	}
	if !bytes.Contains(seen[0], []byte("REWRITTEN")) {
		t.Fatalf("the priced phase saw %s, want the rewritten body: an amount computed from this would not match what was sent", seen[0])
	}
}

// It has to run before anything is dialled, or a refusal arrives after the
// money it was meant to protect has already been spent upstream.
func TestASecondPhaseRefusalReachesNoUpstream(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	dialed := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dialed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)
	RegisterAdmission(svc, pricedAdmission{name: "broke", refuse: true}, AdmitWhenPriced, exchangeView)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if dialed {
		t.Error("an upstream was contacted after the priced phase refused the request")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the table's status for the reported refusal", w.Code)
	}
}

// Release is newest-first across the whole stack, not within each phase. A
// second-phase reservation exists only because the first phase let the request
// through, so it has to be given back before what made it possible.
func TestReleaseRunsNewestFirstAcrossBothPhases(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := upstreamEchoingBody(t, new([]byte))
	svc := newSvc(t, db)
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)

	var log []string
	// Registered priced-first on purpose: the phase decides when each runs, not
	// the order the assembly happened to list them in.
	RegisterAdmission(svc, pricedAdmission{name: "money", log: &log}, AdmitWhenPriced, exchangeView)
	RegisterAdmission(svc, pricedAdmission{name: "gate", log: &log}, AdmitOnArrival, exchangeView)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	want := "admit:gate,admit:money,release:money,release:gate"
	if got := strings.Join(log, ","); got != want {
		t.Fatalf("sequence = %s\nwant     = %s", got, want)
	}
}

// A refusal in the second phase must still give back what the first phase took.
// This is the path a caller with no balance takes on every request, so a leak
// here drains an allowance nobody is using.
func TestASecondPhaseRefusalStillReleasesTheFirstPhase(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := upstreamEchoingBody(t, new([]byte))
	svc := newSvc(t, db)
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)

	var log []string
	RegisterAdmission(svc, pricedAdmission{name: "gate", log: &log}, AdmitOnArrival, exchangeView)
	RegisterAdmission(svc, pricedAdmission{name: "broke", log: &log, refuse: true}, AdmitWhenPriced, exchangeView)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code == http.StatusOK {
		t.Fatalf("the refusal did not end the request: status = %d", w.Code)
	}
	if got := strings.Join(log, ","); got != "admit:gate,admit:broke,release:gate" {
		t.Fatalf("sequence = %s, want the first phase released after the second refused", got)
	}
}

// Acquisition order is phase order, whatever order the assembly registered in,
// and the roster the assembly test pins must say so — it is the only place the
// real order is visible to a test in another package.
func TestRegisteredAdmissionsReadsInAcquisitionOrder(t *testing.T) {
	svc := &Service{}
	RegisterAdmission(svc, pricedAdmission{name: "money"}, AdmitWhenPriced, exchangeView)
	RegisterAdmission(svc, pricedAdmission{name: "gate"}, AdmitOnArrival, exchangeView)
	RegisterAdmission(svc, pricedAdmission{name: "second-gate"}, AdmitOnArrival, exchangeView)

	got := strings.Join(svc.RegisteredAdmissions(), ",")
	if got != "gate,second-gate,money" {
		t.Fatalf("roster = %s, want the arrival phase first and registration order kept within it", got)
	}
}

// A model with no candidate at all ends the request before prices exist. An
// admission registered for the priced phase must simply never be asked, rather
// than be asked with a zero basis it would happily reserve nothing against.
func TestTheSecondPhaseIsNotAskedWhenNothingIsRoutable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	now := time.Now().UTC()
	m := &model.Model{Name: "gpt-4o", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var log []string
	RegisterAdmission(svc, pricedAdmission{name: "money", log: &log}, AdmitWhenPriced, exchangeView)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code == http.StatusOK {
		t.Fatalf("expected the request to end with no routable candidate, got 200")
	}
	if len(log) != 0 {
		t.Fatalf("the priced phase ran anyway: %v — it would have reserved against the zero-value basis", log)
	}
}

// A phase nothing asks is a gate that permits everything, so registering at one
// has to fail at startup rather than at the first request nobody refused.
func TestRegisteringAtAPhaseNothingAsksPanics(t *testing.T) {
	svc := &Service{}
	defer func() {
		if recover() == nil {
			t.Fatal("an admission registered at an unknown phase was accepted; it would never gate anything")
		}
	}()
	// Zero is the value a struct literal or an uninitialised variable supplies,
	// which is how this reaches production without anyone typing it.
	RegisterAdmission(svc, pricedAdmission{name: "money"}, AdmissionPhase(0), exchangeView)
}

// The list registration validates against and the phases the exchange actually
// asks are two different things, and only one of them is checkable at
// registration. This closes the gap: every supported phase must really be
// reached during one served exchange, so adding a constant without wiring it in
// turns red here instead of failing open in production.
func TestEverySupportedPhaseIsActuallyAsked(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := upstreamEchoingBody(t, new([]byte))
	svc := newSvc(t, db)
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)

	asked := map[AdmissionPhase]*[]string{}
	for _, phase := range admissionPhases {
		log := &[]string{}
		asked[phase] = log
		RegisterAdmission(svc, pricedAdmission{name: fmt.Sprintf("phase-%d", phase), log: log}, phase, exchangeView)
	}

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	for _, phase := range admissionPhases {
		if len(*asked[phase]) == 0 {
			t.Errorf("phase %d is registrable but nothing asks it: an admission there would permit every request", phase)
		}
	}
}

// What the priced phase sees is NOT what goes on the wire, and this pins the
// gap rather than papering over it: the configured system prompt is appended
// per candidate, after this phase has already been asked. An admission that
// reserved money from the body it was shown would under-reserve by exactly the
// text added afterwards.
//
// If a later change makes the two equal, this test fails and whoever made them
// equal gets to delete it — which is the point. Silently closing the gap is
// fine; silently believing it was never there is not.
func TestTheBodyThePricedPhaseSeesIsNotYetTheOutboundBody(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var wire []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	defer upstream.Close()

	const injected = "BE CONCISE AND HELPFUL"
	svc := newSvcWithSettings(t, db, stubSettingsProvider{
		val: settings.CustomSystemPromptSetting{Enabled: true, Text: injected},
	})
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)

	var seen [][]byte
	RegisterAdmission(svc, pricedAdmission{name: "late", sawBody: &seen}, AdmitWhenPriced, exchangeView)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if len(seen) != 1 {
		t.Fatalf("the priced phase was asked %d times, want once", len(seen))
	}
	if !bytes.Contains(wire, []byte(injected)) {
		t.Fatalf("the system prompt never reached the wire, so this proves nothing: %s", wire)
	}
	if bytes.Contains(seen[0], []byte(injected)) {
		t.Fatal("the priced phase saw the injected prompt: the accessor's own note says it does not, and a comment that stopped being true is worse than no comment")
	}
}

// The rates the pre-dispatch estimate is taken against come from the FIRST
// routable candidate, and the chain can end on a different one at a different
// price — which is precisely why no accessor hands that basis to capabilities.
// Pinned here so the divergence is a measured fact rather than an argument.
func TestThePricingBasisIsNotNecessarilyTheCandidateThatServes(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer dead.Close()
	alive := upstreamEchoingBody(t, new([]byte))

	svc := newSvc(t, db)
	first := createProvider(t, db, "cheap", dead.URL)
	createProviderKey(t, db, svc.secrets, first.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, first, "gpt-4o", "gpt-4o-real", true, true, 1)

	second := createProvider(t, db, "dear", alive.URL)
	createProviderKey(t, db, svc.secrets, second.ID, "sk-2", "k2", 1, true)
	addCandidateAtPrice(t, db, m, second, "gpt-4o-real", 2, 50.0, 100.0)

	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the second candidate to have served; body = %s", w.Code, w.Body.String())
	}
	served := captured.attempt.Candidate()
	if served == nil {
		t.Fatal("no candidate was recorded as serving")
	}
	if served.InputPrice == captured.pricingBasis.InputPricePerMillion {
		t.Fatalf("the request was priced at %v and served by a candidate at %v: this test exists because those differ, so a fixture that makes them equal proves nothing",
			captured.pricingBasis.InputPricePerMillion, served.InputPrice)
	}
}

// addCandidateAtPrice adds a second candidate to an existing model at rates the
// caller chooses, which is what lets a test tell "priced against" apart from
// "served by".
func addCandidateAtPrice(t *testing.T, db *gorm.DB, m *model.Model, provider *model.Provider, providerModelName string, order int, in, out float64) {
	t.Helper()
	now := time.Now().UTC()
	cand := &model.ModelCandidate{
		ModelID: m.ID, ProviderID: provider.ID, ProviderModelName: providerModelName,
		InputPrice: in, OutputPrice: out, MaxOutput: 4096,
		SupportsStreaming: boolPtr(true), SupportsFunctionCalling: boolPtr(true),
		ManagementStatus: model.ModelCandidateStatusEnabled, SortOrder: order,
		VerificationStatus: model.ModelVerificationStatusPassed,
		CreatedAt:          now, UpdatedAt: now,
	}
	if err := db.Create(cand).Error; err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
}
