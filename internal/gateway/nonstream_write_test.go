package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// nonStreamCaller and nonStreamUpstream are one non-streaming exchange: what
// the caller asked for, and what the provider answered.
const (
	nonStreamCaller   = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	nonStreamUpstream = `{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
)

// TestANonStreamResponseTheCallerNeverReceivedIsNotRecordedAsDelivered pins
// what happens when the one write that hands a non-streaming answer to the
// caller fails.
//
// A non-stream response is written in a single call, and that call's error is
// the only signal that the caller got nothing — there is no sliding write
// deadline to catch it the way streaming has. Ignoring it settles the request
// as a delivered 2xx and bills for an answer nobody received, while blaming the
// provider, which answered correctly.
//
// The failure is injected rather than provoked with real network timing: a
// wall-clock reproduction would race the test process's own scheduling.
func TestANonStreamResponseTheCallerNeverReceivedIsNotRecordedAsDelivered(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", "http://upstream.invalid")
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var cand model.ModelCandidate
	if err := db.Where("model_id = ?", m.ID).First(&cand).Error; err != nil {
		t.Fatalf("load seeded candidate: %v", err)
	}

	gin.SetMode(gin.TestMode)
	fw := &failingPassthroughWriter{}
	c, _ := gin.CreateTestContext(fw)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	rc := &Exchange{requestID: "req-nonstream-write-fail", originalModel: "gpt-4o", apiKeyID: apiKey.ID}
	adm := admitFor(t, protocols.ProtocolOpenAI, "/v1/chat/completions", nonStreamCaller, Candidate{
		ProviderModelName: "gpt-4o-real", EgressProtocol: protocols.ProtocolOpenAI, Passthrough: true,
	})

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(nonStreamUpstream)),
		Header:     make(http.Header),
	}

	rc.attempt.BeginCandidate(&cand)
	rc.attempt.BindProvider(p)
	rc.attempt.BindKey(&model.ProviderKey{})
	result := svc.deliverAndSettle(c, rc, adm, resp, &UpstreamCall{Path: "/v1/chat/completions", ContentType: "application/json"}, time.Now())

	if result != attemptSuccess {
		t.Errorf("result = %v, want attemptSuccess (status+headers already committed, cannot fail over)", result)
	}
	if rc.statusCode != 499 {
		t.Errorf("rc.statusCode = %d, want 499 (a discarded write failure must not settle as a delivered 2xx success)", rc.statusCode)
	}
	if len(rc.ResponseBody()) != 0 {
		t.Errorf("ResponseBody must stay empty when the write to the client failed (never delivered), got %d bytes", len(rc.ResponseBody()))
	}
	if len(rc.UpstreamResponseBody()) == 0 {
		t.Error("UpstreamResponseBody should still be recorded (what the gateway actually consumed from upstream) even though delivery to the client failed")
	}
	if billed := requireBilled(t, rc); billed.Prompt != 5 {
		t.Errorf("billed usage = %+v, want the already-decoded usage preserved for billing despite the write failure", billed)
	}
	if len(rc.attempts) != 1 {
		t.Fatalf("Attempts = %+v, want exactly one entry", rc.attempts)
	}
	if rc.attempts[0].Outcome != AttemptConnError {
		t.Errorf("attempt outcome = %q, want %q (client write timeout must not be classified as an upstream server fault)",
			rc.attempts[0].Outcome, AttemptConnError)
	}
	// The reason code, not the prose after it: the code is what anything
	// querying these rows groups by.
	if !strings.HasPrefix(rc.attempts[0].FailReason, "client_write_timeout") {
		t.Errorf("fail_reason = %q, want it to start with 'client_write_timeout'", rc.attempts[0].FailReason)
	}
}

// TestANonStreamResponseTheCallerReceivedIsRecordedAsDelivered is the positive
// control: when the write succeeds, the response IS recorded as delivered
// (rc.ResponseBody() set) and the request finalizes with the upstream's own
// status code, not 499.
func TestANonStreamResponseTheCallerReceivedIsRecordedAsDelivered(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", "http://upstream.invalid")
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var cand model.ModelCandidate
	if err := db.Where("model_id = ?", m.ID).First(&cand).Error; err != nil {
		t.Fatalf("load seeded candidate: %v", err)
	}

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	rc := &Exchange{requestID: "req-nonstream-write-ok", originalModel: "gpt-4o", apiKeyID: apiKey.ID}
	adm := admitFor(t, protocols.ProtocolOpenAI, "/v1/chat/completions", nonStreamCaller, Candidate{
		ProviderModelName: "gpt-4o-real", EgressProtocol: protocols.ProtocolOpenAI, Passthrough: true,
	})

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(nonStreamUpstream)),
		Header:     make(http.Header),
	}

	rc.attempt.BeginCandidate(&cand)
	rc.attempt.BindProvider(p)
	rc.attempt.BindKey(&model.ProviderKey{})
	result := svc.deliverAndSettle(c, rc, adm, resp, &UpstreamCall{Path: "/v1/chat/completions", ContentType: "application/json"}, time.Now())

	if result != attemptSuccess {
		t.Errorf("result = %v, want attemptSuccess", result)
	}
	if rc.statusCode != http.StatusOK {
		t.Errorf("rc.statusCode = %d, want 200", rc.statusCode)
	}
	if len(rc.ResponseBody()) == 0 {
		t.Error("ResponseBody should be recorded as delivered once the write succeeds")
	}
	if len(rc.attempts) != 1 || rc.attempts[0].Outcome != AttemptSuccess {
		t.Errorf("Attempts = %+v, want exactly one AttemptSuccess entry", rc.attempts)
	}
}
