package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// TestUsageDoesNotSurviveAFailedCandidate pins a billing defect that no test
// could see before, because nothing ever asserted on whose tokens were charged.
//
// A candidate can report usage while delivering nothing at all. Anthropic's
// first frame carries the input and cache token counts, and the encoders that
// translate it emit no client event for it, so the relay can come back holding
// real usage with not one byte on the wire — which means the attempt is still
// free to fail over. The usage it left behind on the exchange is not cleared by
// anything, so it becomes whatever happens next: charged under the 502 if the
// chain runs out, or charged at the NEXT candidate's price if that one succeeds
// without reporting usage of its own.
//
// Here the first candidate reports 1234 prompt tokens and then fails, and the
// second succeeds carrying no usage. Charging anything at all for this request
// means the first candidate's tokens were billed against the second.
// TestUsageDoesNotSurviveTheLastCandidate covers the end of a chain.
//
// Clearing at the START of the next send only helps when there IS a next send.
// A chain whose FINAL attempt reports usage and then fails has nothing after it
// to do the clearing, so the tokens sit on the exchange until settlement reads
// them — which is exactly the case that gets billed under the 502.
func TestUsageDoesNotSurviveTheLastCandidate(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	usageThenError := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(str string) {
			_, _ = w.Write([]byte(str))
			if flusher != nil {
				flusher.Flush()
			}
		}
		write("event: message_start\n")
		write(`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[],"usage":{"input_tokens":777}}}` + "\n\n")
		write("event: error\n")
		write(`data: {"type":"error","error":{"type":"overloaded_error","message":"nope"}}` + "\n\n")
	}))
	defer usageThenError.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "only-candidate", usageThenError.URL)
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

	c, _ := newCtx([]byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if billed := billedUsage(t, captured); billed != nil && billed.Prompt != 0 {
		t.Fatalf("the only candidate delivered nothing, yet %d prompt tokens were billed: %+v",
			billed.Prompt, *billed)
	}
}

func TestUsageDoesNotSurviveAFailedCandidate(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	// Reports usage in message_start, then refuses. An explicit error event is
	// what makes the decoder hand back both the cached usage AND an error; a
	// plain EOF would return neither, and a test written against that would
	// pass no matter what the code did.
	usageThenError := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte(s))
			if flusher != nil {
				flusher.Flush()
			}
		}
		write("event: message_start\n")
		write(`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[],"usage":{"input_tokens":1234,"cache_read_input_tokens":100}}}` + "\n\n")
		write("event: error\n")
		write(`data: {"type":"error","error":{"type":"overloaded_error","message":"upstream overloaded"}}` + "\n\n")
	}))
	defer usageThenError.Close()

	// The second candidate is simply down, so the chain runs out. Nothing after
	// this point reports usage, which means whatever gets billed came from the
	// candidate that already failed.
	deadUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer deadUpstream.Close()

	svc := newSvc(t, db)
	p1 := createAnthropicProvider(t, db, "claude-usage-then-error", usageThenError.URL)
	createProviderKey(t, db, svc.secrets, p1.ID, "sk-1", "k1", 1, true)
	p2 := createAnthropicProvider(t, db, "claude-success", deadUpstream.URL)
	createProviderKey(t, db, svc.secrets, p2.ID, "sk-2", "k1", 1, true)

	now := time.Now().UTC()
	m := &model.Model{Name: "gpt-4o", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	for i, p := range []*model.Provider{p1, p2} {
		if err := db.Create(&model.ModelCandidate{
			ModelID: m.ID, ProviderID: p.ID, ProviderModelName: "claude-3-5-sonnet-20241022",
			InputPrice: 1.0, OutputPrice: 2.0, MaxOutput: 4096,
			SupportsStreaming: boolPtr(true), SupportsFunctionCalling: boolPtr(true),
			ManagementStatus:   model.ModelCandidateStatusEnabled,
			SortOrder:          i + 1,
			VerificationStatus: model.ModelVerificationStatusPassed,
			CreatedAt:          now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatalf("seed candidate %d: %v", i, err)
		}
	}
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	c, w := newCtx([]byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 once the chain runs out; body = %s", w.Code, w.Body.String())
	}
	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if len(captured.attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (both candidates failed)", len(captured.attempts))
	}

	// Nothing was delivered, so there is nothing to charge for. Anything billed
	// here came from the first candidate, which served nobody.
	if billed := billedUsage(t, captured); billed != nil && billed.Prompt != 0 {
		t.Fatalf("a failed chain was billed %d prompt tokens left behind by an abandoned candidate: %+v",
			billed.Prompt, *billed)
	}
}

// TestUpstreamBodyDoesNotSurviveAFailedCandidate is the body counterpart to the
// usage test above.
//
// The audit row has one field for the upstream response body and a request may
// try several providers. Whatever ends up there is read as belonging to the
// attempt the row describes — nothing in the row says otherwise. So a chain
// whose final attempt never reached an upstream at all must not present an
// earlier provider's error response as if it were the one being settled: that
// is an invitation to diagnose a provider that was not even involved in how the
// request ended.
//
// The per-attempt records still show that the first provider answered 500, so
// what is given up here is the raw payload of a failure that is already on
// record — the cheaper of the two mistakes.
func TestUpstreamBodyDoesNotSurviveAFailedCandidate(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	answersWithBody := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"first-provider-marker"}`))
	}))
	defer answersWithBody.Close()

	// Closed straight away, so the second attempt cannot reach an upstream and
	// therefore produces no body of its own to overwrite the first one's.
	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachableURL := unreachable.URL
	unreachable.Close()

	svc := newSvc(t, db)
	p1 := createProvider(t, db, "answers-with-body", answersWithBody.URL)
	createProviderKey(t, db, svc.secrets, p1.ID, "sk-1", "k1", 1, true)
	p2 := createProvider(t, db, "unreachable", unreachableURL)
	createProviderKey(t, db, svc.secrets, p2.ID, "sk-2", "k1", 1, true)

	now := time.Now().UTC()
	m := &model.Model{Name: "gpt-4o", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	for i, p := range []*model.Provider{p1, p2} {
		if err := db.Create(&model.ModelCandidate{
			ModelID: m.ID, ProviderID: p.ID, ProviderModelName: "real-model",
			InputPrice: 0, OutputPrice: 0, MaxOutput: 4096,
			SupportsStreaming: boolPtr(true), SupportsFunctionCalling: boolPtr(true),
			ManagementStatus:   model.ModelCandidateStatusEnabled,
			SortOrder:          i + 1,
			VerificationStatus: model.ModelVerificationStatusPassed,
			CreatedAt:          now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatalf("seed candidate %d: %v", i, err)
		}
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
	if len(captured.attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (the 500, then the unreachable provider)", len(captured.attempts))
	}
	if bytes.Contains(captured.UpstreamResponseBody(), []byte("first-provider-marker")) {
		t.Errorf("the audit row kept the first provider's error body for a request that ended on a different provider: %s",
			captured.UpstreamResponseBody())
	}

	// What the first provider did is still on record, which is why dropping its
	// raw payload is affordable.
	if captured.attempts[0].StatusCode != http.StatusInternalServerError {
		t.Errorf("attempt 1 status = %d, want 500 still recorded", captured.attempts[0].StatusCode)
	}
}

// TestUpstreamBodyDoesNotSurviveAKeyRotation is the case the candidate-level
// test above cannot reach.
//
// One candidate rotates through its provider's keys, and every rotation is a
// real upstream request with its own response. Invalidating per CANDIDATE would
// look correct and still leave key A's leftovers standing while key B runs —
// the audit row would carry A's 401 body under an attempt that belongs to B.
// That is why the boundary is the send.
//
// Key A answers 401 with a distinctive body, which rotates to key B; key B gets
// no response at all, so it cannot overwrite what A left.
//
// The mutation that reddens this is moving the invalidation from per-send to
// per-candidate. Deleting the abandonment call does NOT redden it, and should
// not: abandonment drops usage, while the body here is replaced by the next
// send beginning.
func TestUpstreamBodyDoesNotSurviveAKeyRotation(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	var seen int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen++
		if seen == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"first-key-marker"}`))
			return
		}
		// Second key: hang up without answering, so this attempt produces no
		// body of its own.
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "two-keys", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "key-a", 1, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-b", "key-b", 2, true)

	now := time.Now().UTC()
	m := &model.Model{Name: "gpt-4o", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if err := db.Create(&model.ModelCandidate{
		ModelID: m.ID, ProviderID: p.ID, ProviderModelName: "real-model",
		InputPrice: 0, OutputPrice: 0, MaxOutput: 4096,
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
	if len(captured.attempts) < 2 {
		t.Fatalf("attempts = %d, want at least 2 (the 401, then the rotated key)", len(captured.attempts))
	}
	if bytes.Contains(captured.UpstreamResponseBody(), []byte("first-key-marker")) {
		t.Errorf("one key's 401 body survived a rotation and was stored under a later attempt: %s",
			captured.UpstreamResponseBody())
	}
}
