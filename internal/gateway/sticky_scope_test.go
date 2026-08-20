package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// kindReporter reports one fixed Kind, on one chosen error, which is how a test
// reaches a decision-table row that no shipped capability produces yet.
//
// Reporting on a single attempt rather than on every one is what makes the
// scope observable: a reporter that speaks up each iteration refills whatever
// the previous iteration cleared, and a verdict that outlives its candidate
// becomes indistinguishable from one that does not.
type kindReporter struct {
	name string
	kind fact.Kind
	// onlyOn is the 1-based error this reporter speaks up for, and nothing else.
	onlyOn int
	seen   *int
	// silent reports the verdict with no words of its own, which is what a
	// capability that fills in Kind and nothing else produces.
	silent bool
}

func (r kindReporter) Name() string { return r.name }

func (r kindReporter) ObserveUpstreamError(_ context.Context, _ struct{}, up fact.Upstream, sink fact.Sink) {
	*r.seen++
	if *r.seen != r.onlyOn {
		return
	}
	detail := r.name + " says so"
	if r.silent {
		detail = ""
	}
	sink.Report(fact.Fact{Kind: r.kind, Status: up.StatusCode, Detail: detail})
}

// TestTheTerminalQuotesWhateverTheTableCallsSticky is the check that the slot
// is driven by the table rather than by one capability's name.
//
// Before it was generalised, the kernel held a pair of fields named after
// content inspection and a branch that only ever filled them for that one
// verdict. A second capability wanting the same treatment had to add its own
// pair and its own branch. This uses a DIFFERENT verdict — one no shipped
// capability produces — and asks the terminal to quote it. If the executor ever
// goes back to recognising particular Kinds, this is what notices.
//
// It also pins that the table's own status is what reaches the caller: the row
// used here states a fixed 503, which is neither the upstream's status (500)
// nor the chain-exhausted default (502).
func TestTheTerminalQuotesWhateverTheTableCallsSticky(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	seen := 0
	// Reported on the LAST candidate, so the chain ends on this verdict.
	RegisterUpstreamErrorObserver(svc, kindReporter{
		name: "pricing", kind: fact.KindPricingUnavailableSkip, onlyOn: 2, seen: &seen,
	}, func(*Exchange) struct{} { return struct{}{} })
	apiKey := seedTwoCandidateModel(t, svc, db, upstream.URL)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the table's fixed 503 for the verdict the chain ended on; body = %s",
			w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("pricing says so")) {
		t.Errorf("the caller should be told what the reporter said, got %s", w.Body.String())
	}
}

// TestAVerdictDoesNotOutliveTheCandidateThatEarnedIt covers the clear that the
// key loop cannot reach.
//
// Two clears exist and they back each other up, which is what makes a test
// aimed at "the verdict does not outlive its attempt" prove nothing: delete
// either one and it still passes. So this one is aimed at the path only the
// candidate-level clear covers — a candidate dropped before any key is
// reached — and its sibling below is aimed at the key loop.
//
// The original framing is kept in the name of the sibling test, which is where
// the property actually gets exercised.
//
// Every verdict worth quoting describes ONE attempt: this candidate has no
// price, this candidate cannot serve the request, this key is throttled. A
// scope that let one of them survive into the next candidate would report the
// missing price after a later candidate genuinely fell over — sending the
// caller to audit a configuration while the provider is the thing that is
// down.
//
// The three existing refusal tests check this for content inspection. This
// checks it for a verdict reported by a different capability and carrying a
// different status, because the property belongs to the slot, not to whoever
// happens to fill it.
func TestAVerdictDoesNotOutliveTheCandidateThatEarnedIt(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	seen := 0
	// Reported on the FIRST candidate only. The second candidate then fails
	// with an ordinary outage, which is what actually ends the chain.
	RegisterUpstreamErrorObserver(svc, kindReporter{
		name: "pricing", kind: fact.KindPricingUnavailableSkip, onlyOn: 1, seen: &seen,
	}, func(*Exchange) struct{} { return struct{}{} })
	apiKey := seedTwoCandidateModel(t, svc, db, upstream.URL)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: the first candidate's verdict describes an attempt that "+
			"is over, and the chain ended on an outage. Reporting the earlier verdict sends the "+
			"caller to audit a configuration while the provider is down. body = %s",
			w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("pricing says so")) {
		t.Errorf("the first candidate's verdict outlived the attempt it described: %s", w.Body.String())
	}
}

// TestAVerdictWithNoWordsStillReadsAsAnAnswer covers the case a capability
// author produces by accident.
//
// Detail is optional on a fact: a capability can report a Kind, get the routing
// it asked for, and never write a sentence. That is a reasonable thing to do
// right up until its verdict is the one the chain ends on, at which point the
// terminal has a status to report and nothing to say about it. An empty body
// reads to the caller as a gateway that fell over without explanation, which is
// the opposite of what a stated verdict means.
//
// So the terminal supplies words when the reporter did not. This is the test
// that keeps that branch honest — without it the fallback is a line nobody has
// ever executed.
func TestAVerdictWithNoWordsStillReadsAsAnAnswer(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	seen := 0
	RegisterUpstreamErrorObserver(svc, kindReporter{
		name: "wordless", kind: fact.KindPricingUnavailableSkip, onlyOn: 2, seen: &seen, silent: true,
	}, func(*Exchange) struct{} { return struct{}{} })
	apiKey := seedTwoCandidateModel(t, svc, db, upstream.URL)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the table's 503 even though the reporter said nothing; body = %s",
			w.Code, w.Body.String())
	}
	if len(bytes.TrimSpace(w.Body.Bytes())) == 0 {
		t.Fatal("the caller got a status and an empty body: a stated verdict must reach them as words")
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("refused")) {
		t.Errorf("the fallback wording should say the request was refused, got %s", w.Body.String())
	}
}

// TestAVerdictDoesNotOutliveTheKeyThatEarnedIt is the lifetime failure that a
// candidate-shaped clear cannot catch.
//
// One candidate can make several attempts: a key gets throttled, the gateway
// rotates to the next key on the same provider. Those are different attempts
// against the same candidate, and a verdict describes an attempt. Clearing the
// slot only when a NEW CANDIDATE starts leaves the throttled key's verdict in
// place while its successor runs — and if the successor dies without a verdict
// of its own, the chain ends reporting a rate limit that one key hit, hiding
// the outage that actually ended the request.
//
// The upstream below throttles the first key and then falls over for the
// second, which is the shape that tells the two clears apart.
func TestAVerdictDoesNotOutliveTheKeyThatEarnedIt(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			// 429 is what makes this a KEY rotation rather than a failover: the
			// classifier sends it to the next key on the same provider. A 4xx
			// that terminates instead would end the request here and the second
			// attempt this test depends on would never run.
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	seen := 0
	RegisterUpstreamErrorObserver(svc, kindReporter{
		name: "throttle", kind: fact.KindUpstreamRateLimited, onlyOn: 1, seen: &seen,
	}, func(*Exchange) struct{} { return struct{}{} })

	// Two keys on ONE provider, so the second attempt is a key rotation rather
	// than a failover to a different candidate.
	p1 := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p1.ID, "sk-1", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p1.ID, "sk-2", "k2", 2, true)
	apiKey := seedModelOnProvider(t, db, p1)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if bytes.Contains(w.Body.Bytes(), []byte("throttle says so")) {
		t.Errorf("the first key's verdict outlived the attempt that earned it and was reported "+
			"as what ended the request: %s", w.Body.String())
	}
}

// A key whose ciphertext will not decrypt is unroutable, and is filtered
// out with the disabled, unverified, and destination-stale keys BEFORE
// rotation: it must never be dispatched, and — exactly like those other
// filtered keys — it does not silence the last real attempt's verdict.
// The request below ends on key 1's genuine rate limit, and that verdict
// is the honest outcome to report.
func TestUndecryptableKeyIsFilteredNotDispatched(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	seen := 0
	RegisterUpstreamErrorObserver(svc, kindReporter{
		name: "throttle", kind: fact.KindUpstreamRateLimited, onlyOn: 1, seen: &seen,
	}, func(*Exchange) struct{} { return struct{}{} })

	p1 := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p1.ID, "sk-1", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p1.ID, "sk-2", "k2", 2, true)
	// The second key's ciphertext is corrupted, so it is skipped without
	// ever being dispatched: decryption fails after the key is entered.
	if err := db.Model(&model.ProviderKey{}).Where("label = ?", "k2").
		Update("encrypted_key", "corrupt-ciphertext").Error; err != nil {
		t.Fatalf("corrupt the second key: %v", err)
	}
	apiKey := seedModelOnProvider(t, db, p1)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if calls != 1 {
		t.Fatalf("upstream called %d times, want 1: the undecryptable key must be filtered "+
			"before the walk, never dispatched", calls)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("throttle says so")) {
		t.Errorf("the sole routable key's rate-limit verdict should be the reported outcome; "+
			"got: %s", w.Body.String())
	}
}

// seedModelOnProvider wires one model to one provider, leaving the provider's
// keys to the caller — which is what the key-lifetime tests vary.
func seedModelOnProvider(t *testing.T, db *gorm.DB, p *model.Provider) *model.APIKey {
	t.Helper()
	now := time.Now().UTC()
	m := &model.Model{Name: "gpt-4o", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if err := db.Create(&model.ModelCandidate{
		ModelID: m.ID, ProviderID: p.ID, ProviderModelName: "c1-model",
		MaxOutput: 4096, SupportsStreaming: boolPtr(true), SupportsFunctionCalling: boolPtr(true),
		ManagementStatus: model.ModelCandidateStatusEnabled, SortOrder: 1,
		VerificationStatus: model.ModelVerificationStatusPassed,
		CreatedAt:          now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	return createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})
}
